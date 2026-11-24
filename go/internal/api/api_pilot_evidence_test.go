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
		{"zero pilot under load balancing that holds the start", 0, 0, `,"current_limit_a":0,"current_limited_by":"load_balancer"`, false, 5 * time.Second, "waiting", "load_balancer_wait", "info"},
		{"a pilot the car could start on is not a hold", 0, 6, `,"current_limit_a":0,"current_limited_by":"load_balancer"`, false, 5 * time.Second, "not_following", "no_power_response", "warning"},
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

// Field report srcfl/ftw#2056, 21 Nov 2026, v0.165.1-beta.1, Easee Cloud
// with a test driver built on easee_cloud's pilot evidence: FTW ordered
// 13 A on three phases (8.97 kW), the charger read back 13 A, Easee's load
// balancing signalled 8 A and the car took 5.44 kW. Pilot confirmations
// came a minute apart. Between them the verdict turned to Not following
// for 15 s although the 8 A pilot was still fresh. That driver stops
// reporting its limit for up to 30 s after its order reading moves, until
// the pilot answers the order again. A fresh pilot still names the charger
// as the cause, but not its load balancing, which only the driver can.
func TestFreshPilotExplainsTheDrawWhileTheDriverWithholdsItsLimit(t *testing.T) {
	const balanced = `,"current_limit_a":8,"current_limited_by":"load_balancer"`
	for _, tc := range []struct {
		name                     string
		gapW                     float64
		status, reason, severity string
	}{
		{"steady draw at the pilot", 5440, "limited", "charger_pilot_limit", "info"},
		{"car far below the pilot", 3000, "not_following", "power_below_target", "warning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				source := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
				e := newEVTrace(5440)
				read := func(confirmed time.Time, limit string) {
					e.extra = fmt.Sprintf(`,"max_a":13,"dynamic_limit_a":13,"pilot_a":8,"pilot_observed_at":%q,"pilot_confirmed_at_ms":%d%s`, source, confirmed.UnixMilli(), limit)
				}
				check := func(when, status, reason, severity string) {
					t.Helper()
					f := e.verdict(t)
					if f.Status != status || f.Reason != reason || f.Severity != severity {
						t.Fatalf("%s: got %s/%s/%s want %s/%s/%s", when, f.Status, f.Reason, f.Severity, status, reason, severity)
					}
				}
				// Five minutes under the balancer's 8 A, confirmed every minute.
				for i := range 5 * 60 / 5 {
					if i%12 == 0 {
						read(time.Now(), balanced)
					}
					e.tick(8970, "plan")
				}
				first := time.Now()
				read(first, balanced)
				e.tick(8970, "plan")
				check("first confirmation", "limited", "load_balancer_limit", "info")
				for range 5 {
					e.tick(8970, "plan")
				}
				// 30 s after the confirmation the driver reports no limit for
				// 15 s. The pilot it confirmed stays fresh.
				read(first, "")
				if tc.gapW != e.drawW {
					e.drawW, e.changedAt = tc.gapW, time.Now()
				}
				for range 3 {
					e.tick(8970, "plan")
					check(fmt.Sprintf("gap at %v", time.Since(first)), tc.status, tc.reason, tc.severity)
				}
				if f := e.verdict(t); f.PilotA == nil || *f.PilotA != 8 || f.LimitA != nil || f.LimitedBy != "" {
					t.Fatalf("gap evidence: pilot %v, limit %v by %q", f.PilotA, f.LimitA, f.LimitedBy)
				}
				if tc.status != "limited" {
					return
				}
				read(first, balanced)
				e.tick(8970, "plan")
				check("limit back", "limited", "load_balancer_limit", "info")
				for range 2 {
					e.tick(8970, "plan")
				}
				read(time.Now(), balanced)
				e.tick(8970, "plan")
				check(fmt.Sprintf("second confirmation at %v", time.Since(first)), "limited", "load_balancer_limit", "info")
				// A second confirmation while the driver still withholds the
				// limit keeps the charger as the cause.
				read(time.Now(), "")
				e.tick(8970, "plan")
				check("second confirmation without the limit", "limited", "charger_pilot_limit", "info")
			})
		})
	}
}
