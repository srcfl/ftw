package mpc

import (
	"math"
	"testing"
)

// Arbitrage over a clear cheap/expensive split should beat both
// no-battery and self-consumption baselines — that's the value of
// having the planner.
func TestBaselinesArbitrageBeatsNoBattery(t *testing.T) {
	// 4 hourly slots with a strong price dip at index 1 and a peak at 3.
	// Flat 1000W load, no PV. Arbitrage should buy cheap, sell expensive.
	prices := []float64{100, 20, 100, 300}
	slots := flatLoadSlots(prices)
	for i := range slots {
		slots[i].SpotOre = prices[i] // let export revenue scale with spot
	}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 50
	p.MaxChargeW = 3000
	p.MaxDischargeW = 3000

	plan := Optimize(slots, p)
	b := ComputeBaselines(slots, p)

	if b.NetKWh <= 0 {
		t.Fatalf("expected positive net kWh (flat load, no PV), got %f", b.NetKWh)
	}
	// Arbitrage plan must cost less than doing nothing (no battery).
	if plan.TotalCostOre >= b.NoBatteryOre {
		t.Errorf("arbitrage (%.1f öre) should beat no-battery (%.1f öre)",
			plan.TotalCostOre, b.NoBatteryOre)
	}
	// ...and less than passive self-consumption, which can't import at
	// price=20 to discharge at price=300.
	if plan.TotalCostOre >= b.SelfConsumptionOre {
		t.Errorf("arbitrage (%.1f öre) should beat self-consumption baseline (%.1f öre)",
			plan.TotalCostOre, b.SelfConsumptionOre)
	}
	// Average price over the horizon is the time-weighted mean.
	wantAvg := (100.0 + 20 + 100 + 300) / 4.0
	if math.Abs(b.AvgPriceOre-wantAvg) > 1e-6 {
		t.Errorf("avg price mismatch: got %f, want %f", b.AvgPriceOre, wantAvg)
	}
}

// NoBatteryOre must match analytical sum over slots at consumer price
// when all slots are importing. Locks down the cost model being shared
// with the DP's TotalCostOre path.
func TestBaselinesNoBatteryAnalytical(t *testing.T) {
	prices := []float64{50, 150, 100, 80}
	slots := flatLoadSlots(prices)
	p := baseParams(ModeArbitrage)

	b := ComputeBaselines(slots, p)

	// 1000W × 1h × price/1000 = price öre per slot. Sum over slots.
	want := 50.0 + 150 + 100 + 80
	if math.Abs(b.NoBatteryOre-want) > 1e-6 {
		t.Errorf("no-battery cost: got %.3f, want %.3f", b.NoBatteryOre, want)
	}
	// Net kWh = 4 slots × 1 kWh each.
	if math.Abs(b.NetKWh-4.0) > 1e-9 {
		t.Errorf("net kWh: got %.3f, want 4.0", b.NetKWh)
	}
}

// Self-consumption baseline must equal the cost of Optimize() called
// directly with ModeSelfConsumption on the same inputs. This is what
// makes the savings-vs-SC number honest — we're comparing to a real
// SC dispatch, not an approximation.
func TestBaselinesSelfConsumptionMatchesOptimize(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 80, LoadW: 1000, PVW: -2500, SpotOre: 80},
		{StartMs: 3_600_000, LenMin: 60, PriceOre: 120, LoadW: 1500, PVW: 0, SpotOre: 120},
		{StartMs: 7_200_000, LenMin: 60, PriceOre: 60, LoadW: 800, PVW: -300, SpotOre: 60},
		{StartMs: 10_800_000, LenMin: 60, PriceOre: 200, LoadW: 2000, PVW: 0, SpotOre: 200},
	}
	p := baseParams(ModeArbitrage) // doesn't matter — baseline overrides mode
	p.InitialSoCPct = 40

	pSC := p
	pSC.Mode = ModeSelfConsumption
	want := Optimize(slots, pSC).TotalCostOre

	got := ComputeBaselines(slots, p).SelfConsumptionOre
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("SC baseline mismatch: got %.6f, want %.6f (diff %.6f)",
			got, want, got-want)
	}
}

// Empty slot horizon returns a zero-valued Baselines without panicking.
func TestBaselinesEmpty(t *testing.T) {
	b := ComputeBaselines(nil, baseParams(ModeArbitrage))
	if b != (Baselines{}) {
		t.Errorf("expected zero Baselines for empty slots, got %+v", b)
	}
}

