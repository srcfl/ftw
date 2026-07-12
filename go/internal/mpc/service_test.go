package mpc

import (
	"math"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

func TestBuildSlotsFallsBackToForecastWhenTwinCollapses(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	cloud := 48.1
	forecastPV := 1488.5770353837524
	slots := buildSlots(
		[]state.PricePoint{{
			SlotTsMs:    ts,
			SlotLenMin:  15,
			SpotOreKwh:  120,
			TotalOreKwh: 180,
		}},
		[]state.ForecastPoint{{
			SlotTsMs:      ts,
			SlotLenMin:    60,
			CloudCoverPct: &cloud,
			PVWEstimated:  &forecastPV,
		}},
		2500,
		ts,
		func(time.Time, float64) float64 { return 0 },
		nil,
		nil,
	)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0].PVW; math.Abs(got+forecastPV) > 1e-6 {
		t.Fatalf("slot PVW = %f, want %f", got, -forecastPV)
	}
}

func TestBuildSlotsKeepsTwinWhenPredictionIsSane(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	cloud := 48.1
	forecastPV := 1488.5770353837524
	twinPV := 1180.0
	slots := buildSlots(
		[]state.PricePoint{{
			SlotTsMs:    ts,
			SlotLenMin:  15,
			SpotOreKwh:  120,
			TotalOreKwh: 180,
		}},
		[]state.ForecastPoint{{
			SlotTsMs:      ts,
			SlotLenMin:    60,
			CloudCoverPct: &cloud,
			PVWEstimated:  &forecastPV,
		}},
		2500,
		ts,
		func(time.Time, float64) float64 { return twinPV },
		nil,
		nil,
	)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0].PVW; math.Abs(got+twinPV) > 1e-6 {
		t.Fatalf("slot PVW = %f, want %f", got, -twinPV)
	}
}

// applyPVDownside is the Alt-2 safety mechanism: plan against forecast PV minus
// k·σ (recent PV-forecast error std) so the DP doesn't run the battery down
// betting on PV that may not arrive. The reserve emerges from the forecast
// uncertainty itself — no separate SoC/energy floor.
func TestApplyPVDownsideHaircutsGenerationByKSigma(t *testing.T) {
	slots := []Slot{
		{PVW: -3000}, // 3 kW generation
		{PVW: 0},     // night — no generation
		{PVW: -200},  // small PV, less than the haircut
	}
	applyPVDownside(slots, 1.0, 500) // k=1, σ=500 W → haircut 500 W

	if slots[0].PVW != -2500 {
		t.Errorf("PVW[0] = %v, want -2500 (3000 generation − 500 haircut)", slots[0].PVW)
	}
	if slots[1].PVW != 0 {
		t.Errorf("night PVW must stay 0, got %v", slots[1].PVW)
	}
	if slots[2].PVW != 0 {
		t.Errorf("PVW[2] = %v, want 0 (haircut exceeds the 200 W generation, floored)", slots[2].PVW)
	}
}

func TestApplyPVDownsideNoOpWhenDisabled(t *testing.T) {
	slots := []Slot{{PVW: -3000}}
	applyPVDownside(slots, 0, 500) // k=0 → raw forecast, no hedge
	if slots[0].PVW != -3000 {
		t.Errorf("k=0 must be a no-op, got %v", slots[0].PVW)
	}
	applyPVDownside(slots, 1.0, 0) // σ=0 (no error history) → no hedge
	if slots[0].PVW != -3000 {
		t.Errorf("σ=0 must be a no-op, got %v", slots[0].PVW)
	}
}

func TestApplyPVDownsideNegativeKIsNoOp(t *testing.T) {
	slots := []Slot{{PVW: -3000}}
	applyPVDownside(slots, -1.0, 500) // negative k must be guarded, not amplify PV
	if slots[0].PVW != -3000 {
		t.Errorf("negative k must be a no-op, got %v", slots[0].PVW)
	}
}

func TestApplyPVDownsideScalesWithK(t *testing.T) {
	slots := []Slot{{PVW: -3000}}
	applyPVDownside(slots, 2.0, 500) // k=2, σ=500 → haircut 1000 W
	if slots[0].PVW != -2000 {
		t.Errorf("PVW = %v, want -2000 (3000 − 2·500)", slots[0].PVW)
	}
}

