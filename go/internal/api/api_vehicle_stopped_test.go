package api

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Keep this fixture independent of the pilot/load-balancer PR's evTrace changes.
// stopped is the driver's request_active=false (Easee reason 50 or op mode 4).
type vehicleStopTrace struct {
	*evTrace
	stopped bool
	maxA    float64
}

func (e *vehicleStopTrace) tick(w float64, reason string) {
	c := e.tel.BeginCommand("easee", []byte(fmt.Sprintf(`{"action":"ev_set_current","power_w":%g}`, w)), time.Now())
	e.tel.CompleteCommand(c, "accepted")
	e.mgr.SetCommanded("garage", w, reason)
	extra := `,"request_active":true`
	if e.stopped {
		extra = `,"request_active":false,"op_mode":4`
	}
	if e.maxA > 0 {
		extra += fmt.Sprintf(`,"max_a":%g`, e.maxA)
	}
	e.tel.Update("easee", telemetry.DerEV, e.drawW, nil, []byte(fmt.Sprintf(`{"connected":true,"control_power_observed_at":%q,"control_power_confirmed":true,"power_max_age_s":180%s}`, e.changedAt.Format(time.RFC3339Nano), extra)))
	e.mgr.Observe("garage", true, e.drawW, 0, !e.stopped)
	time.Sleep(5 * time.Second)
}

// Field report #1976: an Easee charger said Completed at the car's own 80 %
// limit while Core still asked for 10.35 kW. A stop that lasts is explained
// without a fault or a full battery; until it lasts, Core keeps waiting.
func TestVehicleStopExplainsZeroDrawButNotAFullBattery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := &vehicleStopTrace{evTrace: newEVTrace(8200), maxA: 12}
		for range 30 {
			e.tick(8280, "plan")
		}
		e.drawW, e.changedAt, e.stopped, e.maxA = 0, time.Now(), true, 15
		stop := time.Now()
		for range 12 * 60 / 5 {
			e.tick(10350, "plan")
			f, since := e.verdict(t), time.Since(stop)
			want := "vehicle_declined"
			if since < loadpoint.SessionCompletionTimeout {
				want = "waiting_response"
			}
			if f.Reason != want || f.Status == "not_following" || f.Severity != "info" {
				t.Fatalf("at %v want %s: %+v", since, want, f)
			}
			states := e.srv.ControlStates(time.Now())
			if len(states) != 1 || states[0].Status == "not_following" {
				t.Fatalf("notification verdict: %+v", states)
			}
		}
		// A new active vehicle request at zero power remains a genuine failed start.
		e.stopped = false
		for range 5 * 60 / 5 {
			e.tick(10350, "plan")
		}
		if f := e.verdict(t); f.Status != "not_following" || f.Reason != "no_power_response" {
			t.Fatalf("failed start hidden: %+v", f)
		}
	})
}

// Easee can report reason 50 for a moment during the session handshake. A
// blip during a failed start must not explain the missing power.
func TestVehicleStopBlipKeepsFailedStartVerdict(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := &vehicleStopTrace{evTrace: newEVTrace(0), maxA: 15}
		start := time.Now()
		for range 60 / 5 {
			e.tick(10350, "plan")
		}
		e.stopped = true
		for range 3 {
			e.tick(10350, "plan")
			if f := e.verdict(t); f.Reason != "waiting_response" {
				t.Fatalf("handshake blip at %v: %+v", time.Since(start), f)
			}
		}
		e.stopped = false
		for range 90 / 5 {
			e.tick(10350, "plan")
		}
		if f := e.verdict(t); f.Reason != "no_power_response" || f.Status != "not_following" {
			t.Fatalf("failed start at %v: %+v", time.Since(start), f)
		}
		e.stopped = true
		for range 60 / 5 {
			e.tick(10350, "plan")
			if f := e.verdict(t); f.Reason != "no_power_response" || f.Status != "not_following" {
				t.Fatalf("blip hid a failed start at %v: %+v", time.Since(start), f)
			}
		}
	})
}

// Core's own 0 W stop for a car that stopped asking is Core in control.
func TestCoreStopForANotRequestingCarIsFollowing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := &vehicleStopTrace{evTrace: newEVTrace(0), stopped: true}
		for range 60 / 5 {
			e.tick(0, "vehicle_not_requesting")
		}
		if f := e.verdict(t); f.Reason != "vehicle_not_requesting" || f.Status != "following" || f.Evidence != "measured" {
			t.Fatalf("Core's own stop: %+v", f)
		}
	})
}

func TestVehicleStopNeverOverridesStaleFailedOrContradictoryEvidence(t *testing.T) {
	now := time.Now()
	no := false
	yes := true
	for _, tc := range []struct {
		name     string
		request  *bool
		declined bool
		fresh    bool
		result   string
		actual   float64
		reason   string
	}{
		{"lasting stop", &no, true, true, "accepted", 0, "vehicle_declined"},
		{"brief stop", &no, false, true, "accepted", 0, "no_power_response"},
		{"missing request", nil, true, true, "accepted", 0, "no_power_response"},
		{"active request", &yes, true, true, "accepted", 0, "no_power_response"},
		{"stale completed", &no, true, false, "accepted", 0, "telemetry_stale"},
		{"command failed", &no, true, true, "failed", 0, "command_failed"},
		{"unconfirmed command", &no, true, true, "unconfirmed", 0, "command_unconfirmed"},
		{"unexpected draw", &no, true, true, "accepted", 12000, "power_above_target"},
		{"lower held offer", &no, true, true, "accepted", 0, "offered_current_lower"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ControlFeedback{Kind: "ev", VehicleRequestActive: tc.request, VehicleDeclined: tc.declined, SentW: watts(10350), RequestedW: watts(10350), RequestedA: watts(15), OfferedA: watts(15), ActualW: watts(tc.actual), ObservedAtMs: now.UnixMilli()}
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
