package control

import (
	"encoding/json"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"math"
	"testing"
	"time"
)

func TestEVFuseAlignment(t *testing.T) {
	for _, tc := range []struct {
		name                string
		restart, grid, want float64
	}{
		{"restart does not rewrite old balance", 7527, 7154, 7230},
		{"new household load still stops", 7527, 12000, 3027},
		{"latest low EV still stops", 286, 7154, 632},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			store := telemetry.NewStore()
			emit := func(driver string, kind telemetry.DerType, ago time.Duration, w float64) {
				d := map[string]any{"control_power_observed_at": now.Add(-ago).Format(time.RFC3339Nano)}
				if kind == telemetry.DerMeter {
					d["l1_a"], d["l2_a"], d["l3_a"] = 10.0, 11.2, 10.0
				}
				b, _ := json.Marshal(d)
				store.Update(driver, kind, w, nil, b)
				store.RecordDriverSuccess(driver)
			}
			emit("battery", telemetry.DerBattery, 7*time.Second, -1000)
			emit("car", telemetry.DerEV, 7*time.Second, 6836)
			emit("meter", telemetry.DerMeter, 6*time.Second, 7106)
			emit("car", telemetry.DerEV, 5*time.Second, 6836)
			emit("car", telemetry.DerEV, 4*time.Second, 286)
			emit("battery", telemetry.DerBattery, 3*time.Second, -1000)
			emit("car", telemetry.DerEV, 2*time.Second, tc.restart)
			emit("meter", telemetry.DerMeter, time.Second, tc.grid)
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

func TestEVFuseLatestFallbackUsesSameBatteryBaseline(t *testing.T) {
	now := time.Now()
	store := telemetry.NewStore()
	for _, p := range []struct {
		driver string
		kind   telemetry.DerType
		ago    time.Duration
		w      float64
	}{
		{"battery", telemetry.DerBattery, 2 * time.Millisecond, -990.37234241847},
		{"meter", telemetry.DerMeter, time.Millisecond, 6.832},
	} {
		d, _ := json.Marshal(map[string]any{"control_power_observed_at": now.Add(-p.ago).Format(time.RFC3339Nano)})
		store.Update(p.driver, p.kind, p.w, nil, d)
		store.RecordDriverSuccess(p.driver)
	}
	st := NewState(0, 0, "meter")
	st.PeakImportCeilingW = 7500
	st.DriverLimits = map[string]PowerLimits{"battery": {MaxChargeW: 10000, MaxDischargeW: 10000}}
	got, _ := EVFuseBudget(store, st, nil, nil, 17250)
	if math.Abs(got-7493.168) > .01 {
		t.Fatalf("cap=%v want7493.168", got)
	}
	// A pending zero order can remove the observed discharge. Even with
	// the preceding fresh measurement, that loss must still be reserved.
	pendingEVOrder(store, 0, now.Add(-3*time.Millisecond), "accepted")
	got, _ = EVFuseBudget(store, st, nil, nil, 17250)
	if math.Abs(got-(7493.168-990.37234241847)) > .01 {
		t.Fatalf("pending discharge release lost: cap=%v", got)
	}
}