// The Service seam reads σ from the PVUncertaintyW hook and the configured k.
func TestServiceApplyPVDownsideToSlotsUsesHookAndK(t *testing.T) {
	s := &Service{
		PVForecastSafetyK: 1.0,
		PVUncertaintyW:    func() float64 { return 800 }, // live σ = 800 W
	}
	slots := []Slot{{PVW: -3000}, {PVW: 0}}
	s.applyPVDownsideToSlots(slots)
	if slots[0].PVW != -2200 {
		t.Errorf("PVW[0] = %v, want -2200 (3000 − 1·800 from the hook)", slots[0].PVW)
	}
	if slots[1].PVW != 0 {
		t.Errorf("night slot must stay 0, got %v", slots[1].PVW)
	}
}

func TestServiceApplyPVDownsideToSlotsNoOpWithoutHook(t *testing.T) {
	s := &Service{PVForecastSafetyK: 1.0} // PVUncertaintyW unwired
	slots := []Slot{{PVW: -3000}}
	s.applyPVDownsideToSlots(slots)
	if slots[0].PVW != -3000 {
		t.Errorf("no σ hook → raw forecast, got %v", slots[0].PVW)
	}
}

func TestServiceApplyPVDownsideToSlotsNilServiceNoPanic(t *testing.T) {
	var s *Service
	slots := []Slot{{PVW: -3000}}
	s.applyPVDownsideToSlots(slots) // must not panic
	if slots[0].PVW != -3000 {
		t.Errorf("nil Service must be a no-op, got %v", slots[0].PVW)
	}
}

// TestBuildSlots_AppliesPVResidualCorrection: a non-nil
// PVResidualCorrector adds an additive bias to the twin's per-slot
// prediction BEFORE selectPlannerPVW blends with the forecast. We mock
// the corrector to apply -200 W to the first slot only and verify the
// final slot 0 PVW reflects the correction while slot 1 does not.
func TestBuildSlots_AppliesPVResidualCorrection(t *testing.T) {
	ts0 := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	ts1 := ts0 + 15*60*1000
	cloud := 30.0
	forecastPV := 0.0 // disable forecast blending so we see the pure twin path
	twinPV := 1500.0
	// Correction of -200 W means PV generation is being under-predicted
	// by 200 W on the rolling residual; final base should be 1300 W on
	// slot 0 only.
	correctedSlot := time.UnixMilli(ts0 + 15*30*1000).UTC() // slot 0 midpoint
	pvCorrect := func(now, tTarget time.Time, base float64) float64 {
		if tTarget.Equal(correctedSlot) {
			return -200
		}
		return 0
	}
	slots := buildSlots(
		[]state.PricePoint{
			{SlotTsMs: ts0, SlotLenMin: 15, SpotOreKwh: 120, TotalOreKwh: 180},
			{SlotTsMs: ts1, SlotLenMin: 15, SpotOreKwh: 120, TotalOreKwh: 180},
		},
		[]state.ForecastPoint{
			{SlotTsMs: ts0, SlotLenMin: 60, CloudCoverPct: &cloud, PVWEstimated: &forecastPV},
		},
		2500,
		ts0,
		func(time.Time, float64) float64 { return twinPV },
		pvCorrect,
		nil,
	)
	if len(slots) != 2 {
		t.Fatalf("got %d slots, want 2", len(slots))
	}
	// Slot 0: base 1500 + (-200) = 1300 → PVW = -1300 (site-sign).
	if got, want := slots[0].PVW, -1300.0; math.Abs(got-want) > 1e-6 {
		t.Fatalf("slot 0 PVW = %f, want %f (correction applied)", got, want)
	}
	// Slot 1: no correction → PVW = -1500.
	if got, want := slots[1].PVW, -1500.0; math.Abs(got-want) > 1e-6 {
		t.Fatalf("slot 1 PVW = %f, want %f (no correction)", got, want)
	}
}

