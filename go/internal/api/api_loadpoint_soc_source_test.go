package api

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The manager reports "completed" when the completion latch pinned the
// estimate to the target. The vehicle decoration used to overwrite that
// with "inferred" on every site without a vehicle driver, so the modal
// could never say why the slider sat where it sat (#1059).
func TestDecorateLoadpointsKeepsCompletedSoCSource(t *testing.T) {
	states := []loadpoint.State{
		{ID: "a", PluggedIn: true, SoCSource: "completed"},
		{ID: "b", PluggedIn: true},
		{ID: "c", PluggedIn: false},
	}
	decorateLoadpointsWithVehicle(states, telemetry.NewStore())
	if states[0].SoCSource != "completed" {
		t.Errorf("a: SoCSource = %q, want completed kept", states[0].SoCSource)
	}
	if states[1].SoCSource != "inferred" {
		t.Errorf("b: SoCSource = %q, want inferred", states[1].SoCSource)
	}
	if states[2].SoCSource != "" {
		t.Errorf("c: unplugged should stay unattributed, got %q", states[2].SoCSource)
	}
}
