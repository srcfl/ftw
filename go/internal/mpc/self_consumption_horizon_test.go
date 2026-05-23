package mpc

import "testing"

// TestSelfConsumptionAbsorbsCheapPVOver48hHorizon is the regression guard
// for the 2026-05-23 production bug where a 1.7 kW exporting slot saw
// the DP pick idle instead of absorbing the surplus into the battery.
//
// Scenario mirrors the production observation:
//   - SoC 65 % (room to 90 %)
//   - Slot 0 has cheap midday spot (2 öre, retail 152 öre) with strong PV
//   - Evening peak (slots 13-21) at retail 260 öre forecasts heavy import
//   - Day 2 is cloudy — no chance to defer charging
//
// With ActionLevels=21 (900 W step) the DP picked idle because the only
// legal charge action (+900 W) didn't visibly improve V over 192 slots
// vs. 0. Production now runs ActionLevels=81 (225 W step) — this test
// pins that value so a future regression that drops it back to 21 or
// 41 (or any coarser grid that re-introduces the bug) trips the test.
func TestSelfConsumptionAbsorbsCheapPVOver48hHorizon(t *testing.T) {
	slots := make([]Slot, 192)
	for i := range slots {
		var price, spot, pv, load float64
		d := i % 96
		switch {
		case d < 6:
			price, spot, pv, load = 152, 2, -2300, 693
		case d < 13:
			price, spot, pv, load = 170, 15, -1500, 800
		case d < 22:
			price, spot, pv, load = 260, 90, -200, 1800
		case d < 30:
			price, spot, pv, load = 170, 12, 0, 500
		case d < 40:
			price, spot, pv, load = 150, 5, 0, 500
		case d < 55:
			price, spot, pv, load = 130, 3, 0, 500
		case d < 70:
			price, spot, pv, load = 200, 50, -800, 1100
		case d < 85:
			price, spot, pv, load = 145, 1, -3500, 693
		default:
			price, spot, pv, load = 150, 2, -2900, 693
		}
		if i >= 96 { // Day 2: cloudy — no PV.
			pv = 0
		}
		slots[i] = Slot{
			LenMin:     15,
			PriceOre:   price,
			SpotOre:    spot,
			PVW:        pv,
			LoadW:      load,
			Confidence: 1.0,
		}
	}

	p := Params{
		Mode:                ModeSelfConsumption,
		CapacityWh:          20000,
		InitialSoCPct:       65,
		SoCMinPct:           10,
		SoCMaxPct:           90,
		SoCLevels:           41,
		MaxChargeW:          9000,
		MaxDischargeW:       9000,
		ActionLevels:        81, // matches main.go's buildMPC default
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    163.99,
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) == 0 {
		t.Fatal("plan empty")
	}
	a := plan.Actions[0]
	if a.BatteryW <= 0 {
		t.Fatalf("DP must charge battery in slot 0 (cheap PV + expensive evening peak ahead), got batt=%v reason=%q",
			a.BatteryW, a.Reason)
	}
}
