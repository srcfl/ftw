package mpc

import (
	"context"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// The bundled worker plans the field night of 15 Nov 2026 (fuseNightSlots)
// without the battery making the car's import room. Core accepts the plan,
// so the site keeps the worker's plan instead of falling back to Core DP.
func TestNativeBatteryThatMayNotCoverTheCarMakesItNoRoom(t *testing.T) {
	o := nativeWorker(t, 1500*time.Millisecond)
	defer o.Close()
	slots := fuseNightSlots()
	for _, mode := range []Mode{ModeArbitrage, ModePassiveArbitrage} {
		p := fuseNightParams(mode, true)
		plan, err := o.Optimize(context.Background(), slots, p)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		for i, a := range plan.Actions {
			if a.LoadpointW > 0 && a.BatteryW < -50 && slots[i].LoadW+a.LoadpointW > fuseNightImportW+loadpoint.SitePowerEpsW {
				t.Errorf("%s slot %d: battery %.0f W makes the room for a %.0f W car", mode, i, a.BatteryW, a.LoadpointW)
			}
		}
		t.Logf("%s: %s, car %.0f%% by slot 7", mode, plan.Solver.Backend, 100*plan.Actions[7].LoadpointSoC)
	}
}
