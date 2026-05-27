package mpc

import (
	"math"
	"testing"
)

// baseParams = small-but-realistic problem for tests.
func baseParams(mode Mode) Params {
	return Params{
		Mode:                mode,
		SoCLevels:           21,
		CapacityWh:          10000, // 10 kWh
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        21,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    0, // neutral — force cost minimization
		ExportOrePerKWh:     0,
	}
}

// Helper: 4 slots × 60 min, no PV, flat 1000W load.
func flatLoadSlots(prices []float64) []Slot {
	out := make([]Slot, len(prices))
	for i, p := range prices {
		out[i] = Slot{
			StartMs:  int64(i) * 60 * 60 * 1000,
			LenMin:   60,
			PriceOre: p,
			PVW:      0,
			LoadW:    1000,
		}
	}
	return out
}

// ---- Mode: self_consumption ----

func TestSelfConsumptionNoGridCharge(t *testing.T) {
	// Flat load 1000W, no PV. In self_consumption we can only discharge
	// to cover load — we should NEVER import to charge.
	prices := []float64{100, 200, 50, 300} // cheap slot at index 2
	slots := flatLoadSlots(prices)
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 80 // full-ish
	plan := Optimize(slots, p)
	for i, a := range plan.Actions {
		// In self-consumption with only load and no PV: baseline_grid = load = +1000.
		// grid_w must be in [0, 1000]. Battery must be ≤ 0 (discharge) or 0.
		if a.BatteryW > 1e-6 {
			t.Errorf("slot %d: charging %fW from grid in self_consumption (price %f)",
				i, a.BatteryW, a.PriceOre)
		}
		if a.GridW < -1e-6 || a.GridW > 1000+1e-6 {
			t.Errorf("slot %d: grid %fW outside [0,1000] in self_consumption", i, a.GridW)
		}
	}
}

func TestSelfConsumptionAbsorbsPVSurplus(t *testing.T) {
	// 2000W load, 3500W PV (1500W surplus). Battery should charge from surplus.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, LoadW: 2000, PVW: -3500},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 50
	plan := Optimize(slots, p)
	a := plan.Actions[0]
	if a.BatteryW < 0 {
		t.Errorf("should charge from PV surplus, got %fW", a.BatteryW)
	}
	if a.GridW < -1e-6 {
		// We can tolerate a small exported fraction if action grid is coarse,
		// but gridW should not be more negative than -baseline (i.e. full surplus).
		if a.GridW < -1500-1e-6 {
			t.Errorf("grid export %fW exceeds surplus", a.GridW)
		}
	}
}

// Operator preference (2026-05-25): when PV surplus is available AND
// the battery has room, the planner must prefer charging from the live
// PV over exporting it and refilling from a forecast cheap-grid slot
// later. Without the PVChargeBonus the DP saw the two as economically
// equivalent on flat-price days and routinely exported 27+ kWh of
// cheap-spot PV (10 öre/kWh) while leaving the battery half-empty —
// then planned to refill from a 30 öre/kWh night slot, eating the
// round-trip loss and forecast risk for no gain.
func TestPassiveArbitragePVChargeBonusPrefersPVOverExport(t *testing.T) {
	// Single slot with PV surplus. Terminal == export rate so the
	// economic value of charging-and-holding exactly equals exporting.
	// Without bonus, charging loses by the round-trip efficiency hit
	// (0.95 vs 1.0) — DP picks export. With bonus, charging strictly
	// wins by the bonus magnitude.
	slots := []Slot{{
		StartMs:    0,
		LenMin:     60,
		PriceOre:   100,
		SpotOre:    20,
		LoadW:      500,
		PVW:        -5000,
		Confidence: 1,
	}}
	pNoBonus := baseParams(ModePassiveArbitrage)
	pNoBonus.InitialSoCPct = 60
	pNoBonus.SoCSafetyFloorPct = 0 // disable safety floor — focus the test on the bonus
	pNoBonus.TerminalSoCPrice = 20 // matches export revenue → DP indifferent without bonus
	pNoBonus.PVChargeBonusOreKwh = 0
	planNoBonus := Optimize(slots, pNoBonus)

	// With PV bonus: DP strictly prefers charging from PV in every
	// PV-surplus slot, even at parity terminal credit.
	pBonus := pNoBonus
	pBonus.PVChargeBonusOreKwh = 30
	planBonus := Optimize(slots, pBonus)

	sumCharge := func(actions []Action) float64 {
		var s float64
		for _, a := range actions {
			if a.BatteryW > 0 {
				s += a.BatteryW
			}
		}
		return s
	}
	noBonusCharge := sumCharge(planNoBonus.Actions)
	bonusCharge := sumCharge(planBonus.Actions)

	if bonusCharge <= noBonusCharge {
		t.Errorf("PV bonus should drive MORE charge: no-bonus=%fW, bonus=%fW", noBonusCharge, bonusCharge)
	}
	// Quantitative check: with 4.5 kW surplus per slot over 4 slots and
	// the bonus active, the planner should grab a meaningful share of
	// the available surplus.
	if bonusCharge < 2000 {
		t.Errorf("PV bonus drove only %fW total charge across 4 surplus slots — expected substantial absorption", bonusCharge)
	}
}

