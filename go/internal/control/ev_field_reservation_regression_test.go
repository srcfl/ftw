package control

import (
	"github.com/srcfl/ftw/go/internal/telemetry"
	"math"
	"testing"
)

// Reconstructed clamp input. Actual internal hardware command logs were not captured.
func TestFieldReservationUnrealized15A(t *testing.T) {
	store, state := evSplitFixture(t, 12987, []float64{23.34, 16.92, 15.68}, 10350, "3p")
	sent(state, "ev", 10350, 10350)
	sent(state, "first", 6000, 6000)
	store.Update("car", telemetry.DerEV, 4175, nil, nil)
	store.RecordDriverSuccess("car")
	out := clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: 6000}}, store, state, 17250)
	battery := targetW(out, "first")
	projected := 23.34 + (10350.0-4175)/(3*230) - (6000-battery)/(3*230)
	t.Logf("battery_target_w=%.2f projected_worst_phase_a=%.3f", battery, projected)
	if math.IsNaN(battery) || projected > 24.0001 {
		t.Fatalf("unrealized offer is not reserved: battery %.2f W, projected %.3f A", battery, projected)
	}
}

func TestFieldReservationRealizedOfferPreservesBattery(t *testing.T) {
	store, state := evSplitFixture(t, 12987, []float64{23.34, 16.92, 15.68}, 10350, "3p")
	sent(state, "ev", 10350, 10350)
	sent(state, "first", 6000, 6000)
	store.Update("car", telemetry.DerEV, 10350, nil, nil)
	store.RecordDriverSuccess("car")
	got := targetW(clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: 6000}}, store, state, 17250), "first")
	if got != 6000 {
		t.Fatalf("realized offer unnecessarily reduced battery: %.2f", got)
	}
}

func TestFieldReservationMissingDeliveryKeepsFullReserve(t *testing.T) {
	store, state := evSplitFixture(t, 12987, []float64{23.34, 16.92, 15.68}, 10350, "3p")
	sent(state, "ev", 10350, 10350)
	sent(state, "first", 6000, 6000)
	store.Remove("car")
	got := targetW(clampEVBatteryHeadroom([]DispatchTarget{{Driver: "first", TargetW: 6000}}, store, state, 17250), "first")
	if got != 0 {
		t.Fatalf("missing EV measurement credited delivery: battery %.2f", got)
	}
}
