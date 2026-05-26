package loadpoint

import "testing"

// PhaseFor + FilterStepsByPhase are exported reference implementations
// of the phase-decision logic. The Easee Lua driver implements the
// same rule locally — these tests pin the contract so any future
// Go-side driver (or refactor) stays consistent with what the Lua
// driver does.

func TestPhaseForLockedModes(t *testing.T) {
	if got := PhaseFor("1p", 5000, 3680); got != 1 {
		t.Errorf("PhaseFor(\"1p\", high) = %d, want 1 (locked)", got)
	}
	if got := PhaseFor("3p", 100, 3680); got != 3 {
		t.Errorf("PhaseFor(\"3p\", low) = %d, want 3 (locked)", got)
	}
}

func TestPhaseForEmptyDefaultsToThree(t *testing.T) {
	// Backward compat: pre-switching configs leave phase_mode unset.
	// The whole fleet has been on 3Φ, so empty must NOT silently
	// flip them to 1Φ on a low wantW.
	if got := PhaseFor("", 1000, 3680); got != 3 {
		t.Errorf("PhaseFor(\"\", 1000) = %d, want 3", got)
	}
}

func TestPhaseForAutoBelowSplit(t *testing.T) {
	if got := PhaseFor("auto", 2000, 3680); got != 1 {
		t.Errorf("PhaseFor auto, 2000 < 3680 = %d, want 1", got)
	}
}

func TestPhaseForAutoAtOrAboveSplit(t *testing.T) {
	if got := PhaseFor("auto", 3680, 3680); got != 3 {
		t.Errorf("PhaseFor auto, 3680 == split = %d, want 3", got)
	}
	if got := PhaseFor("auto", 6000, 3680); got != 3 {
		t.Errorf("PhaseFor auto, 6000 > 3680 = %d, want 3", got)
	}
}

func TestPhaseForAutoNonStandardVoltage(t *testing.T) {
	// 240 V × 16 A = 3840 split; wantW=3700 below that → 1Φ.
	if got := PhaseFor("auto", 3700, 3840); got != 1 {
		t.Errorf("PhaseFor auto at 240V split, 3700 = %d, want 1", got)
	}
	if got := PhaseFor("auto", 3900, 3840); got != 3 {
		t.Errorf("PhaseFor auto at 240V split, 3900 = %d, want 3", got)
	}
}

func TestFilterStepsByPhaseSinglePhase(t *testing.T) {
	steps := []float64{0, 1380, 2300, 4140, 7400, 11000}
	got := FilterStepsByPhase(steps, 1, 3680)
	want := []float64{0, 1380, 2300}
	if !floatSliceEq(got, want) {
		t.Errorf("FilterStepsByPhase(1Φ, split=3680) = %v, want %v", got, want)
	}
}

func TestFilterStepsByPhaseThreePhase(t *testing.T) {
	steps := []float64{0, 1380, 2300, 4140, 7400, 11000}
	got := FilterStepsByPhase(steps, 3, 3680)
	want := []float64{0, 4140, 7400, 11000}
	if !floatSliceEq(got, want) {
		t.Errorf("FilterStepsByPhase(3Φ, split=3680) = %v, want %v", got, want)
	}
}

func TestFilterStepsByPhaseHonorsCustomSplit(t *testing.T) {
	// Operator override: split at 4500 W → 1Φ-eligible includes 4140.
	steps := []float64{0, 1380, 2300, 4140, 7400, 11000}
	got := FilterStepsByPhase(steps, 1, 4500)
	want := []float64{0, 1380, 2300, 4140}
	if !floatSliceEq(got, want) {
		t.Errorf("FilterStepsByPhase(1Φ, split=4500) = %v, want %v", got, want)
	}
}

func TestFilterStepsByPhaseEmptyInput(t *testing.T) {
	if got := FilterStepsByPhase(nil, 1, 3680); got != nil {
		t.Errorf("FilterStepsByPhase(nil) = %v, want nil", got)
	}
	if got := FilterStepsByPhase([]float64{}, 1, 3680); got != nil {
		t.Errorf("FilterStepsByPhase(empty) = %v, want nil", got)
	}
}

func floatSliceEq(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