// PV bonus must NOT motivate grid-charge during PV-less slots.
// Bonus is bounded by live surplus; in a no-PV slot surplus=0 → no
// bonus → no extra incentive to grid-charge.
func TestPassiveArbitragePVChargeBonusDoesNotMotivateGridCharge(t *testing.T) {
	slots := make([]Slot, 4)
	for i := range slots {
		slots[i] = Slot{
			StartMs:    int64(i) * 15 * 60 * 1000,
			LenMin:     15,
			PriceOre:   30, // cheap-ish import
			SpotOre:    5,
			LoadW:      500,
			PVW:        0, // no PV
			Confidence: 1,
		}
	}
	p := baseParams(ModePassiveArbitrage)
	p.InitialSoCPct = 50
	p.SoCSafetyFloorPct = 0 // disable safety floor — focus the test on the bonus
	p.TerminalSoCPrice = 50 // moderate — SC bias must be what blocks grid-charge
	p.PVChargeBonusOreKwh = 30

	plan := Optimize(slots, p)
	// No slot should command a grid-charge (battW must not exceed
	// what could be PV-supplied, which is 0 here). The SC bias
	// (3× house-import cost) makes grid-charging unprofitable at
	// terminal=50 / import=30; the PV bonus does not change that
	// because it is bounded by live PV surplus (= 0 here).
	for i, a := range plan.Actions {
		if a.BatteryW > 100 {
			t.Errorf("slot %d: BatteryW = %fW — PV bonus must not motivate grid-charge", i, a.BatteryW)
		}
	}
}

// 2026-05-25 morning regression: with SoC below the operational safety
// floor and PV surplus available NOW, the planner used to defer
// charging to peak-PV hours because terminal credit alone gave only a
// marginal preference for charging. Operator's mental model and risk
// management both want "fill the battery NOW while sun is shining".
//
// With SoCSafetyFloorPct + SafetyFloorPenaltyOreKwhHour, the DP gets a
// per-slot penalty for every kWh-hour the SoC ends below the floor on
// a PV-surplus slot — so deferring to a later slot costs strictly more
// than charging in slot 0.
// passive_arbitrage merges planner_self + planner_cheap into one mode
// where the DP picks the cheapest charging source per slot (PV when
// surplus exists, grid otherwise). Discharge is still confined to
// local load — no battery export to grid. The strict-SC bias still
// applies so house load is preferentially served from battery.
//
// Summer-like scenario: PV surplus throughout. DP should charge from
// PV, never from grid.
func TestPassiveArbitrageChargesFromPVWhenSurplusAvailable(t *testing.T) {
	slots := make([]Slot, 8)
	for i := range slots {
		slots[i] = Slot{
			StartMs:    int64(i) * 15 * 60 * 1000,
			LenMin:     15,
			PriceOre:   100, // would be cheap to import too
			SpotOre:    10,
			LoadW:      500,
			PVW:        -3000, // PV >> load
			Confidence: 1,
		}
	}
	p := baseParams(ModePassiveArbitrage)
	p.InitialSoCPct = 20
	p.TerminalSoCPrice = 100

	plan := Optimize(slots, p)
	// Should charge in early slots from PV.
	if plan.Actions[0].BatteryW <= 0 {
		t.Errorf("slot 0: BatteryW = %f W, should charge from PV surplus", plan.Actions[0].BatteryW)
	}
	// Should NEVER export from battery (grid <= 0 only when PV exports it).
	for i, a := range plan.Actions {
		// In passive_arbitrage, gridW = baseline + battW must be >= min(0, baseline).
		baseline := a.LoadW + a.PVW
		minGrid := baseline
		if minGrid > 0 {
			minGrid = 0
		}
		if a.GridW < minGrid-50 {
			t.Errorf("slot %d: gridW = %f W < minGrid = %f W — battery is exporting (forbidden in passive_arbitrage)", i, a.GridW, minGrid)
		}
	}
}

// Winter-like scenario: no PV, cheap night prices followed by expensive
// morning. DP should grid-charge during the cheap window for use during
// the expensive window. This is what differentiates passive_arbitrage
// from planner_self (which forbids grid-charge entirely).
func TestPassiveArbitrageGridChargesAtCheapHours(t *testing.T) {
	// 4 cheap-night slots (spot 5 öre, retail 30) + 4 expensive-morning
	// slots (spot 200 öre, retail 250). No PV throughout. Load 500 W.
	slots := make([]Slot, 8)
	for i := range slots {
		if i < 4 {
			slots[i] = Slot{
				StartMs: int64(i) * 60 * 60 * 1000, LenMin: 60,
				PriceOre: 30, SpotOre: 5, LoadW: 500, PVW: 0, Confidence: 1,
			}
		} else {
			slots[i] = Slot{
				StartMs: int64(i) * 60 * 60 * 1000, LenMin: 60,
				PriceOre: 250, SpotOre: 200, LoadW: 500, PVW: 0, Confidence: 1,
			}
		}
	}
	p := baseParams(ModePassiveArbitrage)
	p.InitialSoCPct = 20
	p.TerminalSoCPrice = 50 // low so terminal credit doesn't dominate the test
	// Big enough buffer so a charge during cheap hours fits.
	p.SoCMinPct = 10
	p.SoCMaxPct = 95

	plan := Optimize(slots, p)
	// Sum charge during cheap slots, sum discharge during expensive slots.
	var chargeCheap, dischargeExpensive float64
	for i, a := range plan.Actions {
		if i < 4 && a.BatteryW > 0 {
			chargeCheap += a.BatteryW
		}
		if i >= 4 && a.BatteryW < 0 {
			dischargeExpensive += -a.BatteryW
		}
	}
	if chargeCheap < 500 {
		t.Errorf("expected grid-charge during cheap hours, got total %f W across cheap slots", chargeCheap)
	}
	if dischargeExpensive < 500 {
		t.Errorf("expected discharge during expensive hours, got total %f W across expensive slots", dischargeExpensive)
	}
}

