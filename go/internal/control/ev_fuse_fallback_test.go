package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestEVFuseFallbackKeepsLatestEVProof(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		ev, grid, want            float64
		oldSite, evAfter, offline bool
		phase                     float64
	}{
		{name: "01:55 preceding EV without common bracket", ev: 6820, grid: 6587, want: 7733},
		{name: "real household increase", ev: 6820, grid: 12000, want: 2320},
		{name: "later EV cannot fund earlier site", ev: 6820, grid: 6587, want: 913, evAfter: true},
		{name: "missing EV earns no credit", ev: 6820, grid: 6587, want: 913, offline: true},
		{name: "01:58 stopped EV does not retain older high grid", ev: 0, grid: -99.39, want: 7599.39, oldSite: true},
		{name: "phase limit remains authoritative", ev: 6820, grid: 6587, want: 6130, phase: 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			store := telemetry.NewStore()
			emit := func(driver string, kind telemetry.DerType, ago time.Duration, w float64) {
				d := map[string]any{"control_power_observed_at": now.Add(-ago).Format(time.RFC3339Nano)}
				if kind == telemetry.DerMeter {
					a := tc.phase
					if a == 0 {
						a = 10
					}
					d["l1_a"], d["l2_a"], d["l3_a"] = a, a, a
				}
				b, _ := json.Marshal(d)
				store.Update(driver, kind, w, nil, b)
				store.RecordDriverSuccess(driver)
			}
			emit("battery", telemetry.DerBattery, 5*time.Second, -1000)
			if tc.oldSite {
				emit("meter", telemetry.DerMeter, 4*time.Second, 6640)
			}
			emit("battery", telemetry.DerBattery, 3*time.Second, -1000)
			age := 2 * time.Second
			if tc.evAfter {
				age = 500 * time.Millisecond
			}
			emit("car", telemetry.DerEV, age, tc.ev)
			emit("meter", telemetry.DerMeter, time.Second, tc.grid)
			if tc.offline {
				store.Remove("car")
			}
			st := NewState(0, 0, "meter")
			st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases = 25, 230, 3
			st.PeakImportCeilingW = 7500
			st.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 10000, MaxDischargeW: 10000}}
			got, _ := EVFuseBudget(store, st, []loadpoint.State{{ID: "car", DriverName: "car", PluggedIn: true}}, []loadpoint.Config{{ID: "car", DriverName: "car", PhaseMode: "3p"}}, 17250)
			if math.Abs(got-tc.want) > .01 {
				t.Fatalf("cap=%v want=%v", got, tc.want)
			}
		})
	}
}
