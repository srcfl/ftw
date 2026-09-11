package sitepower

import (
	"math"
	"testing"
)

func TestGridWFiveTermIdentity(t *testing.T) {
	// load 500, PV −8000, battery soak 5000, EV 4140 → grid 1640.
	got := GridW(500, -8000, 5000, 4140, 0)
	if got != 1640 {
		t.Fatalf("grid = %v, want 1640", got)
	}
	// True combo: leftover 7500, battery grid-buy 10 kW, EV 4140 → 6640.
	got = GridW(500, -8000, 10000, 4140, 0)
	if got != 6640 {
		t.Fatalf("combo grid = %v, want 6640", got)
	}
}

func TestHouseLeftoverW(t *testing.T) {
	if got := HouseLeftoverW(500, -8000); got != 7500 {
		t.Fatalf("leftover = %v, want 7500", got)
	}
	if got := HouseLeftoverW(4000, -1000); got != 0 {
		t.Fatalf("deficit leftover = %v, want 0", got)
	}
	if got := HouseLeftoverW(math.NaN(), -8000); got != 0 {
		t.Fatalf("NaN load leftover = %v, want 0", got)
	}
	if got := HouseLeftoverW(500, math.Inf(-1)); got != 0 {
		t.Fatalf("-Inf PV leftover = %v, want 0", got)
	}
}

func TestSurplusAvailableForEVWSoakVersusGridBuy(t *testing.T) {
	// Soak: leftover 7500, battery 5000 → EV may take 2500, not 7500.
	// Offering 7500 is the #957 live-formula leak (EV+soak cause import,
	// then "meter importing" is misread as a deliberate battery grid-buy).
	if got := SurplusAvailableForEVW(500, -8000, 5000); got != 2500 {
		t.Fatalf("soak leftover for EV = %v, want 2500", got)
	}
	// Grid-buy: battery 10 kW > leftover 7500 → leftover stays available.
	if got := SurplusAvailableForEVW(500, -8000, 10000); got != 7500 {
		t.Fatalf("grid-buy leftover for EV = %v, want 7500", got)
	}
	// Idle battery.
	if got := SurplusAvailableForEVW(500, -8000, 0); got != 7500 {
		t.Fatalf("idle leftover for EV = %v, want 7500", got)
	}
	// Discharging battery does not create leftover (no-battery-to-EV).
	if got := SurplusAvailableForEVW(500, -8000, -2000); got != 7500 {
		t.Fatalf("discharge leftover for EV = %v, want 7500 (discharge is a separate check)", got)
	}
	if got := SurplusAvailableForEVW(4000, -1000, 0); got != 0 {
		t.Fatalf("no leftover → %v", got)
	}
}

func TestSurplusOnlyEVExceedsLeftover(t *testing.T) {
	// Soak + EV 4140 with only 2500 leftover after soak → import, forbidden.
	if !SurplusOnlyEVExceedsLeftover(500, -8000, 5000, 4140) {
		t.Fatal("soak+EV 4140 must count as surplus-only import")
	}
	// Same leftover, EV 2000 fits after 5000 soak.
	if SurplusOnlyEVExceedsLeftover(500, -8000, 5000, 2000) {
		t.Fatal("soak+EV 2000 is leftover, not import")
	}
	// Combo: battery buys grid, EV takes leftover 4140 of 7500 — legal even
	// though the meter imports ~6640. The gridW>50 predicate would reject this.
	if SurplusOnlyEVExceedsLeftover(500, -8000, 10000, 4140) {
		t.Fatal("leftover EV beside battery grid-charge must be allowed")
	}
	if SurplusOnlyEVExceedsLeftover(500, -8000, 10000, 0) {
		t.Fatal("idle EV is not an exceed")
	}
}

func TestGridSignIsNotSurplusOnlyImport(t *testing.T) {
	// Characterises the broken DP predicate (evW>0 && gridW>50). A legal
	// leftover+grid-buy combo has grid 6640 and ev 4140; using the meter
	// sign would reject it. Keep this next to the identity so a planner
	// that copies the old predicate fails this file, not a live site.
	load, pv, bat, ev := 500.0, -8000.0, 10000.0, 4140.0
	grid := GridW(load, pv, bat, ev, 0)
	legacyRejects := ev > 0 && grid > 50
	if !legacyRejects {
		t.Fatal("legacy grid-sign predicate should reject the combo (that's the bug)")
	}
	if SurplusOnlyEVExceedsLeftover(load, pv, bat, ev) {
		t.Fatal("identity must allow the combo the grid-sign predicate rejects")
	}
}

func TestBatteryFeedsEV(t *testing.T) {
	// House needs 500, PV covers it (−8000 leftover). Battery discharging
	// 2000 while EV takes 4140: the discharge cannot be house load.
	if !BatteryFeedsEV(500, -8000, -2000, 4140) {
		t.Fatal("battery discharge into leftover house must count as feeding EV")
	}
	// House needs 4000, PV 1000, battery covers the 3000 residual, EV off.
	if BatteryFeedsEV(4000, -1000, -3000, 0) {
		t.Fatal("covering house load is not feeding EV")
	}
	if BatteryFeedsEV(4000, -1000, -3000, 100) {
		t.Fatal("tiny EV with discharge covering house is not feeding EV")
	}
	// Discharge 5000 into house need 3000 while EV draws 2000 → feeds EV.
	if !BatteryFeedsEV(4000, -1000, -5000, 2000) {
		t.Fatal("excess discharge with EV on must count as feeding EV")
	}
}

func TestFinite(t *testing.T) {
	if !Finite(0) || !Finite(-3) || Finite(math.NaN()) || Finite(math.Inf(1)) {
		t.Fatal("Finite mismatch")
	}
}