// Critical invariant: passive_arbitrage must NEVER discharge into the
// grid (no battery-export). This is what separates it from active
// arbitrage and what the operator picked when they chose "passive".
func TestPassiveArbitrageNeverExportsFromBattery(t *testing.T) {
	// Battery starts charged; expensive slot with cheap export rate
	// would tempt DP to discharge into grid for arbitrage. Mode must
	// refuse.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, SpotOre: 300, LoadW: 100, PVW: 0, Confidence: 1},
	}
	p := baseParams(ModePassiveArbitrage)
	p.InitialSoCPct = 80 // plenty of stored energy

	plan := Optimize(slots, p)
	a := plan.Actions[0]
	// Battery may discharge to cover load (100 W), but no more — must
	// not push grid into export.
	if a.BatteryW < -150 {
		t.Errorf("BatteryW = %f W — passive_arbitrage must not discharge beyond local load", a.BatteryW)
	}
	if a.GridW < -50 {
		t.Errorf("gridW = %f W — passive_arbitrage must not push grid into export", a.GridW)
	}
}

func TestSelfConsumptionSafetyFloorChargesEarlyWhenBelowFloor(t *testing.T) {
	// 12 slots × 15 min = 3 hours. PV surplus throughout (PV 5 kW,
	// load 500 W → 4.5 kW surplus). Without the safety floor, DP
	// would defer charging until peak-PV slot; with it, DP must
	// charge in slot 0.
	slots := make([]Slot, 12)
	for i := range slots {
		slots[i] = Slot{
			StartMs:    int64(i) * 15 * 60 * 1000,
			LenMin:     15,
			PriceOre:   200,
			SpotOre:    20,
			LoadW:      500,
			PVW:        -5000,
			Confidence: 1,
		}
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 10 // at hardware floor — needs to recover
	p.SoCSafetyFloorPct = 25
	p.SafetyFloorPenaltyOreKwhHour = 100
	p.TerminalSoCPrice = 200 // matches the post-fix mean-import behaviour

	plan := Optimize(slots, p)
	if plan.Actions[0].BatteryW <= 0 {
		t.Errorf("safety-floor active and SoC=10%% (below 25%% floor): slot 0 should charge, got %f W", plan.Actions[0].BatteryW)
	}
	// SoC must climb out of deficit fast — within ~3 slots.
	if plan.Actions[2].SoCPct < p.SoCSafetyFloorPct-1 {
		t.Errorf("after 3 slots of PV-surplus charging, SoC = %f%%; safety floor = %f%%",
			plan.Actions[2].SoCPct, p.SoCSafetyFloorPct)
	}
}

// Safety floor must NOT motivate grid-charging when there's no PV
// surplus. Operator's "never import" contract is non-negotiable —
// safety is a soft target, the floor is hard physics.
func TestSelfConsumptionSafetyFloorDoesNotGridCharge(t *testing.T) {
	// All slots: load 1000 W, PV 0. No surplus, only import covers
	// the load. DP must NOT charge the battery even though SoC is
	// below the safety floor; safety penalty is gated on PV surplus.
	slots := make([]Slot, 8)
	for i := range slots {
		slots[i] = Slot{
			StartMs:    int64(i) * 15 * 60 * 1000,
			LenMin:     15,
			PriceOre:   30, // cheap night
			SpotOre:    5,
			LoadW:      1000,
			PVW:        0,
			Confidence: 1,
		}
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 10
	p.SoCSafetyFloorPct = 25
	p.SafetyFloorPenaltyOreKwhHour = 100

	plan := Optimize(slots, p)
	for i, a := range plan.Actions {
		if a.BatteryW > 1 {
			t.Errorf("slot %d: BatteryW = %f W — safety floor must not trigger grid-charge", i, a.BatteryW)
		}
	}
}

func TestSelfConsumptionDefersPVStorageWhenCheaperSurplusAhead(t *testing.T) {
	// Operator report 2026-05-24: early positive export price, followed by
	// stronger PV at negative spot. Smart self-consumption should preserve
	// battery headroom and export the early PV instead of filling the battery
	// before the negative-price window.
	slots := []Slot{
		{StartMs: 0, LenMin: 15, PriceOre: 129, SpotOre: 33, LoadW: 1000, PVW: -4000, Confidence: 1},
		{StartMs: 15 * 60 * 1000, LenMin: 15, PriceOre: 75, SpotOre: -15, LoadW: 1000, PVW: -7000, Confidence: 1},
		{StartMs: 30 * 60 * 1000, LenMin: 15, PriceOre: 75, SpotOre: -15, LoadW: 1000, PVW: -7000, Confidence: 1},
		{StartMs: 45 * 60 * 1000, LenMin: 15, PriceOre: 220, SpotOre: 100, LoadW: 2500, PVW: 0, Confidence: 1},
		{StartMs: 60 * 60 * 1000, LenMin: 15, PriceOre: 220, SpotOre: 100, LoadW: 2500, PVW: 0, Confidence: 1},
	}
	p := Params{
		Mode:                ModeSelfConsumption,
		SoCLevels:           41,
		CapacityWh:          4000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       10,
		ActionLevels:        41,
		MaxChargeW:          4000,
		MaxDischargeW:       4000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    0,
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) != len(slots) {
		t.Fatalf("got %d actions, want %d", len(plan.Actions), len(slots))
	}
	if plan.Actions[0].BatteryW > IdleGateThresholdW {
		t.Fatalf("first slot battery_w = %.0f W (%s), want idle/export while later negative surplus is available",
			plan.Actions[0].BatteryW, plan.Actions[0].Reason)
	}
	laterCharge := plan.Actions[1].BatteryW + plan.Actions[2].BatteryW
	if laterCharge <= 0 {
		t.Fatalf("later negative-price PV should be stored, got slot1 %.0f W and slot2 %.0f W",
			plan.Actions[1].BatteryW, plan.Actions[2].BatteryW)
	}
}

func TestSmartSelfConsumptionExportsMorningPVAndChargesNegativeMidday(t *testing.T) {
	// The core "smart self-consumption" contract:
	//   07-10: PV surplus + high export price => export, keep headroom.
	//   10-14: PV surplus + negative spot => charge, avoid paid export.
	//   evening: discharge stored energy into local load.
	slots := make([]Slot, 0, 11)
	startMs := int64(7 * 60 * 60 * 1000)
	for h := 7; h < 10; h++ {
		slots = append(slots, Slot{
			StartMs:    startMs + int64(len(slots))*60*60*1000,
			LenMin:     60,
			PriceOre:   180,
			SpotOre:    100,
			LoadW:      1000,
			PVW:        -4000,
			Confidence: 1,
		})
	}
	for h := 10; h < 14; h++ {
		slots = append(slots, Slot{
			StartMs:    startMs + int64(len(slots))*60*60*1000,
			LenMin:     60,
			PriceOre:   60,
			SpotOre:    -20,
			LoadW:      1000,
			PVW:        -6000,
			Confidence: 1,
		})
	}
	for h := 18; h < 22; h++ {
		slots = append(slots, Slot{
			StartMs:    startMs + int64(len(slots))*60*60*1000,
			LenMin:     60,
			PriceOre:   260,
			SpotOre:    100,
			LoadW:      2000,
			PVW:        0,
			Confidence: 1,
		})
	}

	p := Params{
		Mode:                ModeSelfConsumption,
		SoCLevels:           41,
		CapacityWh:          10000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       10,
		ActionLevels:        41,
		MaxChargeW:          4000,
		MaxDischargeW:       4000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    0,
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) != len(slots) {
		t.Fatalf("got %d actions, want %d", len(plan.Actions), len(slots))
	}

	var morningChargeW, morningExportW, middayChargeW, eveningDischargeW float64
	for i, a := range plan.Actions {
		switch {
		case i < 3:
			if a.BatteryW > 0 {
				morningChargeW += a.BatteryW
			}
			if a.GridW < 0 {
				morningExportW += -a.GridW
			}
		case i < 7:
			if a.BatteryW > 0 {
				middayChargeW += a.BatteryW
			}
		default:
			if a.BatteryW < 0 {
				eveningDischargeW += -a.BatteryW
			}
		}
	}

	if morningChargeW > IdleGateThresholdW*3 {
		t.Fatalf("morning charge = %.0f W-sum, want near zero so high-price PV exports", morningChargeW)
	}
	if morningExportW < 8000 {
		t.Fatalf("morning export = %.0f W-sum, want most of the 9 kW-sum surplus exported", morningExportW)
	}
	if middayChargeW < 7000 {
		t.Fatalf("midday charge = %.0f W-sum, want charging shifted into negative-price PV window", middayChargeW)
	}
	if eveningDischargeW <= 0 {
		t.Fatalf("evening discharge = %.0f W-sum, want stored midday energy used for local load", eveningDischargeW)
	}
}

// ---- Mode: cheap_charge ----

func TestCheapChargeUsesCheapGrid(t *testing.T) {
	// Flat 1000W load, no PV. Prices 100,100,50,100,100,100. Cheap hour
	// is slot 2. The planner SHOULD charge in slot 2 to reduce import
	// later — but since there's no expensive hour later, it only helps
	// if we credit SoC at the terminal. Set a modest terminal credit.
	prices := []float64{100, 100, 50, 100, 100, 100}
	slots := flatLoadSlots(prices)
	p := baseParams(ModeCheapCharge)
	p.InitialSoCPct = 30
	p.TerminalSoCPrice = 100 // credit stored energy at 100 öre/kWh
	plan := Optimize(slots, p)

	cheapSlotBattery := plan.Actions[2].BatteryW
	expensiveSlotBattery := plan.Actions[0].BatteryW
	if cheapSlotBattery <= expensiveSlotBattery {
		t.Errorf("cheap_charge should charge more in cheap slot: cheap=%f expensive=%f",
			cheapSlotBattery, expensiveSlotBattery)
	}
}

func TestCheapChargeNeverExports(t *testing.T) {
	// With a very expensive slot, arbitrage would discharge to grid.
	// cheap_charge must not.
	prices := []float64{50, 50, 500, 50}
	slots := flatLoadSlots(prices)
	p := baseParams(ModeCheapCharge)
	p.InitialSoCPct = 90
	p.ExportOrePerKWh = 400 // tempting
	plan := Optimize(slots, p)
	for i, a := range plan.Actions {
		if a.GridW < -1e-6 {
			t.Errorf("slot %d: grid export %fW in cheap_charge", i, a.GridW)
		}
	}
}

// ---- Mode: arbitrage ----

func TestArbitrageDischargesToExpensive(t *testing.T) {
	// Charge cheap, export to grid during expensive hour.
	prices := []float64{50, 50, 500, 50}
	slots := flatLoadSlots(prices)
	// Force SoC to plenty, give meaningful export credit.
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 80
	p.ExportOrePerKWh = 400
	plan := Optimize(slots, p)
	// Slot 2 (price 500) should see discharge (battery < 0).
	if plan.Actions[2].BatteryW >= -1e-6 {
		t.Errorf("arbitrage should discharge when price spikes: got %fW at price %f",
			plan.Actions[2].BatteryW, plan.Actions[2].PriceOre)
	}
}

// Regression: don't simultaneously discharge the home battery for grid
// export AND charge the EV in the same slot. Either is fine alone but
// together it's strictly worse than picking a slot for one or the
// other — the EV either erodes the export profit or laundered grid
// through the battery at 2× round-trip loss.
func TestArbitrageNoEVChargeWhileBatteryExporting(t *testing.T) {
	// Two slots: slot 0 cheap-with-PV (battery should charge from PV,
	// EV could too), slot 1 expensive (battery should discharge to
	// export). With the constraint, slot 1 must NOT also charge the EV.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 50, SpotOre: 50, LoadW: 500, PVW: -3000, Confidence: 1},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 800, SpotOre: 800, LoadW: 500, PVW: 0, Confidence: 1},
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 80 // headroom for big discharge
	p.ExportOrePerKWh = 700
	p.Loadpoint = &LoadpointSpec{
		ID:               "garage",
		CapacityWh:       60000,
		Levels:           11,
		InitialSoCPct:    20,
		PluggedIn:        true,
		TargetSoCPct:     30,
		TargetSlotIdx:    1, // deadline at slot 1 forces some EV charging
		MaxChargeW:       11000,
		AllowedStepsW:    []float64{0, 5000, 11000},
		ChargeEfficiency: 0.9,
	}
	plan := Optimize(slots, p)

	for i, a := range plan.Actions {
		gridW := a.LoadW + a.PVW + a.BatteryW + a.LoadpointW
		// gridW < -50 means real export this slot.
		if a.LoadpointW > 0 && a.BatteryW < 0 && gridW < -50 {
			t.Errorf("slot %d: EV charging at %fW while battery exports (battW=%f gridW=%f) — constraint violated",
				i, a.LoadpointW, a.BatteryW, gridW)
		}
	}
}

// Regression: arbitrage at negative spot must NOT discharge battery to
// grid. Real incident 2026-05-02: operator switched to planner_arbitrage
// during −5 öre spot prices and watched batteries discharge full power
// into the grid. Root cause was slotExportOre clamping at 0 — exporting
// at negative spot looked "free" instead of "costly", so the DP tied on
// multiple paths and could pick aggressive discharge by tie-break.
//
// With the clamp removed (default), exporting at negative spot is a
// positive cost, so the DP MUST prefer idle/charge over discharge.
func TestArbitrageDoesNotDischargeAtNegativeSpot(t *testing.T) {
	// Six 60-min slots, all with negative spot. Light load + steady PV
	// surplus, so the baseline grid is already exporting; the question
	// is whether the battery makes export *worse*.
	slots := make([]Slot, 6)
	for i := range slots {
		slots[i] = Slot{
			StartMs:    int64(i) * 60 * 60 * 1000,
			LenMin:     60,
			PriceOre:   80,   // consumer total stays positive (grid + VAT
			SpotOre:    -5.0, // wholesale spot pays you to consume
			LoadW:      500,
			PVW:        -2000, // 2 kW solar
			Confidence: 1.0,
		}
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 60      // plenty of headroom either direction
	p.TerminalSoCPrice = 80.0 // realistic — same scale as PriceOre, so
	//                            cycling losses register in the objective
	plan := Optimize(slots, p)

	for i, a := range plan.Actions {
		if a.BatteryW < -1e-6 {
			t.Errorf("slot %d: battery discharging %fW at negative spot %.2f öre",
				i, a.BatteryW, a.SpotOre)
		}
	}
}

func TestSlotGridCostOreCostsNegativeExport(t *testing.T) {
	slot := Slot{LenMin: 60, PriceOre: 80, SpotOre: -5}
	p := baseParams(ModeArbitrage)

	got := SlotGridCostOre(slot, -1.0, p)
	if math.Abs(got-5.0) > 1e-9 {
		t.Fatalf("negative export should be a positive cost: got %.3f, want 5.000", got)
	}
}

func TestOptimizeReportedCostUsesNegativeExportPrice(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 80, SpotOre: -5, LoadW: 0, PVW: -1000, Confidence: 1},
	}
	p := baseParams(ModeArbitrage)
	p.MaxChargeW = 0
	p.MaxDischargeW = 0
	p.ActionLevels = 3
	p.TerminalSoCPrice = 0

	plan := Optimize(slots, p)
	if len(plan.Actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(plan.Actions))
	}
	if math.Abs(plan.Actions[0].CostOre-5.0) > 1e-9 {
		t.Fatalf("CostOre = %.3f, want 5.000 for 1 kWh export at -5 öre", plan.Actions[0].CostOre)
	}
	if math.Abs(plan.TotalCostOre-5.0) > 1e-9 {
		t.Fatalf("TotalCostOre = %.3f, want 5.000", plan.TotalCostOre)
	}
}

