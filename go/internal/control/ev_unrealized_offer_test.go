package control

import (
	"math"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A stable current offer is not proof that its power is already inside the
// site meter. These incident replays require the battery to leave room for
// the unrealized part of a stable EV offer.
// This is a battery-allocation regression, not an Easee hardware simulator.
func TestEVBatteryReservesUnrealizedStableOffer(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		gridW, batteryW, demandW, actualW float64
		phaseA                            []float64
	}{
		{"15A offered but only 4.155kW drawn", 13843, 9383, 10350, 4155, []float64{19.65, 20.52, 19.46}},
		{"6A offered but no draw", 10347, 8261, 4140, 0, []float64{18.84, 13.29, 12.09}},
		{"realized offer preserves battery allocation", 13843, 9383, 4140, 4140, []float64{19.65, 20.52, 19.46}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, state := evSplitFixture(t, tc.gridW, tc.phaseA, tc.demandW, "3p")
			state.SiteFuseSafetyA = .5
			sent(state, "ev", tc.demandW, tc.demandW)
			sent(state, "first", tc.batteryW, tc.batteryW)
			store.Update("car", telemetry.DerEV, tc.actualW, nil, nil)
			store.RecordDriverSuccess("car")
			got := targetW(clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: tc.batteryW}}, store, state, 17250), "first")
			// Conditional balanced three-phase projection at configured 230 V,
			// holding all other loads constant. Release battery charge before EV rise.
			worst := math.Max(tc.phaseA[0], math.Max(tc.phaseA[1], tc.phaseA[2]))
			projectedA := worst + (tc.demandW-tc.actualW)/690
			requiredCutW := math.Max(0, (projectedA-24.5)*690)
			ceilingW := math.Max(0, tc.batteryW-requiredCutW)
			if got > ceilingW+1 {
				t.Fatalf("stable EV offer %.0f W, actual %.0f W: battery allocation %.0f W exceeds EV-priority ceiling %.0f W; projected busiest phase %.2f A", tc.demandW, tc.actualW, got, ceilingW, projectedA)
			}
			if tc.actualW == tc.demandW && math.Abs(got-tc.batteryW) > 1 {
				t.Fatalf("realized EV offer unnecessarily reduced battery: got %.0f W want %.0f W", got, tc.batteryW)
			}
		})
	}
}
