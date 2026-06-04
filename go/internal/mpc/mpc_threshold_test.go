package mpc

import (
	"math"
	"testing"
)

// Two-slot arbitrage: import at 50, export at 70 → gross spread ~14.6 öre/kWh
// after round-trip. Starting at the SoC floor means any slot-1 discharge must
// be funded by a slot-0 charge — i.e. a real arbitrage cycle, not dumping
// initial SoC. This is the fixture for the threshold tests below.
func arbitrageCycleSlots() []Slot {
	return []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 50, SpotOre: 50, Confidence: 1},
		{StartMs: 3600000, LenMin: 60, PriceOre: 70, SpotOre: 70, Confidence: 1},
	}
}

func arbitrageCycleParams() Params {
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 10 // at the floor: discharge must be funded by a charge
	p.TerminalSoCPrice = 0
	return p
}

// With no threshold the planner takes the marginal cycle: charge slot 0,
// discharge slot 1.
func TestArbitrageBaselineTakesMarginalCycle(t *testing.T) {
	plan := Optimize(arbitrageCycleSlots(), arbitrageCycleParams())
	if plan.Actions[0].BatteryW <= 100 {
		t.Fatalf("baseline should charge in slot 0 to arbitrage, got %.1f W", plan.Actions[0].BatteryW)
	}
}

// A threshold above the spread suppresses the marginal cycle — the battery
// holds instead of cycling for a gain below the operator's floor.
func TestArbitrageThresholdSuppressesMarginalCycle(t *testing.T) {
	p := arbitrageCycleParams()
	p.MinArbitrageSpreadOreKwh = 25 // > ~14.6 öre/kWh spread
	plan := Optimize(arbitrageCycleSlots(), p)
	if plan.Actions[0].BatteryW > 100 {
		t.Errorf("threshold 25 öre/kWh should suppress the cycle, but battery charged %.1f W in slot 0",
			plan.Actions[0].BatteryW)
	}
	if math.Abs(plan.Actions[1].BatteryW) > 100 {
		t.Errorf("threshold should suppress slot-1 discharge, got %.1f W", plan.Actions[1].BatteryW)
	}
}

// The threshold is a deadband, not a hard stop: a spread well above it still
// cycles.
func TestArbitrageThresholdDoesNotBlockWideSpread(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 50, SpotOre: 50, Confidence: 1},
		{StartMs: 3600000, LenMin: 60, PriceOre: 200, SpotOre: 200, Confidence: 1},
	}
	p := arbitrageCycleParams()
	p.MinArbitrageSpreadOreKwh = 25 // spread ~144 öre/kWh >> 25
	plan := Optimize(slots, p)
	if plan.Actions[0].BatteryW <= 100 {
		t.Errorf("wide-spread cycle should survive a 25 öre/kWh threshold, got charge %.1f W",
			plan.Actions[0].BatteryW)
	}
}

// Cover-load discharge (self-consumption-style, retail+VAT spread) is never
// suppressed by an öre-level threshold, even in an arbitrage mode.
func TestThresholdDoesNotSuppressCoverLoadDischarge(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 250, SpotOre: 200, LoadW: 2000, Confidence: 1},
	}
	p := baseParams(ModePassiveArbitrage)
	p.InitialSoCPct = 80
	p.MinArbitrageSpreadOreKwh = 25
	plan := Optimize(slots, p)
	if plan.Actions[0].BatteryW >= -100 {
		t.Errorf("battery should discharge to cover load despite the threshold, got %.1f W",
			plan.Actions[0].BatteryW)
	}
}

// Savings isolation: when the dispatch is the same with and without the
// threshold (wide spread → both cycle fully), the reported cost must be
// identical — the threshold biases the DP decision, never the accounting.
func TestThresholdDoesNotAffectReportedCost(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 50, SpotOre: 50, Confidence: 1},
		{StartMs: 3600000, LenMin: 60, PriceOre: 200, SpotOre: 200, Confidence: 1},
	}
	base := arbitrageCycleParams()
	plan0 := Optimize(slots, base)
	pS := base
	pS.MinArbitrageSpreadOreKwh = 25
	planS := Optimize(slots, pS)
	for i := range plan0.Actions {
		if math.Abs(plan0.Actions[i].BatteryW-planS.Actions[i].BatteryW) > 1 {
			t.Fatalf("precondition: wide-spread dispatch should match with/without threshold (slot %d: %.1f vs %.1f)",
				i, plan0.Actions[i].BatteryW, planS.Actions[i].BatteryW)
		}
	}
	if math.Abs(plan0.TotalCostOre-planS.TotalCostOre) > 0.01 {
		t.Errorf("reported TotalCostOre must not change with the threshold: %.4f vs %.4f",
			plan0.TotalCostOre, planS.TotalCostOre)
	}
}

// Mode gating: the threshold applies only in the arbitrage modes. A
// self_consumption cover-load discharge is identical with and without it.
func TestThresholdGatedOutOfSelfConsumption(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 250, SpotOre: 200, LoadW: 2000, Confidence: 1},
	}
	base := baseParams(ModeSelfConsumption)
	base.InitialSoCPct = 80
	plan0 := Optimize(slots, base)
	pS := base
	pS.MinArbitrageSpreadOreKwh = 25
	planS := Optimize(slots, pS)
	if math.Abs(plan0.Actions[0].BatteryW-planS.Actions[0].BatteryW) > 1 {
		t.Errorf("self_consumption must be unaffected by the arbitrage threshold: %.1f vs %.1f",
			plan0.Actions[0].BatteryW, planS.Actions[0].BatteryW)
	}
}