// TestBuildSlots_NoDoubleCorrection: regression for the PR #381
// follow-up. The MPC must consume the UNANCHORED structural PV
// predictor plus the residual corrector — wiring the anchored
// predictor (which already folds in the same structural-vs-live bias)
// produces a double-correction so the planner sees ~0 W PV on a sunny
// day with a heavy downward residual.
//
// Worked example (Codex's reproduction):
//
//	structural prediction  = 1000 W
//	live PV measurement    =  500 W  (heavy downward bias of −500 W)
//	→ anchored Predict     ≈  500 W  (the now-anchor already pulled it)
//	→ ResidualCorrect      = −500 W  (rolling mean of the same bias)
//
// Wiring `PV = pvSvc.Predict` (anchored) + `PVResidualCorrect` gives the
// planner ≈ 0 W. Wiring `PV = pvSvc.PredictStructural` + `PVResidualCorrect`
// (the fix) gives the planner ≈ 500 W — the bias is corrected exactly
// once, by the residual layer that is designed for it.
//
// We simulate both wirings here and assert the structural one matches
// the single-correction outcome.
func TestBuildSlots_NoDoubleCorrection(t *testing.T) {
	ts0 := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	cloud := 30.0
	forecastPV := 0.0 // disable forecast blending to expose the pure twin path

	const structuralW = 1000.0
	const anchoredW = 500.0 // what Predict would return after the now-anchor
	const residualW = -500.0

	// Slot 0 midpoint — buildSlots passes this to both PV and PVResidualCorrect.
	correctedSlot := time.UnixMilli(ts0 + 15*30*1000).UTC()
	pvCorrect := func(now, tTarget time.Time, base float64) float64 {
		if tTarget.Equal(correctedSlot) {
			return residualW
		}
		return 0
	}

	// --- Buggy wiring (anchored predictor + residual): double correction ---
	slotsBuggy := buildSlots(
		[]state.PricePoint{{SlotTsMs: ts0, SlotLenMin: 15, SpotOreKwh: 120, TotalOreKwh: 180}},
		[]state.ForecastPoint{{SlotTsMs: ts0, SlotLenMin: 60, CloudCoverPct: &cloud, PVWEstimated: &forecastPV}},
		2500,
		ts0,
		func(time.Time, float64) float64 { return anchoredW },
		pvCorrect,
		nil,
	)
	// Anchored 500 + residual -500 = 0 → site-sign PVW = 0.
	if got, want := slotsBuggy[0].PVW, 0.0; math.Abs(got-want) > 1e-6 {
		t.Fatalf("buggy-wiring slot 0 PVW = %f, want %f (anchored + residual double-corrects)", got, want)
	}

	// --- Correct wiring (structural predictor + residual): single correction ---
	slotsFixed := buildSlots(
		[]state.PricePoint{{SlotTsMs: ts0, SlotLenMin: 15, SpotOreKwh: 120, TotalOreKwh: 180}},
		[]state.ForecastPoint{{SlotTsMs: ts0, SlotLenMin: 60, CloudCoverPct: &cloud, PVWEstimated: &forecastPV}},
		2500,
		ts0,
		func(time.Time, float64) float64 { return structuralW },
		pvCorrect,
		nil,
	)
	// Structural 1000 + residual -500 = 500 → site-sign PVW = -500.
	if got, want := slotsFixed[0].PVW, -500.0; math.Abs(got-want) > 1e-6 {
		t.Fatalf("fixed-wiring slot 0 PVW = %f, want %f (single correction reflects live bias)", got, want)
	}
}

// ---- upperHalfMeanPrice (arbitrage terminal valuation) ----

func TestUpperHalfMeanLiftsTerminalCreditAboveOverallMean(t *testing.T) {
	// Live-shaped horizon: midday cheap valley + evening peak. Mean is
	// pulled down by the cheap hours; upper-half mean reflects the
	// hours when stored SoC would actually be sold.
	prices := []state.PricePoint{
		{TotalOreKwh: 170}, // cheap midday
		{TotalOreKwh: 175},
		{TotalOreKwh: 180},
		{TotalOreKwh: 200},
		{TotalOreKwh: 250},
		{TotalOreKwh: 300},
		{TotalOreKwh: 320}, // evening peak
		{TotalOreKwh: 345},
	}
	overall := 0.0
	for _, p := range prices {
		overall += p.TotalOreKwh
	}
	overall /= float64(len(prices))
	got := upperHalfMeanPrice(prices)
	// Upper half = {250, 300, 320, 345} mean = 303.75.
	if math.Abs(got-303.75) > 0.01 {
		t.Errorf("upperHalfMeanPrice = %.2f, want 303.75", got)
	}
	if got <= overall {
		t.Errorf("upper-half mean (%.2f) must exceed overall mean (%.2f) on a non-flat horizon", got, overall)
	}
}

