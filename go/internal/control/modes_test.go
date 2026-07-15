package control

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/mpc"
)

// TestPlannerMPCModeMapping locks the single-source-of-truth control.Mode →
// mpc.Mode mapping that the API setter, the HA command callback, and the
// startup mode-restore all share. A wrong or missing arm here would push the
// planner into the wrong economic strategy on every one of those paths.
func TestPlannerMPCModeMapping(t *testing.T) {
	cases := []struct {
		in   Mode
		want mpc.Mode
	}{
		{ModePlannerSelf, mpc.ModeSelfConsumption},
		{ModePlannerCheap, mpc.ModeCheapCharge},
		{ModePlannerPassiveArbitrage, mpc.ModePassiveArbitrage},
		{ModePlannerArbitrage, mpc.ModeArbitrage},
	}
	for _, c := range cases {
		got, ok := PlannerMPCMode(c.in)
		if !ok {
			t.Errorf("PlannerMPCMode(%q) ok=false, want true", c.in)
		}
		if got != c.want {
			t.Errorf("PlannerMPCMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Every planner mode must map — guards against a new planner_* mode being
	// added to the enum but forgotten in the mapping (it would return ok=false
	// and silently skip MPC propagation).
	for _, m := range AllModes() {
		if !m.IsPlannerMode() {
			continue
		}
		if _, ok := PlannerMPCMode(m); !ok {
			t.Errorf("planner mode %q has no mpc.Mode mapping", m)
		}
	}
	// Non-planner modes must report ok=false (zero-value mpc.Mode skipped).
	for _, m := range []Mode{ModeIdle, ModeSelfConsumption, ModePeakShaving, ModeCharge, ModePriority, ModeWeighted, "garbage"} {
		if mm, ok := PlannerMPCMode(m); ok {
			t.Errorf("PlannerMPCMode(%q) = (%q, true), want ok=false", m, mm)
		}
	}
}

// TestAllModesCoversPlannerModes is the regression guard for the Home
// Assistant "Invalid option for select" bug: the mode state topic can emit
// any planner_* mode (they are the default UI choice), and the HA discovery
// `select` options derive from AllModes. If a planner mode ever drops out of
// AllModes, HA rejects the published state again. See go/internal/ha/bridge.go.
func TestAllModesCoversPlannerModes(t *testing.T) {
	want := []Mode{
		ModeIdle, ModeSelfConsumption, ModePeakShaving,
		ModeCharge, ModePriority, ModeWeighted,
		ModePlannerSelf, ModePlannerCheap,
		ModePlannerPassiveArbitrage, ModePlannerArbitrage,
	}
	got := AllModes()
	if len(got) != len(want) {
		t.Fatalf("AllModes() returned %d modes, want %d: %v", len(got), len(want), got)
	}
	for i, m := range want {
		if got[i] != m {
			t.Errorf("AllModes()[%d] = %q, want %q", i, got[i], m)
		}
	}
}

// TestIsValidModeAgreesWithAllModes locks the validator to the canonical
// list so the API mode setter and the HA bridge can't drift from each other.
func TestIsValidModeAgreesWithAllModes(t *testing.T) {
	for _, m := range AllModes() {
		if !IsValidMode(m) {
			t.Errorf("IsValidMode(%q) = false, want true (mode is in AllModes)", m)
		}
	}
	for _, bad := range []Mode{"", "planner", "self", "arbitrage", "PLANNER_ARBITRAGE"} {
		if IsValidMode(bad) {
			t.Errorf("IsValidMode(%q) = true, want false", bad)
		}
	}
}

// TestEveryPlannerModeIsValid guards the specific failure from the field
// report: planner_arbitrage published as state must be a recognized mode.
func TestEveryPlannerModeIsValid(t *testing.T) {
	for _, m := range AllModes() {
		if !m.IsPlannerMode() {
			continue
		}
		if !IsValidMode(m) {
			t.Errorf("planner mode %q is not valid per IsValidMode", m)
		}
	}
}
