package control

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A site exporting 8 kW via battery discharge, with max_export_w = 5 kW set
// below the fuse: the fuse guard must scale the discharge back so predicted
// export lands at the export ceiling, not at the (looser) fuse limit.
// Models the recurring Ferroamp 0x8030 fault: ~8 kW sustained midday export
// tripped the EnergyHub even though the 16 A fuse (~11 kW) was never hit.
func TestExportCeilingScalesBackDischarge(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, -8000, nil, nil) // exporting 8 kW
	soc := 0.5
	store.Update("bat", telemetry.DerBattery, -8000, &soc, nil) // discharging 8 kW

	state := NewState(0, 50, "meter")
	state.MaxExportW = 5000

	targets := []DispatchTarget{{Driver: "bat", TargetW: -8000}}
	out := applyFuseGuard(targets, store, state, 11040) // fuse would allow 8 kW

	if len(out) != 1 {
		t.Fatalf("expected 1 target, got %d", len(out))
	}
	// predicted = grid(-8000) - bat(-8000) + target(-8000) = -8000
	// exportOverage = -5000 - (-8000) = 3000 → discharge 8000 → 5000
	if got := out[0].TargetW; got < -5050 || got > -4950 {
		t.Fatalf("discharge should be scaled back to ~-5000 W (export ceiling), got %.1f", got)
	}
	if !out[0].Clamped {
		t.Fatalf("target should be marked Clamped after export-ceiling scaling")
	}
}

// With max_export_w unset (0), the export ceiling is the fuse alone, so an
// 8 kW export under an 11 kW fuse is left untouched — back-compat guard so
// the new knob is strictly opt-in.
func TestExportCeilingDisabledExportsUpToFuse(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, -8000, nil, nil)
	soc := 0.5
	store.Update("bat", telemetry.DerBattery, -8000, &soc, nil)

	state := NewState(0, 50, "meter") // MaxExportW left 0
	targets := []DispatchTarget{{Driver: "bat", TargetW: -8000}}
	out := applyFuseGuard(targets, store, state, 11040)

	if got := out[0].TargetW; got != -8000 {
		t.Fatalf("export under fuse with no ceiling should be untouched, got %.1f", got)
	}
	if out[0].Clamped {
		t.Fatalf("target should not be Clamped when within fuse and no export ceiling")
	}
}

// Ceiling math: disabled → fuse, tighter config → config, looser config →
// fuse still binds (operator can't raise export above the breaker).
func TestEffectiveExportCeilingW(t *testing.T) {
	st := NewState(0, 50, "meter")
	const fuse = 11040.0

	if got := st.effectiveExportCeilingW(fuse); got != fuse {
		t.Fatalf("disabled (MaxExportW=0): want %.0f got %.0f", fuse, got)
	}
	st.MaxExportW = 5000
	if got := st.effectiveExportCeilingW(fuse); got != 5000 {
		t.Fatalf("tighter than fuse: want 5000 got %.0f", got)
	}
	st.MaxExportW = 20000
	if got := st.effectiveExportCeilingW(fuse); got != fuse {
		t.Fatalf("looser than fuse: fuse must still bind, want %.0f got %.0f", fuse, got)
	}
}
