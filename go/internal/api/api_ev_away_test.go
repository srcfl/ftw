package api

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Field report, 10 Nov 2026, a tester's Audi e-tron on an Easee: with the
// car out on the road the app said "Charger · Lost control". The car left,
// Core ordered the charger 0 W, and the idle charger stopped reporting. A
// charger with no car, which FTW asks for nothing, has nothing to control:
// its quiet is not lost control. With a car on it, it still is.
func TestAQuietChargerWithNoCarIsNotLostControl(t *testing.T) {
	for _, tc := range []struct {
		name    string
		carAway bool
		orderW  float64
		reason  string
		status  string
	}{
		{name: "car away, nothing ordered", carAway: true, orderW: 0, reason: "not_connected", status: "waiting"},
		{name: "car on it, charging ordered", carAway: false, orderW: 7000, reason: "readings_lost", status: "no_contact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEVTrace(7000)
				for range 12 {
					e.tick(7000, "plan")
				}
				order := func(w float64, reason string) {
					c := e.tel.BeginCommand("easee", []byte(fmt.Sprintf(`{"action":"ev_set_current","power_w":%g}`, w)), time.Now())
					e.tel.CompleteCommand(c, "accepted")
					e.mgr.SetCommanded("garage", w, reason)
				}
				if tc.carAway {
					// The car leaves: the charger says so for a minute, and
					// Core orders nothing.
					for range 12 {
						order(0, "no_plan_budget")
						e.tel.Update("easee", telemetry.DerEV, 0, nil, []byte(`{"connected":false}`))
						e.mgr.Observe("garage", false, 0, 0, false)
						time.Sleep(5 * time.Second)
					}
				}
				// Then the charger goes quiet for five minutes.
				for range 60 {
					if tc.carAway {
						order(0, "no_plan_budget")
					} else {
						order(tc.orderW, "plan")
					}
					time.Sleep(5 * time.Second)
				}
				f := e.verdict(t)
				if f.Reason != tc.reason || f.Status != tc.status {
					t.Fatalf("got %s/%s/%s, want %s/%s", f.Status, f.Reason, f.Severity, tc.status, tc.reason)
				}
				if tc.carAway && (f.Severity != "info" || f.VerificationLost) {
					t.Fatalf("a charger with nothing to control alarms: %+v", f)
				}
			})
		})
	}
}
