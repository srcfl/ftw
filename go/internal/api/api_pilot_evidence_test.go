package api

import (
	"encoding/json"
	"fmt"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"testing"
	"testing/synctest"
	"time"
)

func TestPilotFreshnessRequiresSpecificConfirmation(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	ms := now.UnixMilli()
	base := feedbackReading{PilotA: watts(13), PilotObservedAt: now.Add(-24 * time.Hour).Format(time.RFC3339Nano), PilotConfirmedAtMs: &ms}
	if age, ok := pilotReadingAge(base, now); !ok || age != 0 {
		t.Fatal("unchanged old source value with current confirmation rejected")
	}
	// A confirmation past the window is valid but too old to explain a draw.
	old := now.Add(-91 * time.Second).UnixMilli()
	stale := base
	stale.PilotConfirmedAtMs = &old
	if age, ok := pilotReadingAge(stale, now); !ok || age <= pilotFreshFor {
		t.Fatalf("stale confirmation: age %v ok %v", age, ok)
	}
	for _, tc := range []struct {
		name   string
		change func(*feedbackReading)
	}{
		{"missing confirmation", func(d *feedbackReading) { d.PilotConfirmedAtMs = nil }},
		{"zero confirmation", func(d *feedbackReading) { v := int64(0); d.PilotConfirmedAtMs = &v }},
		{"missing source", func(d *feedbackReading) { d.PilotObservedAt = "" }},
		{"future confirmation", func(d *feedbackReading) { v := now.Add(time.Second).UnixMilli(); d.PilotConfirmedAtMs = &v }},
		{"future source", func(d *feedbackReading) { d.PilotObservedAt = now.Add(time.Second).Format(time.RFC3339Nano) }},
		{"negative pilot", func(d *feedbackReading) { d.PilotA = watts(-1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			tc.change(&d)
			if _, ok := pilotReadingAge(d, now); ok {
				t.Fatal("invalid pilot accepted")
			}
		})
	}
}

func TestPilotAwareDriverFeedback(t *testing.T) {
	// The charger holds FTW's 15 A order; limit is the driver's current the
	// car may take, and age the pilot confirmation's age at the verdict.
	const at13 = `,"current_limit_a":13`
	const balanced = at13 + `,"current_limited_by":"load_balancer"`
	for _, tc := range []struct {
		name                     string
		draw, pilot              float64
		limit                    string
		device                   bool // the charger's own 13 A limit
		age                      time.Duration
		result, reason, severity string
	}{
		{"fresh lower pilot, internal cause unknown", 8850, 13, at13, false, 5 * time.Second, "limited", "charger_pilot_limit", "info"},
		{"confirmed static limit", 8850, 13, at13, true, 5 * time.Second, "limited", "device_limit", "warning"},
		{"static limit cannot hide low draw", 4000, 13, at13, true, 5 * time.Second, "not_following", "power_below_target", "warning"},
		{"static limit without a current limit", 8850, 13, "", true, 5 * time.Second, "limited", "device_limit", "warning"},
		{"pilot at the end of its window", 8850, 13, at13, false, pilotFreshFor, "limited", "charger_pilot_limit", "info"},
		{"stale pilot cannot explain power", 8850, 13, at13, false, pilotFreshFor + time.Second, "limited", "offered_current_lower", "warning"},
		{"stale pilot keeps the load balancing", 8850, 13, balanced, false, pilotFreshFor + time.Second, "limited", "load_balancer_limit", "info"},
		{"future confirmation cannot explain power", 8850, 13, at13, false, -time.Minute, "not_following", "power_below_target", "warning"},
		{"zero delivery stays visible", 0, 13, at13, false, 5 * time.Second, "not_following", "no_power_response", "warning"},
		{"zero pilot cannot explain ongoing draw", 8850, 0, at13, false, 5 * time.Second, "not_following", "power_below_target", "warning"},
		{"delivery below effective pilot stays visible", 4000, 13, at13, false, 5 * time.Second, "not_following", "power_below_target", "warning"},
		{"pilot above the order", 8850, 20, at13, false, 5 * time.Second, "limited", "offered_current_lower", "warning"},
		{"pilot above the limit", 8850, 14.2, at13, false, 5 * time.Second, "limited", "offered_current_lower", "warning"},
		{"pilot within 0.5 A of the order", 9900, 14.8, `,"current_limit_a":14.4`, false, 5 * time.Second, "limited", "offered_current_lower", "warning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEVTrace(tc.draw)
				for range 6 * 60 / 5 {
					// tick waits five seconds before the verdict.
					confirmed := time.Now().Add(5*time.Second - tc.age)
					e.extra = fmt.Sprintf(`,"max_a":15,"dynamic_limit_a":15%s,"pilot_a":%g,"pilot_observed_at":%q,"pilot_confirmed_at_ms":%d`, tc.limit, tc.pilot, time.Now().Add(-time.Hour).Format(time.RFC3339Nano), confirmed.UnixMilli())
					if tc.device {
						e.extra += `,"device_limit_a":13,"device_limit_age_s":0`
					}
					e.tick(10350, "plan")
				}
				f := e.verdict(t)
				if f.Status != tc.result || f.Reason != tc.reason || f.Severity != tc.severity {
					t.Fatalf("got %s/%s/%s want %s/%s/%s", f.Status, f.Reason, f.Severity, tc.result, tc.reason, tc.severity)
				}
				// A pilot past its window is unknown, not shown.
				if tc.age > pilotFreshFor && (f.PilotA != nil || f.PilotConfirmedAtMs != 0) {
					t.Fatalf("stale pilot shown: %+v", f)
				}
				if tc.reason == "charger_pilot_limit" {
					if f.Severity != "info" || f.PilotA == nil || *f.PilotA != 13 || f.DynamicLimitA == nil || *f.DynamicLimitA != 15 {
						t.Fatalf("pilot evidence: %+v", f)
					}
					raw, err := json.Marshal(f)
					if err != nil {
						t.Fatal(err)
					}
					var out map[string]any
					if err = json.Unmarshal(raw, &out); err != nil {
						t.Fatal(err)
					}
					if out["pilot_a"] != float64(13) || out["dynamic_limit_a"] != float64(15) {
						t.Fatal(string(raw))
					}
				}
			})
		})
	}
}

func TestPilotLimitNeverHidesCommandFailureOrPendingCommand(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct{ result, reason string }{{"failed", "command_failed"}, {"unconfirmed", "command_unconfirmed"}, {"pending", "waiting_response"}} {
		t.Run(tc.result, func(t *testing.T) {
			f := ControlFeedback{Kind: "ev", SentW: watts(10350), RequestedW: watts(10350), RequestedA: watts(15), OfferedA: watts(15), PilotA: watts(13), LimitA: watts(13), ActualW: watts(8850), ObservedAtMs: now.UnixMilli()}
			cmd := telemetry.CommandEvidence{Result: tc.result, Since: now.Add(-3 * time.Minute)}
			classifyControlFeedback(&f, cmd, true, true, "", nil, now)
			if f.Reason != tc.reason {
				t.Fatalf("got %s want %s", f.Reason, tc.reason)
			}
		})
	}
}