func TestUpperHalfMeanFallsBackForTinyHorizon(t *testing.T) {
	// With 4 or fewer slots, taking the "upper half" loses meaning —
	// fall back to plain mean.
	prices := []state.PricePoint{
		{TotalOreKwh: 100},
		{TotalOreKwh: 300},
	}
	got := upperHalfMeanPrice(prices)
	if math.Abs(got-200) > 0.01 {
		t.Errorf("upperHalfMeanPrice (tiny horizon) = %.2f, want 200 (plain mean)", got)
	}
}

func TestUpperHalfMeanEmptyReturnsZero(t *testing.T) {
	if got := upperHalfMeanPrice(nil); got != 0 {
		t.Errorf("upperHalfMeanPrice(nil) = %f, want 0", got)
	}
}

// ---- Terminal SoC valuation ----

func TestSelfConsumptionTerminalPriceIsMeanImport(t *testing.T) {
	// Retail 300 öre/kWh average across the horizon. Spot/bonus/fee are
	// irrelevant in self-consumption mode (operator never sells stored
	// energy, so the export side doesn't enter the value of a kept kWh).
	prices := []state.PricePoint{
		{SpotOreKwh: 80, TotalOreKwh: 300},
		{SpotOreKwh: 80, TotalOreKwh: 300},
	}
	got := selfConsumptionTerminalPrice(prices, 60, 6)
	if math.Abs(got-300) > 1e-9 {
		t.Fatalf("terminal price = %f, want 300 (mean import)", got)
	}
}

func TestSelfConsumptionTerminalPriceIgnoresExportRate(t *testing.T) {
	// Even when export rate (spot+bonus−fee) exceeds retail, the
	// terminal value of stored SoC is still mean import — self-consumption
	// mode never sells stored energy, so the export side is moot.
	prices := []state.PricePoint{{SpotOreKwh: 500, TotalOreKwh: 100}}
	got := selfConsumptionTerminalPrice(prices, 0, 0)
	if math.Abs(got-100) > 1e-9 {
		t.Fatalf("terminal price = %f, want 100 (mean import) regardless of export rate", got)
	}
}

func TestSelfConsumptionTerminalPriceEmpty(t *testing.T) {
	got := selfConsumptionTerminalPrice(nil, 0, 0)
	if got != 0 {
		t.Fatalf("terminal price = %f, want 0", got)
	}
}

// End-to-end proof: with the new self-consumption terminal valuation, a
// battery that's ≥50% full WILL discharge to cover load instead of
// choosing "idle — import to cover load". Regression test for the exact
// bug we saw on homelab-rpi (bat_w=0 on every slot with SoC=84%).
func TestOptimizeSelfConsumptionDischargesWithSpreadTerminalPrice(t *testing.T) {
	// 4-slot horizon, PV < load in every slot so battery has work to do.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 7200 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 10800 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
	}

	// Build PricePoints identical to the slots and compute the
	// mode-appropriate terminal price. Mirrors what service.replan does.
	prices := []state.PricePoint{
		{SpotOreKwh: 80, TotalOreKwh: 300}, {SpotOreKwh: 80, TotalOreKwh: 300},
		{SpotOreKwh: 80, TotalOreKwh: 300}, {SpotOreKwh: 80, TotalOreKwh: 300},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 80
	p.ExportBonusOreKwh = 60
	p.ExportFeeOreKwh = 6
	p.TerminalSoCPrice = selfConsumptionTerminalPrice(prices, 60, 6)

	plan := Optimize(slots, p)
	var discharging int
	for _, a := range plan.Actions {
		if a.BatteryW < -1e-6 {
			discharging++
		}
		if a.BatteryW > 1e-6 {
			t.Errorf("slot at %d charging %.0fW with no PV surplus", a.SlotStartMs, a.BatteryW)
		}
	}
	if discharging == 0 {
		t.Fatalf("expected at least one discharging slot with SoC=80%% and load>PV, got %+v", plan.Actions)
	}
}

