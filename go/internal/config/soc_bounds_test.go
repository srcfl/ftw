package config

import (
	"encoding/json"
	"testing"
)

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

// Settings saved a blank Max SoC as 0. Only soc_max loses it: a soc_min of 0
// and the power limits are real choices.
func TestDropZeroBatteryCeilings(t *testing.T) {
	c := &Config{Batteries: map[string]Battery{
		"blank":    {SoCMin: f64(0), SoCMax: f64(0), MaxChargeW: f64(7500)},
		"negative": {SoCMax: f64(-0.1)},
		"set":      {SoCMax: f64(0.9)},
		"unset":    {SoCMin: f64(0.1)},
	}}

	c.dropZeroBatteryCeilings()

	for name, b := range c.Batteries {
		if name != "set" && b.SoCMax != nil {
			t.Errorf("%s: soc_max = %v, want unset", name, *b.SoCMax)
		}
	}
	if b := c.Batteries["set"]; b.SoCMax == nil || *b.SoCMax != 0.9 {
		t.Errorf("set: soc_max = %v, want 0.9", b.SoCMax)
	}
	if b := c.Batteries["blank"]; b.SoCMin == nil || *b.SoCMin != 0 || b.MaxChargeW == nil || *b.MaxChargeW != 7500 {
		t.Errorf("blank: other limits changed: %+v", b)
	}
	if out := WithBatterySoCBounds([]Driver{{Name: "blank"}}, c.Batteries); out[0].Config["charge_ceil_soc"] != nil {
		t.Errorf("a dropped soc_max still reached the driver: %v", out[0].Config)
	}
}

// A box's settings live in the config database. A zero an older Core stored
// there must load as unset, and the copy being validated must not write into
// the map it shares with the live config.
func TestStoredZeroSoCMaxLoadsAsUnset(t *testing.T) {
	path, _, st := importedLiveSettings(t)
	doc, found, err := st.Configuration()
	if err != nil || !found {
		t.Fatalf("stored configuration: found=%v err=%v", found, err)
	}
	var stored map[string]any
	if err := json.Unmarshal(doc.Document, &stored); err != nil {
		t.Fatal(err)
	}
	stored["config"].(map[string]any)["batteries"] = map[string]any{
		"pixii": map[string]any{"soc_min": 0, "soc_max": 0, "max_charge_w": 0, "max_discharge_w": 0},
	}
	raw, _ := json.Marshal(stored)
	if _, err := st.SaveConfiguration(raw, doc.Revision, nil); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Batteries["pixii"]; b.SoCMax != nil || b.SoCMin == nil || b.MaxChargeW == nil {
		t.Fatalf("loaded %+v, want soc_max unset and the other limits kept", b)
	}

	live := Config{Batteries: map[string]Battery{"pixii": {SoCMax: f64(0)}}}
	next := live
	next.dropZeroBatteryCeilings()
	if live.Batteries["pixii"].SoCMax == nil || next.Batteries["pixii"].SoCMax != nil {
		t.Fatalf("live %+v, copy %+v: want only the copy changed", live.Batteries, next.Batteries)
	}
}