// With ExportFloorOreKwh set to a pointer-to-zero, the old clamp
// behaviour returns: export at negative spot looks free again. This
// codifies the back-compat knob for retailers that don't bill for
// negative-spot export, and also guards against accidental removal of
// the per-customer override.
func TestArbitrageNegativeSpotWithExportFloorClampsAtZero(t *testing.T) {
	zero := 0.0
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 80, SpotOre: -5.0,
			LoadW: 500, PVW: -2000, Confidence: 1.0},
		// Second slot identical so the planner has multiple
		// indistinguishable options to pick from. With the floor at 0
		// the cost of discharging vs idling is exactly equal — but
		// neither should exhibit a *positive* cost (i.e. v < 0 must
		// have been clamped out).
		{StartMs: 60 * 60 * 1000, LenMin: 60, PriceOre: 80, SpotOre: -5.0,
			LoadW: 500, PVW: -2000, Confidence: 1.0},
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 60
	p.ExportFloorOreKwh = &zero
	p.TerminalSoCPrice = 0
	// We don't assert a specific dispatch — the floor makes export
	// a tie. The important invariant is that negative spot is floored
	// out of both per-slot and total reported cost.
	plan := Optimize(slots, p)
	for i, a := range plan.Actions {
		if a.GridW < 0 && math.Abs(a.CostOre) > 1e-6 {
			t.Errorf("slot %d: exporting at floor=0 should cost exactly 0, got %f öre",
				i, a.CostOre)
		}
	}
}