// ---- online battery fleet snapshot ----

func TestOnlineFleetParamsUsesCapacityWeightedOnlineSoC(t *testing.T) {
	tel := telemetry.NewStore()
	socA := 0.20
	socB := 0.80
	socOffline := 0.95
	tel.Update("a", telemetry.DerBattery, 0, &socA, nil)
	tel.DriverHealthMut("a").RecordSuccess()
	tel.Update("b", telemetry.DerBattery, 0, &socB, nil)
	tel.DriverHealthMut("b").RecordSuccess()
	tel.Update("offline", telemetry.DerBattery, 0, &socOffline, nil)
	tel.DriverHealthMut("offline").SetOffline()

	s := &Service{Tele: tel, FuseMaxW: 6000}
	p, ok := s.onlineFleetParams(Params{InitialSoCPct: 50}, []BatteryFleetMember{
		{Driver: "a", CapacityWh: 10000, MaxChargeW: 3000, MaxDischargeW: 4000},
		{Driver: "b", CapacityWh: 30000, MaxChargeW: 5000, MaxDischargeW: 5000},
		{Driver: "offline", CapacityWh: 50000, MaxChargeW: 9000, MaxDischargeW: 9000},
	})
	if !ok {
		t.Fatal("onlineFleetParams returned ok=false")
	}
	if p.CapacityWh != 40000 {
		t.Fatalf("CapacityWh = %.0f, want 40000", p.CapacityWh)
	}
	// (10 kWh * 20% + 30 kWh * 80%) / 40 kWh = 65%.
	if math.Abs(p.InitialSoCPct-65) > 1e-9 {
		t.Fatalf("InitialSoCPct = %.3f, want 65.000", p.InitialSoCPct)
	}
	if p.MaxChargeW != 6000 {
		t.Fatalf("MaxChargeW = %.0f, want fuse-clamped 6000", p.MaxChargeW)
	}
	if p.MaxDischargeW != 6000 {
		t.Fatalf("MaxDischargeW = %.0f, want fuse-clamped 6000", p.MaxDischargeW)
	}
	if len(p.Storages) != 2 {
		t.Fatalf("len(Storages) = %d, want 2", len(p.Storages))
	}
	if p.Storages[0].ID != "a" || p.Storages[0].InitialEnergyWh != 2000 {
		t.Fatalf("Storages[0] = %+v, want battery a at 2000 Wh", p.Storages[0])
	}
	if p.Storages[1].ID != "b" || p.Storages[1].InitialEnergyWh != 24000 {
		t.Fatalf("Storages[1] = %+v, want battery b at 24000 Wh", p.Storages[1])
	}
	if p.Storages[0].MaxChargeW != 2250 || p.Storages[1].MaxChargeW != 3750 {
		t.Fatalf("storage charge limits = %.0f + %.0f, want fuse-scaled 2250 + 3750",
			p.Storages[0].MaxChargeW, p.Storages[1].MaxChargeW)
	}
	if math.Abs(p.Storages[0].MaxDischargeW-8000.0/3.0) > 1e-9 || math.Abs(p.Storages[1].MaxDischargeW-10000.0/3.0) > 1e-9 {
		t.Fatalf("storage discharge limits = %.3f + %.3f, want proportional 6000 W total",
			p.Storages[0].MaxDischargeW, p.Storages[1].MaxDischargeW)
	}
}

func TestOnlineFleetParamsRequiresOnlineSoCTelemetry(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("no-soc", telemetry.DerBattery, 0, nil, nil)
	tel.DriverHealthMut("no-soc").RecordSuccess()
	s := &Service{Tele: tel}

	_, ok := s.onlineFleetParams(Params{InitialSoCPct: 50}, []BatteryFleetMember{
		{Driver: "no-soc", CapacityWh: 10000, MaxChargeW: 3000, MaxDischargeW: 3000},
		{Driver: "missing", CapacityWh: 10000, MaxChargeW: 3000, MaxDischargeW: 3000},
	})
	if ok {
		t.Fatal("onlineFleetParams ok=true without any online battery SoC")
	}
}

// ---- Edge cases / hardening ----

