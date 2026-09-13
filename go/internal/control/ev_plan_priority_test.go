package control

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func TestScheduledEVKeepsBudgetBeforeBatteryCharge(t *testing.T) {
	for _, draw := range []float64{0, 4000, 8000} {
		t.Run(fmt.Sprint(draw), func(t *testing.T) {
			now := time.Now()
			dir := SlotDirective{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), BatteryEnergyWh: 1250, LoadpointEnergyWh: map[string]float64{"garage": 2000}, Strategy: "arbitrage"}
			store := seedStore(200+draw, []struct {
				name          string
				currentW, soc float64
			}{{"ferroamp", 0, .5}})
			st := newStateWithEnergyDispatch(dir, "ferroamp")
			st.EVChargingW = draw
			st.BatteryCoversEV = true
			targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11000)
			if len(targets) != 1 {
				t.Fatalf("targets %v", targets)
			}
			if !st.FuseSaturated || math.Abs(st.FuseEVMaxW-8000) > 1 {
				t.Fatalf("EV budget lost while drawing %.0f: cap %.0f saturated %v", draw, st.FuseEVMaxW, st.FuseSaturated)
			}
			if targets[0].TargetW < 0 || targets[0].TargetW > 2800.1 {
				t.Fatalf("optional battery charge exceeds remaining 2800 W: %v", targets)
			}
			if 200+st.FuseEVMaxW+targets[0].TargetW > 11000.1 {
				t.Fatal("planned EV and battery exceed fuse")
			}
		})
	}
}

func TestScheduledEVRejectsExpiredDemand(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{SlotStart: now.Add(-30 * time.Minute), SlotEnd: now.Add(-15 * time.Minute), LoadpointEnergyWh: map[string]float64{"garage": 2000}}
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	if got := scheduledEVPowerW(st); got != 0 {
		t.Fatalf("stale demand reserved %.0f W", got)
	}
}
