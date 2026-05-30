package mpc

import "testing"

// The MPC must cap each slot's export at the tighter of the fuse and the
// operator's max_export_w, so the planner never schedules a battery
// discharge that would over-export and trip the inverter (the recurring
// Ferroamp 0x8030 fault). A pre-existing tighter limit (e.g. a DSO
// curtailment signal) must never be loosened.
func TestClampSlotGridLimitsTightensExportBelowFuse(t *testing.T) {
	slots := []Slot{
		{},                                      // unset → take fuse / export cap
		{Limits: PowerLimits{MaxExportW: 3000}}, // already tighter than the cap
		{Limits: PowerLimits{MaxExportW: 9000}}, // looser than the cap
	}
	clampSlotGridLimits(slots, 11000, 5000) // fuse 11 kW, export cap 5 kW

	if slots[0].Limits.MaxImportW != 11000 {
		t.Fatalf("slot0 import: want 11000 got %.0f", slots[0].Limits.MaxImportW)
	}
	if slots[0].Limits.MaxExportW != 5000 {
		t.Fatalf("slot0 export: want 5000 (export cap) got %.0f", slots[0].Limits.MaxExportW)
	}
	if slots[1].Limits.MaxExportW != 3000 {
		t.Fatalf("slot1 export: tighter pre-set must survive, got %.0f", slots[1].Limits.MaxExportW)
	}
	if slots[2].Limits.MaxExportW != 5000 {
		t.Fatalf("slot2 export: looser pre-set must be capped to 5000, got %.0f", slots[2].Limits.MaxExportW)
	}
}

// With no export cap configured (0), export is bounded only by the fuse —
// back-compat with the pre-existing per-slot fuse plumbing.
func TestClampSlotGridLimitsNoExportCapUsesFuse(t *testing.T) {
	slots := []Slot{{}}
	clampSlotGridLimits(slots, 11000, 0)
	if slots[0].Limits.MaxImportW != 11000 {
		t.Fatalf("import should be fuse (11000), got %.0f", slots[0].Limits.MaxImportW)
	}
	if slots[0].Limits.MaxExportW != 11000 {
		t.Fatalf("export with no cap should be fuse (11000), got %.0f", slots[0].Limits.MaxExportW)
	}
}