// FlatAvg must price imports and exports separately. The consumer total
// PriceOre includes grid tariffs that are not earned on export, so
// netting energy at one mean would credit each exported kWh at the
// import-tariff price and silently shrink the apparent timing value.
//
// Setup: 4 slots, 60 min each, SpotOre = PriceOre/2 (export earns half
// of consumer price, no bonus/fee). Load = 0 in slots 0..1, PV = -2 kW
// in slots 0..1; load = 2 kW in slots 2..3, no PV. So the no-battery
// flows are exactly: slot 0,1 export 2 kWh each; slot 2,3 import 2 kWh
// each. Symmetric volumes, asymmetric prices.
func TestBaselinesFlatAvgPricesImportExportSeparately(t *testing.T) {
	prices := []float64{100, 200, 300, 400} // consumer total, öre/kWh
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: prices[0], SpotOre: prices[0] / 2, LoadW: 0, PVW: -2000},
		{StartMs: 3_600_000, LenMin: 60, PriceOre: prices[1], SpotOre: prices[1] / 2, LoadW: 0, PVW: -2000},
		{StartMs: 7_200_000, LenMin: 60, PriceOre: prices[2], SpotOre: prices[2] / 2, LoadW: 2000, PVW: 0},
		{StartMs: 10_800_000, LenMin: 60, PriceOre: prices[3], SpotOre: prices[3] / 2, LoadW: 2000, PVW: 0},
	}
	p := baseParams(ModeArbitrage)

	b := ComputeBaselines(slots, p)

	// AvgPriceOre is the mean *import* price (consumer total).
	wantAvgImport := (100.0 + 200 + 300 + 400) / 4.0 // 250
	if math.Abs(b.AvgPriceOre-wantAvgImport) > 1e-6 {
		t.Errorf("avg import price: got %.3f, want %.3f", b.AvgPriceOre, wantAvgImport)
	}
	// NetKWh is import minus export. Both are 4 kWh ⇒ 0.
	if math.Abs(b.NetKWh) > 1e-9 {
		t.Errorf("net kWh: got %.6f, want 0 (4 in, 4 out)", b.NetKWh)
	}
	// FlatAvg priced separately: 4 kWh × 250 import − 4 kWh × 125 export
	// = 1000 − 500 = 500 öre. The old (buggy) implementation returned
	// netKWh × avgImport = 0 × 250 = 0 — i.e., it would tell the user
	// timing was worth nothing, which is exactly the case Erik flagged.
	wantFlat := 4.0*250.0 - 4.0*125.0
	if math.Abs(b.FlatAvgOre-wantFlat) > 1e-6 {
		t.Errorf("flat-avg cost: got %.3f, want %.3f", b.FlatAvgOre, wantFlat)
	}
	// Sanity: NoBattery on the same flows uses the same per-slot model,
	// just with the actual per-slot prices. Imports cost slot.PriceOre,
	// exports earn slot.SpotOre (no flat fee/bonus in baseParams).
	wantNoBat := 2.0*300.0 + 2.0*400.0 - 2.0*50.0 - 2.0*100.0 // 1400 − 300 = 1100
	if math.Abs(b.NoBatteryOre-wantNoBat) > 1e-6 {
		t.Errorf("no-battery cost: got %.3f, want %.3f", b.NoBatteryOre, wantNoBat)
	}
}

// Honor a flat ExportOrePerKWh feed-in tariff: every exported kWh in
// FlatAvgOre is credited at exactly that rate, regardless of slot spot.
func TestBaselinesFlatAvgHonorsFlatExportTariff(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 200, SpotOre: 50, LoadW: 0, PVW: -1000},
		{StartMs: 3_600_000, LenMin: 60, PriceOre: 200, SpotOre: 50, LoadW: 1000, PVW: 0},
	}
	p := baseParams(ModeArbitrage)
	p.ExportOrePerKWh = 60 // flat feed-in beats spot=50

	b := ComputeBaselines(slots, p)

	// Import: 1 kWh × 200 = 200. Export: 1 kWh × 60 = 60. FlatAvg = 140.
	wantFlat := 1.0*200.0 - 1.0*60.0
	if math.Abs(b.FlatAvgOre-wantFlat) > 1e-6 {
		t.Errorf("flat-avg with flat export tariff: got %.3f, want %.3f", b.FlatAvgOre, wantFlat)
	}
}
