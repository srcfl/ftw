package mpc

import (
	"context"
	"testing"
	"time"
)

// belowFloorNight is the shape of #2043: a 20 kWh battery 60 Wh under its
// floor and a car due by morning, with a cheap night before a dear day.
func belowFloorNight() ([]Slot, Params) {
	slots := make([]Slot, 96)
	for i := range slots {
		load, price := 1500.0, 260.0
		switch {
		case i < 16:
			load, price = 2000, 300
		case i < 40:
			load, price = 800, 115
		}
		slots[i] = Slot{StartMs: 1_794_942_000_000 + int64(i)*15*60*1000, LenMin: 15, PriceOre: price, SpotOre: price * .8, LoadW: load}
	}
	p := Params{
		Mode: ModePassiveArbitrage, SoCLevels: 41, ActionLevels: 41,
		CapacityWh: 20000, SoCMin: .1, SoCMax: .95, InitialSoC: .097,
		MaxChargeW: 10000, MaxDischargeW: 10000, ChargeEfficiency: .95, DischargeEfficiency: .95,
		TerminalSoCPrice: 200, MinArbitrageSpreadOreKwh: 60,
		Loadpoint: &LoadpointSpec{
			ID: "car", CapacityWh: 84000, Levels: 85, SoCMin: 0, SoCMax: 1,
			InitialSoC: .34, PluggedIn: true, TargetSoC: .8, TargetSlotIdx: 48,
			MinChargeW: 4800, MaxChargeW: 11100, ChargeEfficiency: .9,
			AllowedStepsW: []float64{0, 4800, 5500, 6200, 6900, 7600, 8300, 9000, 9700, 10400, 11100},
			NoBatteryToEV: true,
			Charging:      ChargingPeriods{MinChargeSeconds: 300, StartCostOre: 5},
		},
	}
	return slots, p
}

// On a field request Energyplan 0.7.0 kept such a battery idle for the whole
// horizon (#2043). Core now starts it on the floor (#2045), and the worker
// must plan the night charge from either start. This shape passes on 0.7.0
// too under Core's time budget; Energyplan's own suite holds the case that
// failed.
func TestNativeBatteryUnderItsFloorBesideACarChargesAtNight(t *testing.T) {
	external := nativeWorker(t, 1500*time.Millisecond)
	defer external.Close()
	o := &EnergyplanOptimizer{ExternalOptimizer: external}
	nightKWh := func(plan Plan) float64 {
		var wh float64
		for i, a := range plan.Actions {
			if i >= 16 && i < 40 && a.BatteryW > 0 {
				wh += a.BatteryW * float64(plan.Actions[i].SlotLenMin) / 60
			}
		}
		return wh / 1000
	}
	for _, onFloor := range []bool{true, false} {
		slots, p := belowFloorNight()
		if onFloor {
			if _, ok := clampParamsIntoOperatingBand(&p); !ok {
				t.Fatal("clamp refused")
			}
		}
		plan, err := o.Optimize(context.Background(), slots, p)
		if err != nil {
			t.Fatalf("on floor %v: %v", onFloor, err)
		}
		if err := ValidatePlan(slots, p, &plan); err != nil {
			t.Fatalf("on floor %v: %v", onFloor, err)
		}
		night := nightKWh(plan)
		t.Logf("on floor %v: %s, %.1f kWh charged at night", onFloor, plan.Solver.Backend, night)
		if night < 10 {
			t.Errorf("on floor %v: %.1f kWh charged at night, want a night charge", onFloor, night)
		}
	}
}
