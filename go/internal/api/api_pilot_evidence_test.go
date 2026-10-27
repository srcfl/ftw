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
	if !pilotReadingFresh(base, now) {
		t.Fatal("unchanged old source value with current confirmation rejected")
	}
	for _, tc := range []struct {
		name   string
		change func(*feedbackReading)
	}{
		{"missing confirmation", func(d *feedbackReading) { d.PilotConfirmedAtMs = nil }},
		{"missing source", func(d *feedbackReading) { d.PilotObservedAt = "" }},
		{"stale confirmation", func(d *feedbackReading) { v := now.Add(-91 * time.Second).UnixMilli(); d.PilotConfirmedAtMs = &v }},
		{"future confirmation", func(d *feedbackReading) { v := now.Add(time.Second).UnixMilli(); d.PilotConfirmedAtMs = &v }},
		{"future source", func(d *feedbackReading) { d.PilotObservedAt = now.Add(time.Second).Format(time.RFC3339Nano) }},
		{"negative pilot", func(d *feedbackReading) { d.PilotA = watts(-1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := base
			tc.change(&d)
			if pilotReadingFresh(d, now) {
				t.Fatal("invalid pilot accepted")
			}
		})
	}
}

func TestPilotAwareDriverFeedback(t *testing.T) {
	for _, tc := range []struct {
		name           string
		draw           float64
		age            time.Duration
		result, reason string
	}{
		{"fresh lower pilot, internal cause unknown", 8850, 0, "limited", "charger_pilot_limit"},
		{"confirmed static limit", 8850, 0, "limited", "device_limit"},
		{"static limit cannot hide low draw", 4000, 0, "not_following", "power_below_target"},
		{"stale pilot cannot explain power", 8850, 91 * time.Second, "not_following", "power_below_target"},
		{"future confirmation cannot explain power", 8850, -time.Minute, "not_following", "power_below_target"},
		{"zero delivery stays visible", 0, 0, "not_following", "no_power_response"},
		{"zero pilot cannot explain ongoing draw", 8850, 0, "not_following", "power_below_target"},
		{"delivery below effective pilot stays visible", 4000, 0, "not_following", "power_below_target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEVTrace(tc.draw)
				for range 6 * 60 / 5 {
					pilot := 13.0
					if tc.name == "zero pilot cannot explain ongoing draw" {
						pilot = 0
					}
					e.extra = fmt.Sprintf(`,"max_a":15,"dynamic_limit_a":15,"current_limit_a":13,"pilot_a":%g,"pilot_observed_at":%q,"pilot_confirmed_at_ms":%d`, pilot, time.Now().Add(-time.Hour).Format(time.RFC3339Nano), time.Now().Add(-tc.age).UnixMilli())
					if tc.name == "confirmed static limit" || tc.name == "static limit cannot hide low draw" {
						e.extra += `,"device_limit_a":13,"device_limit_age_s":0`
					}
					e.tick(10350, "plan")
				}
				f := e.verdict(t)
				if f.Status != tc.result || f.Reason != tc.reason {
					t.Fatalf("got %s/%s want %s/%s", f.Status, f.Reason, tc.result, tc.reason)
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
