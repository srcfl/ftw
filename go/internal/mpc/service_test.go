package mpc

import (
	"math"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
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
	)
	if len(slots) != 1 {
		t.Fatalf("got %d slots, want 1", len(slots))
	}
	if got := slots[0].PVW; math.Abs(got+twinPV) > 1e-6 {
		t.Fatalf("slot PVW = %f, want %f", got, -twinPV)
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

func TestSelfConsumptionTerminalPriceIsImportMinusExport(t *testing.T) {
	// Retail 300 öre/kWh, spot 80 öre/kWh, bonus 60, fee 6.
	// Per slot: export rate = 80 + 60 − 6 = 134. Spread = 300 − 134 = 166.
	prices := []state.PricePoint{
		{SpotOreKwh: 80, TotalOreKwh: 300},
		{SpotOreKwh: 80, TotalOreKwh: 300},
	}
	got := selfConsumptionTerminalPrice(prices, 60, 6)
	if math.Abs(got-166) > 1e-9 {
		t.Fatalf("terminal price = %f, want 166", got)
	}
}

func TestSelfConsumptionTerminalPriceClampsToZero(t *testing.T) {
	// Export rate (spot+bonus−fee) > retail → spread would be negative.
	// Must floor at 0 so we never actively credit draining the battery.
	prices := []state.PricePoint{{SpotOreKwh: 500, TotalOreKwh: 100}}
	got := selfConsumptionTerminalPrice(prices, 0, 0)
	if got != 0 {
		t.Fatalf("terminal price = %f, want 0", got)
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
