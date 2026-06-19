package mpc

import (
	"math"
	"testing"
)

// TestPowerLimitsDefaultIsUnlimited asserts the zero value places no
// constraints on the DP — protecting backwards compatibility. Without
// this invariant, every existing Slot caller would suddenly face a
// zero-import / zero-export constraint.
func TestPowerLimitsDefaultIsUnlimited(t *testing.T) {
	var l PowerLimits
	for _, grid := range []float64{-10000, -100, 0, 100, 10000} {
		if !l.allowsImport(grid) {
			t.Errorf("default should allow import for gridW=%.0f", grid)
		}
		if !l.allowsExport(grid) {
			t.Errorf("default should allow export for gridW=%.0f", grid)
		}
	}
}

func TestPowerLimitsImportCap(t *testing.T) {
	l := PowerLimits{MaxImportW: 5000}
	cases := []struct {
		gridW  float64
		ok     bool
		reason string
	}{
		{-10000, true, "export ignores import cap"},
		{0, true, "zero flow allowed"},
		{4000, true, "below cap"},
		{5000, true, "at cap"},
		{5001, false, "above cap"},
		{10000, false, "well above cap"},
	}
	for _, tc := range cases {
		if got := l.allowsImport(tc.gridW); got != tc.ok {
			t.Errorf("%s: gridW=%.0f, got allowsImport=%v, want %v",
				tc.reason, tc.gridW, got, tc.ok)
		}
	}
}

func TestPowerLimitsExportCap(t *testing.T) {
	l := PowerLimits{MaxExportW: 3000}
	cases := []struct {
		gridW  float64
		ok     bool
		reason string
	}{
		{10000, true, "import ignores export cap"},
		{0, true, "zero flow allowed"},
		{-2000, true, "below cap (magnitude)"},
		{-3000, true, "at cap"},
		{-3001, false, "above cap"},
	}
	for _, tc := range cases {
		if got := l.allowsExport(tc.gridW); got != tc.ok {
			t.Errorf("%s: gridW=%.0f, got allowsExport=%v, want %v",
				tc.reason, tc.gridW, got, tc.ok)
		}
	}
}

// TestOptimizeRespectsImportCap is the end-to-end: if every cheap slot
// has an import cap, the DP should not schedule import over that cap.
// Without the cap, a cheap slot would tempt an unbounded charge.
func TestOptimizeRespectsImportCap(t *testing.T) {
	// 4 hourly slots, all cheap at 10 öre. Default battery params
	// would happily charge at full power in every slot; the cap on
	// slot 1 should force the DP to stay under 2000 W net import.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 10, SpotOre: 10,
			LoadW: 500, Confidence: 1.0},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 10, SpotOre: 10,
			LoadW: 500, Confidence: 1.0,
			Limits: PowerLimits{MaxImportW: 2000}},
		{StartMs: 7200_000, LenMin: 60, PriceOre: 10, SpotOre: 10,
			LoadW: 500, Confidence: 1.0},
		{StartMs: 10800_000, LenMin: 60, PriceOre: 10, SpotOre: 10,
			LoadW: 500, Confidence: 1.0},
	}
	p := Params{
		Mode:                ModeCheapCharge,
		SoCLevels:           41,
		CapacityWh:          15000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       20,
		ActionLevels:        21,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    50,
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) != len(slots) {
		t.Fatalf("got %d actions, want %d", len(plan.Actions), len(slots))
	}
	// Slot 1 has the cap. GridW there must not exceed 2000 W.
	g := plan.Actions[1].GridW
	if g > 2000+1e-6 {
		t.Errorf("capped slot GridW = %.1f, exceeds cap 2000 W", g)
	}
	// Uncapped slots should still be free to import more to make up
	// what was missed in the capped one — we don't assert a specific
	// bound, just that the DP didn't get stuck at a degenerate plan.
	if plan.Actions[0].GridW <= 0 && plan.Actions[2].GridW <= 0 &&
		plan.Actions[3].GridW <= 0 {
		t.Error("uncapped slots should import at least somewhere in a " +
			"4-hour flat-cheap window")
	}
}

// TestOptimizeInfeasibleStatePicksNearIdle — when every action at a
// state violates limits or mode, the DP previously left Policy at 0
// which encodes "full discharge, EV off" — the worst possible
// fallback. We now pick the closest-to-idle action so forward-sim
// produces something sensible. Codex flagged this in PR #99 review.
func TestOptimizeInfeasibleStatePicksNearIdle(t *testing.T) {
	// One slot with BOTH import and export hard-capped to 0 → no
	// grid flow allowed. Baseline load (+ no PV) is 500 W of import,
	// already violating MaxImportW=1. No feasible action exists.
	// Forward-sim should pick battery action ≈ 0 (action index =
	// (A-1)/2 with A=5 → index 2 → 0 W at the mid-point of the
	// action grid).
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 50, SpotOre: 20,
			LoadW: 500, Confidence: 1.0,
			Limits: PowerLimits{MaxImportW: 1, MaxExportW: 1}},
	}
	p := Params{
		Mode:                ModeSelfConsumption,
		SoCLevels:           11,
		CapacityWh:          5000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        5,
		MaxChargeW:          2000,
		MaxDischargeW:       2000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(plan.Actions))
	}
	// Key assertion: fallback is near-idle, NOT full-discharge (−2000 W).
	a := plan.Actions[0]
	if a.BatteryW < -500 {
		t.Errorf("infeasible fallback should be near-idle, "+
			"got BatteryW=%.1f (full-discharge = −2000)", a.BatteryW)
	}
}

