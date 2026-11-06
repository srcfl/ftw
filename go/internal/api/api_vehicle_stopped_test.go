package api

import (
	"fmt"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"testing"
	"testing/synctest"
	"time"
)

// Keep this fixture independent of the pilot/load-balancer PR's evTrace changes.
type vehicleStopTrace struct {
	*evTrace
	extra string
}

func (e *vehicleStopTrace) tick(w float64, reason string) {
	c := e.tel.BeginCommand("easee", []byte(fmt.Sprintf(`{"action":"ev_set_current","power_w":%g}`, w)), time.Now())
	e.tel.CompleteCommand(c, "accepted")
	e.mgr.SetCommanded("garage", w, reason)
	e.tel.Update("easee", telemetry.DerEV, e.drawW, nil, []byte(fmt.Sprintf(`{"connected":true,"control_power_observed_at":%q,"control_power_confirmed":true,"power_max_age_s":180%s}`, e.changedAt.Format(time.RFC3339Nano), e.extra)))
	e.mgr.Observe("garage", true, e.drawW, 0, true)
	time.Sleep(5 * time.Second)
}

func TestVehicleStopExplainsZeroDrawButNotAFullBattery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := &vehicleStopTrace{evTrace: newEVTrace(8200)}
		e.extra = `,"request_active":true,"max_a":12`
		for range 30 {
			e.tick(8280, "plan")
		}
		e.drawW = 0
		e.changedAt = time.Now()
		e.extra = `,"request_active":false,"op_mode":4,"max_a":15`
		for range 12 * 60 / 5 {
			e.tick(10350, "plan")
			f := e.verdict(t)
			if f.Reason != "vehicle_not_requesting" || f.Status == "not_following" || f.Severity != "info" {
				t.Fatalf("false fault: %+v", f)
			}
			states := e.srv.ControlStates(time.Now())
			if len(states) != 1 || states[0].Status == "not_following" {
				t.Fatalf("notification verdict: %+v", states)
			}
		}
		// A new active vehicle request at zero power remains a genuine failed start.
		e.extra = `,"request_active":true,"max_a":15`
		for range 5 * 60 / 5 {
			e.tick(10350, "plan")
		}
		if f := e.verdict(t); f.Status != "not_following" || f.Reason != "no_power_response" {
			t.Fatalf("failed start hidden: %+v", f)
		}
	})
}
func TestVehicleStopNeverOverridesStaleFailedOrContradictoryEvidence(t *testing.T) {
	now := time.Now()
	no := false
	yes := true
	for _, tc := range []struct {
		name    string
		request *bool
		fresh   bool
		result  string
		actual  float64
		reason  string
	}{
		{"explicit stop", &no, true, "accepted", 0, "vehicle_not_requesting"},
		{"missing request", nil, true, "accepted", 0, "no_power_response"},
		{"active request", &yes, true, "accepted", 0, "no_power_response"},
		{"stale completed", &no, false, "accepted", 0, "telemetry_stale"},
		{"command failed", &no, true, "failed", 0, "command_failed"},
		{"unconfirmed command", &no, true, "unconfirmed", 0, "command_unconfirmed"},
		{"unexpected draw", &no, true, "accepted", 12000, "power_above_target"},
		{"lower held offer", &no, true, "accepted", 0, "offered_current_lower"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ControlFeedback{Kind: "ev", VehicleRequestActive: tc.request, SentW: watts(10350), RequestedW: watts(10350), RequestedA: watts(15), OfferedA: watts(15), ActualW: watts(tc.actual), ObservedAtMs: now.UnixMilli()}
			if tc.name == "lower held offer" {
				f.OfferedA = watts(0)
			}
			cmd := telemetry.CommandEvidence{Result: tc.result, Since: now.Add(-10 * time.Minute), PowerMismatchSince: now.Add(-5 * time.Minute), LastObservation: now}
			classifyControlFeedback(&f, cmd, true, tc.fresh, "plan", &yes, now)
			if f.Reason != tc.reason {
				t.Fatalf("reason %s want %s", f.Reason, tc.reason)
			}
			if !tc.fresh && f.VehicleRequestActive != nil {
				t.Fatal("stale request exposed")
			}
		})
	}
}