func TestBuildSlotsEmptyForecast(t *testing.T) {
	ts := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC).UnixMilli()
	slots := buildSlots(
		[]state.PricePoint{{
			SlotTsMs:    ts,
			SlotLenMin:  60,
			SpotOreKwh:  100,
			TotalOreKwh: 200,
		}},
		nil, // empty forecasts
		1500,
		ts,
		nil,
		nil,
		nil,
	)
	if len(slots) != 1 {
		t.Fatalf("expected 1 slot, got %d", len(slots))
	}
	// With no forecast, PVW should be 0 (no panic).
	if slots[0].PVW != 0 {
		t.Errorf("expected PVW=0 with empty forecast, got %f", slots[0].PVW)
	}
	if slots[0].LoadW != 1500 {
		t.Errorf("expected LoadW=1500, got %f", slots[0].LoadW)
	}
}

func TestSelectPlannerPVWBothNaN(t *testing.T) {
	got := selectPlannerPVW(math.NaN(), math.NaN(), false)
	if got != 0 {
		t.Errorf("both NaN should return 0, got %f", got)
	}
}

// Radiation-backed forecast: predicted twin gets a minority vote so an
// under-trained RLS can't dominate. 4000W forecast + 2000W twin with
// PlannerRadiationWeight=0.3 → 0.7*4000 + 0.3*2000 = 3400.
func TestSelectPlannerPVWRadiationBlend(t *testing.T) {
	got := selectPlannerPVW(4000, 2000, true)
	want := 0.7*4000 + 0.3*2000
	if math.Abs(got-want) > 0.01 {
		t.Errorf("radiation blend: got %f, want %f", got, want)
	}
}

// Even a wild twin overshoot gets capped by the 30% weight — 4000W
// forecast + 10000W twin → 0.7*4000 + 0.3*10000 = 5800. Still sane,
// not the full 10000 the cloud-only path would have let through.
func TestSelectPlannerPVWRadiationBlendClampsWildTwin(t *testing.T) {
	got := selectPlannerPVW(4000, 10000, true)
	want := 0.7*4000 + 0.3*10000
	if math.Abs(got-want) > 0.01 {
		t.Errorf("radiation blend clamping: got %f, want %f", got, want)
	}
	// Without radiation backing, the same inputs would let the twin
	// take over completely (cloud-only path, not collapsed).
	if got := selectPlannerPVW(4000, 10000, false); got != 10000 {
		t.Errorf("cloud-only path should pass through twin prediction, got %f", got)
	}
}

// When forecast is radiation-backed but zero (night), the legacy cloud
// path takes over — we don't want to emit 0.3*predicted for a slot
// where the forecast correctly says "no sun".
func TestSelectPlannerPVWRadiationZeroForecastIgnoresBlend(t *testing.T) {
	// Twin predicts 300W at night (probably garbage); radiation says 0.
	// With the guard, we fall through to cloud-only logic: forecast <
	// 200 threshold → use twin. That's the original behaviour and
	// matches "we have no sun, twin is the only signal left".
	got := selectPlannerPVW(0, 300, true)
	if got != 300 {
		t.Errorf("zero-forecast with radiation flag should fall through, got %f", got)
	}
}

// T33 regression: open_meteo predicted 2002 W (solar_wm2=154, cloud=1%) for a
// 13 kW site while the trained RLS twin (NowAnchor-corrected via live telemetry)
// predicted 290 W — actual measured PV was ~290 W.  The old code produced
// 0.7*2002 + 0.3*290 = 1488 W (5× actual).  With the forecast cap, the forecast
// is limited to PlannerForecastCapRatio (3×) × twin before blending:
//
//	cappedForecast = 3 × 290 = 870
//	result         = 0.7×870 + 0.3×290 = 696 W   (2.4× actual — still an overshoot
//	                                                but far better than 5×)
//
// The residual over-prediction is expected and acceptable: the cap only activates
// when the NWP cloud forecast was catastrophically wrong.  On a normal day (forecast
// and twin agree within 3×) the cap is a no-op and accuracy is unchanged.
func TestSelectPlannerPVWForecastCapActivatesOnWildForecast(t *testing.T) {
	// Reproduce T33 inputs (scaled to round numbers).
	forecast := 2002.0
	twin := 290.0 // NowAnchor-corrected RLS twin value

	got := selectPlannerPVW(forecast, twin, true)

	// With the cap at PlannerForecastCapRatio=3: capped = 3*290 = 870.
	cappedForecast := PlannerForecastCapRatio * twin
	want := (1-PlannerRadiationWeight)*cappedForecast + PlannerRadiationWeight*twin
	if math.Abs(got-want) > 0.5 {
		t.Errorf("T33 forecast-cap: got %.1f, want %.1f (capped at %.0fx twin=%g)",
			got, want, PlannerForecastCapRatio, twin)
	}

	// Result must be materially less than the uncapped blend.
	uncapped := (1-PlannerRadiationWeight)*forecast + PlannerRadiationWeight*twin
	if got >= uncapped {
		t.Errorf("capped result %.1f should be less than uncapped %.1f", got, uncapped)
	}
}