// ---- Efficiency ----

func TestEfficiencyCostsSoC(t *testing.T) {
	// Charging 1000W × 1h with 95% eff should add 950Wh to SoC (9.5% of 10kWh).
	// Use fine-grained SoC buckets to avoid snap rounding.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, LoadW: 0, PVW: -1000},
	}
	p := baseParams(ModeArbitrage)
	p.SoCLevels = 171 // 0.5%-grid: (95-10)/170 = 0.5
	p.InitialSoCPct = 50
	p.ActionLevels = 11
	p.MaxChargeW = 1000
	p.MaxDischargeW = 0
	p.TerminalSoCPrice = 100 // give DP reason to charge (vs let PV waste)
	plan := Optimize(slots, p)
	a := plan.Actions[0]
	expected := 50.0 + (1000*1.0*0.95)/10000.0*100.0
	if math.Abs(a.SoCPct-expected) > 1.0 {
		t.Errorf("eff-aware SoC: got %f, want ~%f", a.SoCPct, expected)
	}
}

func TestRoundTripLossMakesArbitrageHarder(t *testing.T) {
	// Buy at 100, sell at 150, 50% round-trip → guaranteed loss (need ≥200
	// to break even). Start empty so the only way to "arbitrage" is charge
	// in slot 0 then sell in slot 1. Planner should hold.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, LoadW: 0, PVW: 0},
		{StartMs: 60 * 60 * 1000, LenMin: 60, PriceOre: 150, LoadW: 0, PVW: 0},
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 10 // empty
	p.ChargeEfficiency = 0.707
	p.DischargeEfficiency = 0.707
	p.ExportOrePerKWh = 150
	p.TerminalSoCPrice = 0
	plan := Optimize(slots, p)
	if math.Abs(plan.Actions[0].BatteryW) > 100 {
		t.Errorf("lossy arbitrage shouldn't charge from empty: slot0 batt=%f", plan.Actions[0].BatteryW)
	}
}