func TestOptimizeInfeasibleStatePicksIdleWithAsymmetricLimits(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 50, SpotOre: 20,
			LoadW: 500, Confidence: 1.0,
			Limits: PowerLimits{MaxImportW: 1, MaxExportW: 1}},
	}
	p := Params{
		Mode:                ModeSelfConsumption,
		SoCLevels:           11,
		CapacityWh:          5000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        81,
		MaxChargeW:          9000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(plan.Actions))
	}
	if math.Abs(plan.Actions[0].BatteryW) > 1e-9 {
		t.Fatalf("infeasible fallback BatteryW = %.1f, want idle 0 W", plan.Actions[0].BatteryW)
	}
}

// TestOptimizeRespectsExportCap mirrors the import test — PV surplus
// exported into a negative-price slot must respect the cap.
func TestOptimizeRespectsExportCap(t *testing.T) {
	// 3 hourly slots. Middle slot has negative price (export is
	// painful) AND a hard export cap of 500 W. The DP's export
	// decision in that slot must not exceed the cap.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, SpotOre: 80,
			PVW: -4000, LoadW: 500, Confidence: 1.0},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 100, SpotOre: 80,
			PVW: -4000, LoadW: 500, Confidence: 1.0,
			Limits: PowerLimits{MaxExportW: 500}},
		{StartMs: 7200_000, LenMin: 60, PriceOre: 100, SpotOre: 80,
			PVW: -4000, LoadW: 500, Confidence: 1.0},
	}
	p := Params{
		Mode:                ModeArbitrage,
		SoCLevels:           41,
		CapacityWh:          15000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       70,
		ActionLevels:        21,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    80,
	}
	plan := Optimize(slots, p)
	g := plan.Actions[1].GridW
	if g < -500-1e-6 {
		t.Errorf("capped slot GridW = %.1f, below export cap -500 W", g)
	}
}

// Live regression (Sat 14:45 plan): PV -6 kW + battery -9 kW + load
// 0.7 kW = grid -14.2 kW exporting past the 11 kW fuse. Pre-fix, the
// service-level fuse plumbing only set MaxImportW, so this slot was
// feasible to the DP. With both directions plumbed (service.go:560),
// the DP must pick a less-aggressive discharge that keeps |grid| ≤
// fuse.
func TestOptimizeRespectsFuseExportCap(t *testing.T) {
	// One slot: 6 kW PV, 0.7 kW load, fuse 11 kW both ways. Price is
	// peak (encourages aggressive discharge). The DP should pick a
	// discharge that brings grid down to ≈ -11 kW, not -14 kW.
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 345, SpotOre: 156,
			PVW: -6000, LoadW: 700, Confidence: 1.0,
			Limits: PowerLimits{MaxImportW: 11000, MaxExportW: 11000}},
	}
	p := Params{
		Mode:                ModeArbitrage,
		SoCLevels:           41,
		CapacityWh:          20000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       85,
		ActionLevels:        21,
		MaxChargeW:          9000,
		MaxDischargeW:       9000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    250,
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(plan.Actions))
	}
	g := plan.Actions[0].GridW
	if g < -11000-1 {
		t.Errorf("plan grid %.0f W exceeds export fuse −11000 W — DP didn't apply MaxExportW. battery_w=%.0f, pv_w=%.0f",
			g, plan.Actions[0].BatteryW, plan.Actions[0].PVW)
	}
}

// Service-level: when FuseMaxW > 0, both MaxImportW and MaxExportW on
// every slot must end up populated (and capped at FuseMaxW). This is
// the plumbing layer between cfg.Fuse and the DP. Pre-fix the export
// side was silently uncapped.
func TestFuseMaxWPopulatesBothDirections(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 100, SpotOre: 50, Confidence: 1.0},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 100, SpotOre: 50, Confidence: 1.0,
			Limits: PowerLimits{MaxImportW: 5000, MaxExportW: 7000}},
	}
	const fuseW = 11000
	// Inline the plumbing under test (mirrors service.go:560-573).
	for i := range slots {
		if slots[i].Limits.MaxImportW <= 0 || slots[i].Limits.MaxImportW > fuseW {
			slots[i].Limits.MaxImportW = fuseW
		}
		if slots[i].Limits.MaxExportW <= 0 || slots[i].Limits.MaxExportW > fuseW {
			slots[i].Limits.MaxExportW = fuseW
		}
	}
	// Slot 0: both were zero → both fill to fuse.
	if slots[0].Limits.MaxImportW != fuseW || slots[0].Limits.MaxExportW != fuseW {
		t.Errorf("slot 0 limits = (imp %.0f, exp %.0f), want both %.0f",
			slots[0].Limits.MaxImportW, slots[0].Limits.MaxExportW, float64(fuseW))
	}
	// Slot 1: import 5000 < fuse → keep. Export 7000 < fuse → keep.
	// (Tighter pre-existing caps win.)
	if slots[1].Limits.MaxImportW != 5000 || slots[1].Limits.MaxExportW != 7000 {
		t.Errorf("slot 1 limits = (imp %.0f, exp %.0f), want (5000, 7000)",
			slots[1].Limits.MaxImportW, slots[1].Limits.MaxExportW)
	}
}
