package mpc

import (
	"math"
	"testing"
)

func TestSanitizeLoadW(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want float64 }{
		{-50, 0}, {math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0}, {0, 0}, {700, 700}, {50000, 50000},
	} {
		if got := sanitizeLoadW(tc.in); got != tc.want {
			t.Fatalf("sanitizeLoadW(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeSlotAndPlanLoads(t *testing.T) {
	t.Parallel()
	slots := []Slot{{LoadW: 50000}, {LoadW: 700}, {LoadW: -10}, {LoadW: math.NaN()}}
	sanitizeSlotLoads(slots)
	for i, want := range []float64{50000, 700, 0, 0} {
		if slots[i].LoadW != want {
			t.Fatalf("slot %d load = %v, want %v", i, slots[i].LoadW, want)
		}
	}
	plan := &Plan{Actions: []Action{{LoadW: -1}, {LoadW: 800}}}
	sanitizePlanLoads(plan)
	if plan.Actions[0].LoadW != 0 || plan.Actions[1].LoadW != 800 {
		t.Fatalf("plan loads = %v, %v", plan.Actions[0].LoadW, plan.Actions[1].LoadW)
	}
	sanitizePlanLoads(nil)
}