// ---- Output integrity ----

func TestGridEqualsLoadPlusPVPlusBattery(t *testing.T) {
	prices := []float64{100, 200, 50, 300}
	slots := flatLoadSlots(prices)
	plan := Optimize(slots, baseParams(ModeArbitrage))
	for i, a := range plan.Actions {
		want := a.LoadW + a.PVW + a.BatteryW
		if math.Abs(a.GridW-want) > 1e-6 {
			t.Errorf("slot %d: grid %f != load+pv+batt %f", i, a.GridW, want)
		}
	}
}

func TestSoCStaysInBounds(t *testing.T) {
	prices := []float64{50, 500, 50, 500, 50, 500, 50, 500}
	slots := flatLoadSlots(prices)
	p := baseParams(ModeArbitrage)
	p.ExportOrePerKWh = 400
	plan := Optimize(slots, p)
	for i, a := range plan.Actions {
		if a.SoCPct < p.SoCMinPct-1e-6 || a.SoCPct > p.SoCMaxPct+1e-6 {
			t.Errorf("slot %d: SoC %f outside [%f, %f]", i, a.SoCPct, p.SoCMinPct, p.SoCMaxPct)
		}
	}
}

func TestEmptySlotsReturnsEmptyPlan(t *testing.T) {
	plan := Optimize(nil, baseParams(ModeSelfConsumption))
	if len(plan.Actions) != 0 {
		t.Errorf("empty input should return empty plan, got %d actions", len(plan.Actions))
	}
}

// ---- Mode enforcement at boundary ----

// ---- Tariffs + export bonus ----

func TestImportTariffRaisesMPCImportCost(t *testing.T) {
	// Tariff-free vs heavy-tariff day: same spot, very different consumer
	// prices. cheap_charge should charge LESS aggressively when import
	// tariff is high (because grid import is more expensive).
	makeSlots := func(total float64) []Slot {
		s := make([]Slot, 4)
		for i := range s {
			s[i] = Slot{
				StartMs:  int64(i) * 3600 * 1000,
				LenMin:   60,
				PriceOre: total,
				LoadW:    500,
				PVW:      0,
			}
		}
		return s
	}
	p := baseParams(ModeCheapCharge)
	p.InitialSoCPct = 30
	p.TerminalSoCPrice = 100

	cheap := Optimize(makeSlots(50), p)   // low consumer price — grid-charge
	tariff := Optimize(makeSlots(300), p) // high consumer price — hold off

	var chgCheap, chgTariff float64
	for _, a := range cheap.Actions {
		chgCheap += math.Max(a.BatteryW, 0)
	}
	for _, a := range tariff.Actions {
		chgTariff += math.Max(a.BatteryW, 0)
	}
	if chgTariff >= chgCheap {
		t.Errorf("high-tariff charge (%.0fW) should be less than low-tariff charge (%.0fW)", chgTariff, chgCheap)
	}
}

func TestExportBonusMakesArbitrageMoreProfitable(t *testing.T) {
	// With a big export bonus, arbitrage should discharge MORE at
	// expensive hours because revenue per kWh is higher.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 50, LoadW: 500, PVW: 0},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 500, LoadW: 500, PVW: 0},
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 80
	p.TerminalSoCPrice = 0

	p.ExportOrePerKWh = 40
	lowBonus := Optimize(slots, p)

	p.ExportOrePerKWh = 200
	highBonus := Optimize(slots, p)

	if highBonus.TotalCostOre >= lowBonus.TotalCostOre {
		t.Errorf("high export bonus should yield more revenue (lower cost): low=%.1f high=%.1f",
			lowBonus.TotalCostOre, highBonus.TotalCostOre)
	}
}

