package mpc

import "testing"

// TestStrictSelfConsumptionDischargesWhenSoCHealthy mirrors Fredrik's
// 2026-04-19 08:19 incident: self_consumption mode, ~50% SoC of a
// small battery, grid would import 2 kW at 166 öre. Before the
// strict-SC bias the DP happily idled and imported, treating
// "preserve SoC for later" as a better bet than "use battery now".
// After the bias, any action that reduces import should beat idle.
func TestStrictSelfConsumptionDischargesWhenSoCHealthy(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 166, SpotOre: 63,
			LoadW: 3480, PVW: -1390, Confidence: 1.0},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 165, SpotOre: 63,
			LoadW: 3480, PVW: -1985, Confidence: 1.0},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoC = 0.5
	p.TerminalSoCPrice = 107 // roughly selfConsumptionTerminalPrice output

	plan := Optimize(slots, p)
	if len(plan.Actions) == 0 {
		t.Fatal("no actions — plan empty")
	}
	first := plan.Actions[0]
	if first.BatteryW >= -100 {
		t.Errorf("strict SC expected battery discharge > 100 W, got BatteryW=%.1f "+
			"(GridW=%.1f, SoC=%.1f%%, reason=%q)",
			first.BatteryW, first.GridW, first.SoC, first.Reason)
	}
	// GridW should be lower than baseline 2090 W import — discharging
	// pulls it down.
	if first.GridW > 2000 {
		t.Errorf("GridW still near baseline (%.1f); battery not helping", first.GridW)
	}
}

// TestStrictSelfConsumptionDoesNotStarveEVDeadline is Codex P1 on
// PR #122: the original strict-SC bias multiplied the cost of every
// importing action by 3, including EV-charging import. At slot
// prices above ~4/3 × meanPrice the DP would prefer missing the
// EV's deadline (shortfall penalty = 4×meanPrice per kWh) over
// paying the tripled import cost. The fix scopes the bias to the
// HOUSE portion of the grid import only — EV import keeps its
// un-biased cost so the deadline-penalty comparison stays honest.
//
// Scenario: 3-slot horizon with a peak-price deadline slot.
// meanPrice = (400+100+100)/3 ≈ 200. Slot 0 price 400 = 2× mean,
// comfortably above the 4/3 breakpoint where the pre-fix bias
// would have chosen to miss the deadline. MaxDischargeW is
// limited to 1000 W so the battery physically CANNOT cover both
// house load and the EV's 2.5 kW draw — the DP has to import
// something, and the question is whether that import is priced
// under the SC bias (pre-fix → starves EV) or left at its plain
// cost (post-fix → DP charges to meet deadline).
func TestStrictSelfConsumptionDoesNotStarveEVDeadline(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 400, SpotOre: 200,
			LoadW: 500, PVW: -200, Confidence: 1.0},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 100, SpotOre: 40,
			LoadW: 500, PVW: -200, Confidence: 1.0},
		{StartMs: 2 * 3600 * 1000, LenMin: 60, PriceOre: 100, SpotOre: 40,
			LoadW: 500, PVW: -200, Confidence: 1.0},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoC = 0.8 // well above floor+20
	p.TerminalSoCPrice = 100
	p.MaxDischargeW = 1000 // battery can't cover load + EV alone
	p.MaxChargeW = 1000
	p.Loadpoint = &LoadpointSpec{
		ID:               "garage",
		CapacityWh:       10000,
		Levels:           11,
		SoCMax:           1.0,
		InitialSoC: 0.2,
		PluggedIn:        true,
		TargetSoC: 0.4, // need 2 kWh
		TargetSlotIdx:    0,  // deadline = slot 0
		MaxChargeW:       2500,
		AllowedStepsW:    []float64{0, 2500},
		ChargeEfficiency: 0.95,
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) == 0 {
		t.Fatal("empty plan")
	}
	evW := plan.Actions[0].LoadpointW
	if evW < 1000 {
		t.Errorf("EV deadline starved: slot 0 LoadpointW=%.0f W (expected ≈ 2500). "+
			"Before the Codex fix the strict-SC import bias applied to EV energy, "+
			"making missed-deadline cost cheaper than charging at peak price.", evW)
	}
}

