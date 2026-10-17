package control

import (
	"encoding/json"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"math"
	"testing"
	"time"
)

func TestEVFuseConfirmedSteadyPowerSurvivesRateChange(t *testing.T) {
	for _, tc := range []struct {
		name               string
		before, grid, want float64
		missing, stopped   bool
	}{
		{"known charging then rate decrease", 6183, 7019, 5238, false, false},
		{"new household load still limits", 6183, 10000, 2257, false, false},
		{"new start earns no guessed credit", 0, 7019, 481, false, false},
		{"missing observation revokes old credit", 6183, 7019, 481, true, false},
		{"observed stop before restart retains zero credit", 6183, 7019, 481, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := telemetry.NewStore()
			emit := func(driver string, kind telemetry.DerType, w float64, at time.Time, confirmed bool) {
				d, _ := json.Marshal(map[string]any{"control_power_observed_at": at.Format(time.RFC3339Nano), "control_power_confirmed": confirmed})
				store.Update(driver, kind, w, nil, d)
				store.RecordDriverSuccess(driver)
			}
			emit("car", telemetry.DerEV, tc.before, time.Now().Add(-time.Minute), true)
			if tc.missing {
				store.Update("car", telemetry.DerEV, tc.before, nil, []byte(`{"control_power_available":false}`))
			}
			emit("meter", telemetry.DerMeter, tc.grid, time.Now(), false)
			if tc.stopped {
				emit("car", telemetry.DerEV, 0, time.Now(), true)
			}
			emit("car", telemetry.DerEV, 4757, time.Now(), true)
			st := NewState(0, 0, "meter")
			st.PeakImportCeilingW = 7500
			got, _ := EVFuseBudget(store, st, []loadpoint.State{{ID: "car", DriverName: "car", PluggedIn: true}}, []loadpoint.Config{{ID: "car", DriverName: "car", PhaseMode: "3p"}}, 17250)
			if math.Abs(got-tc.want) > .01 {
				t.Fatalf("cap=%v want=%v", got, tc.want)
			}
		})
	}
}