// Cap must be a no-op when forecast and twin are within PlannerForecastCapRatio.
// A well-trained twin slightly under-predicting because of orientation or
// soiling should not trigger the cap.
func TestSelectPlannerPVWForecastCapInactiveWhenRatioOK(t *testing.T) {
	forecast := 4000.0
	twin := 2000.0 // 2× — well below cap threshold of 3×

	got := selectPlannerPVW(forecast, twin, true)
	want := (1-PlannerRadiationWeight)*forecast + PlannerRadiationWeight*twin
	if math.Abs(got-want) > 0.01 {
		t.Errorf("cap should be inactive when forecast/twin=2x: got %.2f, want %.2f", got, want)
	}
}

// Cap must not activate when the twin is near-zero (< 50 W):  that means the
// physics night gate fired and the twin result is meaningless — the forecast
// should dominate unchanged.
func TestSelectPlannerPVWForecastCapInactiveWhenTwinNearZero(t *testing.T) {
	// Twin near-zero (e.g. cs < 50 W/m² after physics gate) but forecast
	// still has some radiation signal at twilight.
	forecast := 300.0
	twin := 30.0 // below the 50 W threshold

	got := selectPlannerPVW(forecast, twin, true)
	want := (1-PlannerRadiationWeight)*forecast + PlannerRadiationWeight*twin // no cap
	if math.Abs(got-want) > 0.01 {
		t.Errorf("cap should be inactive when twin < 50 W: got %.2f, want %.2f", got, want)
	}
}

// Strict self_consumption: even with a high terminal price (= mean
// import), the DP must still discharge when battery has headroom
// (SoC > min + 20). This used to be a guardrail documenting the
// OPPOSITE behaviour — that a too-high terminal price blocked
// discharge and we'd just sit and import. The strict-SC bias
// introduced in the planner-logic investigation round inverts it:
// self_consumption now means "use the battery first" regardless of
// the terminal-value arithmetic. That matches the operator intent
// the mode name implies.
func TestOptimizeSelfConsumptionDischargesDespiteHighTerminal(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
		{StartMs: 3600 * 1000, LenMin: 60, PriceOre: 300, SpotOre: 80, LoadW: 3000, PVW: -500, Confidence: 1},
	}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 80
	p.TerminalSoCPrice = 300 // mean import price — pre-strict this would have blocked discharge.

	plan := Optimize(slots, p)
	var anyDischarge bool
	for _, a := range plan.Actions {
		if a.BatteryW < -100 {
			anyDischarge = true
			break
		}
	}
	if !anyDischarge {
		t.Fatalf("strict SC should discharge despite terminal=mean; got actions %+v", plan.Actions)
	}
}