// TestUpdateCapacityPropagatesToDefaults covers Codex P1 on PR #121
// — the hot-reload path now pushes new totals into the running
// service so the next replan uses them instead of the startup
// snapshot. The field is mutex-protected because a reactive replan
// could race the reload.
func TestUpdateCapacityPropagatesToDefaults(t *testing.T) {
	s := &Service{Defaults: Params{CapacityWh: 99800, MaxChargeW: 11040, MaxDischargeW: 11040}}
	s.UpdateCapacity(24800, 8000, 8000)
	if s.Defaults.CapacityWh != 24800 {
		t.Errorf("CapacityWh = %f, want 24800", s.Defaults.CapacityWh)
	}
	if s.Defaults.MaxChargeW != 8000 {
		t.Errorf("MaxChargeW = %f, want 8000", s.Defaults.MaxChargeW)
	}
	if s.Defaults.MaxDischargeW != 8000 {
		t.Errorf("MaxDischargeW = %f, want 8000", s.Defaults.MaxDischargeW)
	}
	// Nil receiver must no-op, not panic.
	var nilSvc *Service
	nilSvc.UpdateCapacity(1, 2, 3)
}

// TestStrictSelfConsumptionRespectsFloor — replaces the old
// BacksOffNearSoCFloor test after #157. The strict-SC bias now
// extends all the way down to SoCMin: the operator's configured
// floor IS the reserve, no implicit +20 % buffer on top. The DP
// must still never drive SoC below SoCMin because the SoC-
// transition feasibility check rejects it.
//
// We start at SoC = 22 % (12 % above the 10 % floor) so the action
// grid has room to express a real discharge step; at SoC < ~15 %
// the 500 W action step would push SoC below the floor and be
// filtered out by the feasibility check — idle would then be the
// only legal action regardless of bias, which is correct behaviour
// but makes the test assert nothing useful about the bias.
func TestStrictSelfConsumptionRespectsFloor(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 166, LoadW: 3480, PVW: -1390, Confidence: 1.0},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoC = 0.22 // comfortably above min (10) but well below the old min+20 buffer
	p.TerminalSoCPrice = 200
	plan := Optimize(slots, p)
	// Floor is hard: SoC must not dip below SoCMin.
	if plan.Actions[0].SoC < p.SoCMin-1e-6 {
		t.Errorf("SoC went below configured floor: %.2f < %.2f",
			plan.Actions[0].SoC, p.SoCMin)
	}
	// With the +20 buffer removed the bias is active here — DP
	// should discharge rather than idle, since the slot price is
	// well above zero and the battery has room above the hard floor.
	if plan.Actions[0].BatteryW >= -50 {
		t.Errorf("expected discharge under strict-SC bias at SoC=%.0f%% (floor=%.0f%%); "+
			"got BatteryW=%.1f", p.InitialSoC, p.SoCMin, plan.Actions[0].BatteryW)
	}
}

// TestStrictSelfConsumptionDischargesBelowOldBufferAtHighPrice is the
// regression for #157: field report 2026-04-20 of the planner idling
// at Tue 02:15 with SoC ≈ 27 %, importing 5.3 kW at 206 öre, because
// the strict-SC bias used to be gated by `soc > SoCMin+20`. Below
// that threshold the DP fell back to pure cost minimisation and
// preferred reserving the last 17 % of capacity for the morning peak
// — arbitrage behaviour the operator did not ask for.
func TestStrictSelfConsumptionDischargesBelowOldBufferAtHighPrice(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 206, SpotOre: 80,
			LoadW: 5300, PVW: 0, Confidence: 1.0},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoC = 0.28     // just below the old floor+20 threshold (10+20)
	p.TerminalSoCPrice = 100 // modest terminal; not strong enough to dominate
	plan := Optimize(slots, p)
	if len(plan.Actions) == 0 {
		t.Fatal("empty plan")
	}
	first := plan.Actions[0]
	if first.BatteryW >= -100 {
		t.Errorf("expected discharge below the old floor+20 buffer at price=206 öre; "+
			"got BatteryW=%.1f (SoC=%.1f%%, reason=%q)",
			first.BatteryW, first.SoC, first.Reason)
	}
	// Floor is still respected.
	if first.SoC < p.SoCMin-1e-6 {
		t.Errorf("SoC went below configured floor: %.2f < %.2f", first.SoC, p.SoCMin)
	}
}
