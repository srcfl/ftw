package control

import (
	"encoding/json"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"math"
	"testing"
	"time"
)

func newEVBatteryTransitionTestState(t *testing.T, kind string) (*telemetry.Store, *State, map[string]float64, float64) {
	t.Helper()
	store := telemetry.NewStore()
	soc := 0.6
	low := 0.04
	data, _ := json.Marshal(map[string]float64{"l1_a": 22.3, "l2_a": 12.3, "l3_a": 12.3, "l1_w": 22.3 * 230, "l2_w": 12.3 * 230, "l3_w": 12.3 * 230})
	store.Update("review_meter", telemetry.DerMeter, 10787, nil, data)
	store.RecordDriverSuccess("review_meter")
	a, b := -2000.0, 0.0
	if kind == "battery_at_soc_floor" {
		a, b = -1000, -2000
	}
	store.Update("review_a", telemetry.DerBattery, a, &soc, nil)
	store.RecordDriverSuccess("review_a")
	store.Update("review_ev", telemetry.DerEV, 10350, nil, nil)
	store.RecordDriverSuccess("review_ev")
	caps := map[string]float64{"review_a": 10000}
	if kind == "battery_at_soc_floor" {
		store.Update("review_a", telemetry.DerBattery, a, &low, nil)
		store.Update("review_b", telemetry.DerBattery, b, &soc, nil)
		store.RecordDriverSuccess("review_b")
		caps["review_b"] = 10000
	}
	now := time.Now().Add(time.Millisecond)
	st := NewState(0, 0, "review_meter")
	st.SiteFuseAmps = 25
	st.SiteFuseVoltage = 230
	st.SiteFusePhases = 3
	st.SiteFuseSafetyA = 1
	st.Mode = ModeIdle
	st.SlewEnabled = false
	st.MinDispatchIntervalS = 0
	st.DriverLimits = map[string]PowerLimits{"review_a": {MaxChargeW: 5000, MaxDischargeW: 5000}, "review_b": {MaxChargeW: 5000, MaxDischargeW: 5000}}
	st.clock = func() time.Time { return now.Add(-time.Hour) }
	st.SetEVCommanded([]loadpoint.State{{ID: "review_lp", DriverName: "review_ev", PluggedIn: true, CommandedKnown: true, CommandedW: 10350, CommandedPhases: 3}})
	st.clock = func() time.Time { return now }
	st.SetEVFuseDemands([]loadpoint.FuseDemand{{DriverName: "review_ev", PowerW: 10350, PhaseMode: "3p"}})
	st.evOffers = commandLog{}.add(now.Add(-time.Hour), 10350)
	st.batteryCommands = map[string]commandLog{"review_a": commandLog{}.add(now.Add(-time.Hour), a)}
	pending := 0.0
	if kind == "pending_battery_without_target" {
		st.batteryCommands["review_b"] = commandLog{}.add(now.Add(-time.Hour), 0).add(now.Add(-2*time.Second), 1000)
		pending = 1000
	}
	if kind == "battery_at_soc_floor" {
		st.batteryCommands["review_b"] = commandLog{}.add(now.Add(-time.Hour), b)
	}
	return store, st, caps, pending
}
func TestEVBatteryPendingImportAcrossTargets(t *testing.T) {
	for _, kind := range []string{"single_control", "pending_battery_without_target", "battery_at_soc_floor"} {
		for _, path := range []string{"allocation", "idle", "manual"} {
			t.Run(kind+"/"+path, func(t *testing.T) {
				store, st, caps, pending := newEVBatteryTransitionTestState(t, kind)
				targets := []DispatchTarget{{Driver: "review_a", TargetW: 0}}
				initial := -2000.0
				if kind == "battery_at_soc_floor" {
					targets = append(targets, DispatchTarget{Driver: "review_b", TargetW: 0})
					initial = -3000
				}
				var out []DispatchTarget
				if path == "allocation" {
					out = clampEVBatteryHeadroom(targets, store, st, 17250)
				} else {
					if path == "manual" {
						st.SetBatteryManualHold(BatteryManualHold{PowerW: 0, ExpiresAt: st.now().Add(time.Hour)})
					}
					out = ComputeDispatch(store, st, caps, 17250)
				}
				total := 0.0
				for _, v := range out {
					total += v.TargetW
				}
				projected := 22.3 + (pending+total-initial)/690
				t.Logf("review result target_w=%.3f projected_a=%.6f", total, projected)
				if len(out) != len(targets) {
					t.Fatalf("missing battery targets")
				}
				if projected > 24.001 || math.Abs(projected-24) > 0.01 {
					t.Fatalf("unsafe or excessive relief: projected phase %.6f A; target %.3f W", projected, total)
				}
				if kind == "battery_at_soc_floor" {
					for _, v := range out {
						if v.Driver == "review_a" && v.TargetW < 0 {
							t.Fatalf("battery below floor was retained")
						}
					}
				}
			})
		}
	}
}