// ---- Rain-check: export-now vs store-for-later ----
//
// Scenario: morning price is HIGH, midday price drops, evening is
// moderate. PV peaks at midday (typical curve). What should arbitrage do?
//   - morning: export PV immediately — price is good right now
//   - midday: store PV — price is low, storing banks kWh for evening
//   - evening: discharge battery — realize the arbitrage
//
// This lines up with "when price is high in the morning we'd rather
// sell PV than store it; when price dips at midday we'd rather store
// than sell cheap". Confirms the DP handles opportunity-cost
// reasoning correctly.
func TestExportWhenMorningIsHighStoreWhenMiddayIsLow(t *testing.T) {
	// 24 × 1-hour slots. PV is a Gaussian centered at 12:00 peaking
	// at 8 kW. Prices: morning 07-09 = 200 öre, midday 11-14 = 50,
	// evening 17-20 = 150, else 100.
	slots := make([]Slot, 24)
	for h := 0; h < 24; h++ {
		var price float64
		switch {
		case h >= 7 && h <= 9:
			price = 200
		case h >= 11 && h <= 14:
			price = 50
		case h >= 17 && h <= 20:
			price = 150
		default:
			price = 100
		}
		var pvW float64
		if h >= 6 && h <= 18 {
			// Gaussian peak at 12, width 3h.
			pvW = 8000 * math.Exp(-0.5*math.Pow(float64(h-12)/3.0, 2))
		}
		slots[h] = Slot{
			StartMs:    int64(h) * 3600 * 1000,
			LenMin:     60,
			PriceOre:   price,
			SpotOre:    price * 0.7, // rough: strip tariff + VAT for export
			PVW:        -pvW,
			LoadW:      500,
			Confidence: 1.0,
		}
	}

	p := Params{
		Mode:                ModeArbitrage,
		SoCLevels:           41,
		CapacityWh:          10000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       40,
		ActionLevels:        21,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		// Per-slot export pricing: leave ExportOrePerKWh=0 and let the
		// DP compute slot.SpotOre + bonus − fee. This is the realistic
		// Nordic setup where export earns spot, not a fixed rate.
		ExportOrePerKWh:  0,
		TerminalSoCPrice: 100,
	}
	plan := Optimize(slots, p)

	sumMorningCharge := 0.0 // how much of the high-price PV gets stored
	sumMiddayCharge := 0.0  // how much of the cheap-price PV gets stored
	sumEveningDischarge := 0.0
	morningExport := 0.0 // how much leaves the site 07-09
	for _, a := range plan.Actions {
		h := int(a.SlotStartMs / (3600 * 1000))
		switch {
		case h >= 7 && h <= 9:
			if a.BatteryW > 0 {
				sumMorningCharge += a.BatteryW
			}
			if a.GridW < 0 {
				morningExport += -a.GridW
			}
		case h >= 11 && h <= 14:
			if a.BatteryW > 0 {
				sumMiddayCharge += a.BatteryW
			}
		case h >= 17 && h <= 20:
			if a.BatteryW < 0 {
				sumEveningDischarge += -a.BatteryW
			}
		}
	}

	t.Logf("morning charge W-hours : %6.0f  (should be low — sell now, not store)", sumMorningCharge)
	t.Logf("morning grid export Wh : %6.0f  (should be high — sell the PV)", morningExport)
	t.Logf("midday  charge W-hours : %6.0f  (should be high — store the cheap PV)", sumMiddayCharge)
	t.Logf("evening discharge Wh   : %6.0f  (should realise the arbitrage)", sumEveningDischarge)

	// Rain-check assertions.
	if sumMiddayCharge <= sumMorningCharge {
		t.Errorf("midday charge (%.0f) should exceed morning charge (%.0f) — DP should prefer storing cheap PV",
			sumMiddayCharge, sumMorningCharge)
	}
	if morningExport <= 0 {
		t.Errorf("morning export should be positive — high-price PV should leave the site, got %.0f", morningExport)
	}
	if sumEveningDischarge <= 0 {
		t.Errorf("evening discharge should be positive to realise arbitrage, got %.0f", sumEveningDischarge)
	}
}

// ---- Solar curtailment ----

func TestCurtailmentFlagsNegativeExportSlots(t *testing.T) {
	// Big PV surplus, no load absorption left (battery already full),
	// zero export revenue. Expect curtailment suggestion on those slots.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 10, LoadW: 500, PVW: -8000},
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 95 // already at max — battery can't absorb more
	p.ExportOrePerKWh = 0
	plan := Optimize(slots, p)
	a := plan.Actions[0]
	if a.PVLimitW == 0 {
		t.Errorf("expected curtailment on negative-export slot, got pv_limit_w=0 (grid_w=%f)", a.GridW)
	}
	// Recommended limit should roughly equal what the site can consume.
	expected := a.LoadW + math.Max(0, a.BatteryW)
	if math.Abs(a.PVLimitW-expected) > 500 {
		t.Errorf("pv_limit_w = %f, expected ~%f (load + charge)", a.PVLimitW, expected)
	}
}

func TestCurtailmentSkipsWhenExportProfitable(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, LoadW: 500, PVW: -8000},
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 95
	p.ExportOrePerKWh = 80 // profitable export
	plan := Optimize(slots, p)
	if plan.Actions[0].PVLimitW != 0 {
		t.Errorf("profitable export should not trigger curtailment, got pv_limit_w=%f",
			plan.Actions[0].PVLimitW)
	}
}

