package control

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Site rig s1 and s15 (28 Oct 2026, c9b52c36): Charge now from idle on a
// 25 A or 16 A fuse, 3 A of house per phase covered by the battery, so every phase
// reads 0 A. The charger answers in about a second. Core logs an offer when
// the next control tick reads the loadpoint's command, 2 s after it went
// out, and that tick's meter reading came in 7 ms before it. The reading
// already shows the car at 15.9 A, but it is older than the logged offer.
// The landing then read as a raise not shown yet: 15.9 + 16 A passed
// 1.2 x In, and Core cut 11,040 W to 5,520 W with 9 A of room on every
// phase ("EV hard limit: site meter over limit ... worst_phase_a=15.9
// rise_on_its_way_a=[16 16 16] cap_w=5520"). The car then crept back 1 A
// at a time and held 9,660 W on fuse_limit. Since #1923's ab027102 the
// rise still to come reads the meter's own rise since the raise went out.
func TestEVFuseRaiseShownBeforeItsOfferIsLogged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fuseA   float64
		phases  int
		offerW  float64
		landedA [3]float64
	}{
		{"s1_25A_fuse", 25, 3, 11040, [3]float64{15.9, 15.9, 15.9}},
		{"s1_16A_fuse", 16, 3, 10350, [3]float64{15, 15, 15}},
		{"s15_one_phase_charger_on_L1", 25, 1, 3680, [3]float64{16, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newCutRig(t, 0, 0)
			r.phases, r.state.SiteFuseAmps = tc.phases, tc.fuseA
			// Each reading comes in 7 ms before the tick that reads it.
			read := func(phaseA [3]float64) {
				t.Helper()
				data, _ := json.Marshal(map[string]any{"l1_a": phaseA[0], "l2_a": phaseA[1], "l3_a": phaseA[2]})
				r.now = r.now.Add(2 * time.Second)
				r.store.Update("meter", telemetry.DerMeter, (phaseA[0]+phaseA[1]+phaseA[2])*230, nil, data)
				r.store.Get("meter", telemetry.DerMeter).UpdatedAt = r.now.Add(-7 * time.Millisecond)
				_, _, points, configs := evMeterBudgetFixture(t, 0, nil, r.offerW, "3p")
				points[0].CommandedPhases = r.phases
				if capW, _ := EVFuseBudget(r.store, r.state, points, configs, tc.fuseA*690); capW < r.offerW {
					t.Fatalf("offer %.0f W cut to %.0f W with the car at %v A on a %.0f A fuse", r.offerW, capW, phaseA, tc.fuseA)
				}
			}
			for range 15 {
				read([3]float64{})
			}
			r.offerW = tc.offerW // sent at the last tick, logged at this one
			for range 60 {
				read(tc.landedA)
			}
		})
	}
}
