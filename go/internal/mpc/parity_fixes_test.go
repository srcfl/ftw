package mpc

// Parity fixes ported from the MILP formulation (#1020). Each test here
// pins one behaviour the DP and the Python champion disagreed on, so a
// future edit to either solver has to break a test to break parity.

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

// TestStrictSCBiasClampsNegativePrices: the bias is a penalty on house
// import, so it must never go negative. On a negative retail slot an
// unclamped 2×price bias INVERTS into a reward for importing — the DP
// would prefer buying the house's load from the grid over covering it
// from the battery, which is the opposite of self-consumption.
func TestStrictSCBiasClampsNegativePrices(t *testing.T) {
	if got := strictSCBiasOre(-200, 1.0); got != 0 {
		t.Errorf("bias at −200 öre/kWh = %v, want 0 (clamped, never an import bonus)", got)
	}
	if got := strictSCBiasOre(100, 2.0); got != 400 {
		t.Errorf("bias at 100 öre/kWh × 2 kWh = %v, want 400", got)
	}
}

// TestHorizonMeansAreLengthWeighted: with mixed 15/60-minute slots the
// unweighted mean over-counts the short ones. The mean feeds the
// confidence blend and the EV deadline penalty, so the error moves real
// decisions.
func TestHorizonMeansAreLengthWeighted(t *testing.T) {
	p := Params{ExportBonusOreKwh: 10}
	slots := []Slot{
		{StartMs: 0, LenMin: 15, PriceOre: 100, SpotOre: 20},
		{StartMs: 15 * 60 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 60},
	}
	meanPrice, meanExport := horizonMeans(slots, p)

	const wantPrice = (100*15 + 300*60) / 75.0 // 260, not the unweighted 200
	if math.Abs(meanPrice-wantPrice) > 1e-9 {
		t.Errorf("meanPrice = %v, want %v (unweighted would be 200)", meanPrice, wantPrice)
	}
	wantExport := (SlotExportPriceOre(slots[0], p)*15 + SlotExportPriceOre(slots[1], p)*60) / 75.0
	if math.Abs(meanExport-wantExport) > 1e-9 {
		t.Errorf("meanExport = %v, want %v", meanExport, wantExport)
	}
	unweightedExport := (SlotExportPriceOre(slots[0], p) + SlotExportPriceOre(slots[1], p)) / 2.0
	if math.Abs(wantExport-unweightedExport) < 1e-9 {
		t.Fatal("fixture is degenerate: weighted and unweighted export means coincide")
	}

	// Pure: the caller's slots come back untouched.
	if slots[0].Confidence != 0 || slots[0].PriceOre != 100 || slots[1].LenMin != 60 {
		t.Errorf("horizonMeans mutated its input: %+v", slots)
	}
}

// TestHorizonMeansFallBackToUnweighted covers the degenerate horizon
// where nothing carries a length — the means must still be finite.
func TestHorizonMeansFallBackToUnweighted(t *testing.T) {
	slots := []Slot{{PriceOre: 100, SpotOre: 10}, {PriceOre: 300, SpotOre: 30}}
	meanPrice, meanExport := horizonMeans(slots, Params{})
	if meanPrice != 200 {
		t.Errorf("meanPrice = %v, want 200", meanPrice)
	}
	if meanExport != 20 {
		t.Errorf("meanExport = %v, want 20", meanExport)
	}
}

// TestForwardSimStartsAtRealInitialSoC: the policy is looked up on the
// SoC grid, but the simulated trajectory must start at the battery's
// real SoC. Snapping the start to the nearest grid point invented (or
// lost) up to half a step of energy at t=0 — here half of 0.3 × 10 kWh
// = 1.5 kWh, reported to the operator as fact.
func TestForwardSimStartsAtRealInitialSoC(t *testing.T) {
	// Grid points are 0.1 / 0.4 / 0.7 / 1.0; 0.5 sits between two of
	// them and used to be reported as 0.4.
	p := Params{
		Mode:                ModeArbitrage,
		SoCMin:              0.1,
		SoCMax:              1.0,
		SoCLevels:           4,
		InitialSoC:          0.5,
		ActionLevels:        5,
		CapacityWh:          10000,
		MaxChargeW:          100,
		MaxDischargeW:       100,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    100, // == slot price → idle is optimal
	}
	slots := []Slot{{StartMs: 0, LenMin: 60, PriceOre: 100, SpotOre: 0,
		LoadW: 0, PVW: 0, Confidence: 1}}

	plan := Optimize(slots, p)
	if len(plan.Actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(plan.Actions))
	}
	if got := plan.Actions[0].BatteryW; got != 0 {
		t.Fatalf("fixture should idle, got BatteryW=%v", got)
	}
	if got := plan.Actions[0].SoC; math.Abs(got-0.5) > 1e-6 {
		t.Errorf("reported SoC = %v, want 0.5 (grid snap would give 0.4)", got)
	}
}