func TestCurtailmentSkipsPositiveSpotExport(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, SpotOre: 80, LoadW: 500, PVW: -8000, Confidence: 1},
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 95
	p.MaxDischargeW = 0
	p.ActionLevels = 3

	plan := Optimize(slots, p)
	if len(plan.Actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(plan.Actions))
	}
	if plan.Actions[0].GridW >= 0 {
		t.Fatalf("test setup expected PV export, got grid_w=%f", plan.Actions[0].GridW)
	}
	if plan.Actions[0].PVLimitW != 0 {
		t.Errorf("positive per-slot export price should not trigger curtailment, got pv_limit_w=%f",
			plan.Actions[0].PVLimitW)
	}
}

// ---- Edge cases / hardening ----

func TestOptimizeWithNaNPVDoesNotPanic(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, LoadW: 1000, PVW: math.NaN()},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 100, LoadW: 1000, PVW: 0},
	}
	p := baseParams(ModeArbitrage)
	plan := Optimize(slots, p)
	if len(plan.Actions) != 2 {
		t.Errorf("expected 2 actions, got %d", len(plan.Actions))
	}
	for i, a := range plan.Actions {
		if math.IsNaN(a.GridW) || math.IsInf(a.GridW, 0) {
			t.Fatalf("slot %d: grid_w is non-finite: %v", i, a.GridW)
		}
		if math.IsNaN(a.CostOre) || math.IsInf(a.CostOre, 0) {
			t.Fatalf("slot %d: cost_ore is non-finite: %v", i, a.CostOre)
		}
		if math.IsNaN(a.PVW) || math.IsInf(a.PVW, 0) {
			t.Fatalf("slot %d: pv_w is non-finite: %v", i, a.PVW)
		}
	}
}

func TestOptimizeDropsNonFinitePriceSlots(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: math.NaN(), SpotOre: 100, LoadW: 1000, PVW: 0},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 100, SpotOre: math.Inf(1), LoadW: 1000, PVW: 0},
		{StartMs: 2 * 3600 * 1000, LenMin: 60, PriceOre: 100, SpotOre: 100, LoadW: 1000, PVW: 0},
	}
	plan := Optimize(slots, baseParams(ModeArbitrage))
	if len(plan.Actions) != 1 {
		t.Fatalf("got %d actions, want only the finite-price slot", len(plan.Actions))
	}
	if plan.Actions[0].SlotStartMs != 2*3600*1000 {
		t.Fatalf("kept slot start %d, want final finite slot", plan.Actions[0].SlotStartMs)
	}
}

func TestOptimizeZeroCapacityReturnsEmptyPlan(t *testing.T) {
	slots := flatLoadSlots([]float64{100, 200, 50, 300})
	p := baseParams(ModeArbitrage)
	p.CapacityWh = 0
	plan := Optimize(slots, p)
	if len(plan.Actions) != 0 {
		t.Errorf("zero capacity should return empty plan, got %d actions", len(plan.Actions))
	}
}

func TestOptimizeZeroActionLevelsDoesNotPanic(t *testing.T) {
	slots := flatLoadSlots([]float64{100, 200})
	p := baseParams(ModeArbitrage)

	// ActionLevels = 0 — the optimizer clamps to 3 internally.
	p.ActionLevels = 0
	plan := Optimize(slots, p)
	if len(plan.Actions) != 2 {
		t.Errorf("ActionLevels=0: expected 2 actions, got %d", len(plan.Actions))
	}

	// ActionLevels = 1 — also clamped to 3.
	p.ActionLevels = 1
	plan = Optimize(slots, p)
	if len(plan.Actions) != 2 {
		t.Errorf("ActionLevels=1: expected 2 actions, got %d", len(plan.Actions))
	}
}

func TestZeroEfficiencyDoesNotPanic(t *testing.T) {
	// Zero or negative efficiency must not cause division-by-zero or NaN.
	// The optimizer should silently default to 0.95.
	slots := flatLoadSlots([]float64{100, 200, 50, 300})
	p := baseParams(ModeArbitrage)
	p.ChargeEfficiency = 0
	p.DischargeEfficiency = 0
	p.InitialSoCPct = 50
	p.ExportOrePerKWh = 100

	plan := Optimize(slots, p) // must not panic

	if len(plan.Actions) != len(slots) {
		t.Fatalf("expected %d actions, got %d", len(slots), len(plan.Actions))
	}
	for i, a := range plan.Actions {
		if math.IsNaN(a.SoCPct) || math.IsInf(a.SoCPct, 0) {
			t.Errorf("slot %d: SoC is NaN/Inf", i)
		}
		if math.IsNaN(a.CostOre) || math.IsInf(a.CostOre, 0) {
			t.Errorf("slot %d: cost is NaN/Inf", i)
		}
		if a.SoCPct < p.SoCMinPct-1e-6 || a.SoCPct > p.SoCMaxPct+1e-6 {
			t.Errorf("slot %d: SoC %f outside bounds", i, a.SoCPct)
		}
	}

	// Also test negative efficiency values.
	p.ChargeEfficiency = -0.5
	p.DischargeEfficiency = -1.0
	plan2 := Optimize(slots, p)
	for i, a := range plan2.Actions {
		if math.IsNaN(a.SoCPct) || math.IsInf(a.SoCPct, 0) {
			t.Errorf("negative-eff slot %d: SoC is NaN/Inf", i)
		}
	}
}

func TestSelfConsumptionWithZeroBaseline(t *testing.T) {
	// load==PV → baseline=0. Battery must stay at 0.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, LoadW: 2000, PVW: -2000},
	}
	p := baseParams(ModeSelfConsumption)
	plan := Optimize(slots, p)
	if math.Abs(plan.Actions[0].BatteryW) > 100 { // tolerance for action grid granularity
		t.Errorf("zero baseline should keep battery idle, got %f", plan.Actions[0].BatteryW)
	}
}