// SlotDirectiveAt returns energy-allocation directive for the slot
// containing now. Verifies that power is converted to energy via the
// slot length, that stale plans return ok=false, and that out-of-window
// queries return ok=false.
func TestSlotDirectiveAt(t *testing.T) {
	// Anchor on real wall clock — SlotDirectiveAt rejects plans older
	// than MaxPlanAge (30 min) via time.Since(GeneratedAtMs), so a
	// hardcoded past timestamp would make this test flaky as soon as
	// the wall clock drifts past the plan's age ceiling.
	now := time.Now().UTC().Truncate(time.Second)
	slotStart := now.Add(-3 * time.Minute) // we're 3 min into a 15-min slot
	slotLenMin := 15

	s := &Service{
		Defaults: Params{Mode: ModeArbitrage},
		last: &Plan{
			GeneratedAtMs: now.Add(-time.Minute).UnixMilli(),
			Actions: []Action{{
				SlotStartMs: slotStart.UnixMilli(),
				SlotLenMin:  slotLenMin,
				BatteryW:    800, // 800 W × 15/60 h = 200 Wh for the slot
				SoCPct:      45.5,
				GridW:       -150, // plan expects 150 W export
			}},
		},
	}

	d, ok := s.SlotDirectiveAt(now)
	if !ok {
		t.Fatal("SlotDirectiveAt returned ok=false, want true")
	}
	if want := 200.0; math.Abs(d.BatteryEnergyWh-want) > 0.01 {
		t.Errorf("BatteryEnergyWh = %f, want %f", d.BatteryEnergyWh, want)
	}
	if !d.SlotStart.Equal(slotStart) {
		t.Errorf("SlotStart = %v, want %v", d.SlotStart, slotStart)
	}
	if want := slotStart.Add(15 * time.Minute); !d.SlotEnd.Equal(want) {
		t.Errorf("SlotEnd = %v, want %v", d.SlotEnd, want)
	}
	if d.SoCTargetPct != 45.5 {
		t.Errorf("SoCTargetPct = %f, want 45.5", d.SoCTargetPct)
	}
	if d.Strategy != ModeArbitrage {
		t.Errorf("Strategy = %v, want arbitrage", d.Strategy)
	}
	// GridW must surface unchanged from the plan action — this is the
	// wiring the control-layer PlannedGridW cap depends on. If it
	// silently breaks, the cap silently never fires.
	if d.GridW != -150 {
		t.Errorf("GridW = %f, want −150 (must propagate from Action.GridW)", d.GridW)
	}
}

// Discharge intent (negative BatteryW) surfaces as negative energy.
func TestSlotDirectiveAtDischarge(t *testing.T) {
	// Use real wall clock — MaxPlanAge (30 min) would reject a hardcoded
	// past timestamp. Same flake as TestSlotDirectiveAt earlier.
	now := time.Now().UTC().Truncate(time.Second)
	s := &Service{
		last: &Plan{
			GeneratedAtMs: now.UnixMilli(),
			Actions: []Action{{
				SlotStartMs: now.UnixMilli(),
				SlotLenMin:  15,
				BatteryW:    -2400, // discharge 600 Wh over 15 min
			}},
		},
	}
	d, ok := s.SlotDirectiveAt(now)
	if !ok {
		t.Fatal("ok=false")
	}
	if want := -600.0; math.Abs(d.BatteryEnergyWh-want) > 0.01 {
		t.Errorf("BatteryEnergyWh = %f, want %f", d.BatteryEnergyWh, want)
	}
}

// A plan older than MaxPlanAge should not surface any directive — the
// control loop falls back to auto_fallback.
func TestSlotDirectiveAtStalePlan(t *testing.T) {
	now := time.Now()
	s := &Service{
		last: &Plan{
			GeneratedAtMs: now.Add(-MaxPlanAge - time.Minute).UnixMilli(),
			Actions: []Action{{
				SlotStartMs: now.UnixMilli(),
				SlotLenMin:  15,
				BatteryW:    800,
			}},
		},
	}
	if _, ok := s.SlotDirectiveAt(now); ok {
		t.Error("SlotDirectiveAt returned ok=true for stale plan, want false")
	}
}

// A query outside any slot's time window should return ok=false.
func TestSlotDirectiveAtOutOfWindow(t *testing.T) {
	slotStart := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)
	s := &Service{
		last: &Plan{
			GeneratedAtMs: slotStart.UnixMilli(),
			Actions: []Action{{
				SlotStartMs: slotStart.UnixMilli(),
				SlotLenMin:  15,
				BatteryW:    800,
			}},
		},
	}
	future := slotStart.Add(30 * time.Minute) // 15 min past slot end
	if _, ok := s.SlotDirectiveAt(future); ok {
		t.Error("SlotDirectiveAt returned ok=true for out-of-window time")
	}
}

// Nil service must not panic.
func TestSlotDirectiveAtNilService(t *testing.T) {
	var s *Service
	if _, ok := s.SlotDirectiveAt(time.Now()); ok {
		t.Error("nil Service returned ok=true")
	}
}