// TestPVBonusAppliesInEveryMode: the PV-charge bonus used to be gated
// to passive_arbitrage. The MILP never gated it, and the gate is not
// what makes the bonus safe — the bound to LIVE PV surplus is, which
// holds in every mode. Removing the gate is the point of this test
// (parity, #1020): in self_consumption, a battery sitting empty under
// PV surplus must absorb the sun rather than export it.
func TestPVBonusAppliesInEveryMode(t *testing.T) {
	// One hour, 2.5 kW of surplus. Export earns 110 öre/kWh; storing
	// earns the 100 öre/kWh terminal credit on 0.95 of what goes in.
	// Exporting therefore wins on price alone — until the bonus.
	slots := []Slot{{StartMs: 0, LenMin: 60, PriceOre: 150, SpotOre: 110,
		LoadW: 500, PVW: -3000, Confidence: 1}}
	base := Params{
		Mode:                ModeSelfConsumption,
		SoCMin:              0.1,
		SoCMax:              1.0,
		SoCLevels:           91,
		InitialSoC:          0.1, // empty
		ActionLevels:        41,
		CapacityWh:          10000,
		MaxChargeW:          2000,
		MaxDischargeW:       2000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		ExportOrePerKWh:     110,
		TerminalSoCPrice:    100,
	}

	noBonus := Optimize(slots, base)
	if len(noBonus.Actions) != 1 {
		t.Fatalf("no-bonus: got %d actions, want 1", len(noBonus.Actions))
	}
	if soc := noBonus.Actions[0].SoC; soc > base.SoCMin+0.01 {
		t.Fatalf("fixture broken: without the bonus the DP should export, "+
			"got SoC=%v BatteryW=%v", soc, noBonus.Actions[0].BatteryW)
	}

	withBonus := base
	withBonus.PVChargeBonusOreKwh = 30
	plan := Optimize(slots, withBonus)
	if len(plan.Actions) != 1 {
		t.Fatalf("bonus: got %d actions, want 1", len(plan.Actions))
	}
	if w := plan.Actions[0].BatteryW; w <= 0 {
		t.Errorf("self_consumption + PV bonus: BatteryW = %v, want charge — "+
			"the bonus must not be gated to passive_arbitrage", w)
	}
	if soc := plan.Actions[0].SoC; soc <= base.SoCMin+0.01 {
		t.Errorf("self_consumption + PV bonus: end SoC = %v, want well above the "+
			"%v floor", soc, base.SoCMin)
	}
}

// TestDiagnosticPersistsReplayEconomics: a snapshot that omits the
// arbitrage spread or the PV-downside inputs re-solves under different
// economics than the replan it claims to reproduce, which makes the
// replay bench measure the wrong thing.
func TestDiagnosticPersistsReplayEconomics(t *testing.T) {
	start := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC).UnixMilli()
	slots := []Slot{
		{StartMs: start, LenMin: 15, PriceOre: 100, SpotOre: 50,
			PVW: -200, LoadW: 400, Confidence: 1.0},
		{StartMs: start + 15*60*1000, LenMin: 15, PriceOre: 150, SpotOre: 80,
			PVW: -100, LoadW: 500, Confidence: 1.0},
	}
	p := Params{
		Mode:                     ModeArbitrage,
		SoCMin:                   0.1,
		SoCMax:                   0.95,
		SoCLevels:                11,
		InitialSoC:               0.5,
		ActionLevels:             7,
		CapacityWh:               10000,
		MaxChargeW:               5000,
		MaxDischargeW:            5000,
		ChargeEfficiency:         0.95,
		DischargeEfficiency:      0.95,
		TerminalSoCPrice:         100,
		MinArbitrageSpreadOreKwh: 30,
		PVUncertaintyW:           1891,
		PVRelativeUncertainty:    0.28,
		PVForecastSafetyK:        1,
	}
	plan := Optimize(slots, p)

	svc := &Service{
		Zone:         "SE4",
		last:         &plan,
		lastSlots:    slots,
		lastParams:   p,
		lastReplanAt: time.UnixMilli(plan.GeneratedAtMs),
		lastReason:   "unit-test",
	}
	d := svc.Diagnose()
	if d == nil {
		t.Fatal("Diagnose returned nil after a successful optimize")
	}

	blob, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDiagnosticBlob(blob)
	if err != nil {
		t.Fatal(err)
	}
	_, _, got, _, ok := planFromDiagnostic(loaded)
	if !ok {
		t.Fatal("round-tripped diagnostic not rehydratable")
	}
	if got.MinArbitrageSpreadOreKwh != 30 {
		t.Errorf("MinArbitrageSpreadOreKwh = %v, want 30", got.MinArbitrageSpreadOreKwh)
	}
	if got.PVUncertaintyW != 1891 {
		t.Errorf("PVUncertaintyW = %v, want 1891", got.PVUncertaintyW)
	}
	if got.PVRelativeUncertainty != 0.28 {
		t.Errorf("PVRelativeUncertainty = %v, want 0.28", got.PVRelativeUncertainty)
	}
	if got.PVForecastSafetyK != 1 {
		t.Errorf("PVForecastSafetyK = %v, want 1", got.PVForecastSafetyK)
	}
}
