package config

import "testing"

func f64(v float64) *float64 { return &v }

// soc_max on a battery must reach the matching driver as charge_ceil_soc so a
// driver's built-in ceiling (e.g. Ferroamp's 0.95) can't silently override an
// operator's soc_max: 1.0 and stop charging at ~95%.
func TestWithBatterySoCBoundsInjectsFromBattery(t *testing.T) {
	drivers := []Driver{{Name: "ferroamp"}}
	bats := map[string]Battery{"ferroamp": {SoCMax: f64(1.0), SoCMin: f64(0.1)}}

	out := WithBatterySoCBounds(drivers, bats)

	if got := out[0].Config["charge_ceil_soc"]; got != 1.0 {
		t.Errorf("charge_ceil_soc = %v, want 1.0", got)
	}
	if got := out[0].Config["discharge_floor_soc"]; got != 0.1 {
		t.Errorf("discharge_floor_soc = %v, want 0.1", got)
	}
}

// An explicit value in the driver's own config block is the operator's
// override and must win over the battery default.
func TestWithBatterySoCBoundsOperatorOverrideWins(t *testing.T) {
	drivers := []Driver{{Name: "ferroamp", Config: map[string]any{"charge_ceil_soc": 0.8}}}
	bats := map[string]Battery{"ferroamp": {SoCMax: f64(1.0)}}

	out := WithBatterySoCBounds(drivers, bats)

	if got := out[0].Config["charge_ceil_soc"]; got != 0.8 {
		t.Errorf("explicit charge_ceil_soc overwritten: got %v, want 0.8", got)
	}
}

// YAML null means the driver-level override is unset. It must not suppress the
// battery-level bound and revive the driver's hidden built-in ceiling.
func TestWithBatterySoCBoundsNullOverrideIsUnset(t *testing.T) {
	drivers := []Driver{{Name: "ferroamp", Config: map[string]any{
		"charge_ceil_soc":     nil,
		"discharge_floor_soc": nil,
	}}}
	bats := map[string]Battery{"ferroamp": {SoCMax: f64(1.0), SoCMin: f64(0.1)}}

	out := WithBatterySoCBounds(drivers, bats)

	if got := out[0].Config["charge_ceil_soc"]; got != 1.0 {
		t.Errorf("charge_ceil_soc = %v, want 1.0 for null override", got)
	}
	if got := out[0].Config["discharge_floor_soc"]; got != 0.1 {
		t.Errorf("discharge_floor_soc = %v, want 0.1 for null override", got)
	}
}

// A driver with no matching battery entry (e.g. an EV charger) is untouched.
func TestWithBatterySoCBoundsNoMatchingBattery(t *testing.T) {
	drivers := []Driver{{Name: "ctek"}}

	out := WithBatterySoCBounds(drivers, map[string]Battery{})

	if out[0].Config != nil {
		t.Errorf("config = %v, want nil for driver without a battery entry", out[0].Config)
	}
}

// Nil bounds (battery entry present but soc_min/soc_max unset) inject nothing.
func TestWithBatterySoCBoundsNilBoundsSkipped(t *testing.T) {
	drivers := []Driver{{Name: "ferroamp"}}
	bats := map[string]Battery{"ferroamp": {}}

	out := WithBatterySoCBounds(drivers, bats)

	if out[0].Config != nil {
		t.Errorf("config = %v, want nil when soc bounds are unset", out[0].Config)
	}
}

// The input config maps must never be mutated, or a later config save would
// persist the derived charge_ceil_soc and shadow a future soc_max change.
func TestWithBatterySoCBoundsDoesNotMutateInput(t *testing.T) {
	orig := map[string]any{"foo": "bar"}
	drivers := []Driver{{Name: "ferroamp", Config: orig}}
	bats := map[string]Battery{"ferroamp": {SoCMax: f64(1.0)}}

	out := WithBatterySoCBounds(drivers, bats)

	if _, exists := orig["charge_ceil_soc"]; exists {
		t.Error("input Config map was mutated — persistence pollution risk")
	}
	// ...but the returned copy carries the merged value.
	if got := out[0].Config["charge_ceil_soc"]; got != 1.0 {
		t.Errorf("returned charge_ceil_soc = %v, want 1.0", got)
	}
	if got := out[0].Config["foo"]; got != "bar" {
		t.Errorf("merged copy lost existing key foo: got %v", got)
	}
}
