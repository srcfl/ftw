package ha

import (
	"encoding/json"
	"testing"

	"github.com/srcfl/ftw/go/internal/control"
)

// TestModeSelectOptionsMatchAllModes is the wiring guard for the original
// "Invalid option for select" bug: the HA mode `select` options must equal
// control.AllModes() stringified, in order. The state topic publishes
// string(ctrl.Mode), so any mode AllModes admits but the options omit makes
// HA reject the published state. modes_test.go in control guards the enum
// itself; this guards that the bridge actually derives its options from it
// (so a future edit reintroducing a hardcoded literal list fails CI).
func TestModeSelectOptionsMatchAllModes(t *testing.T) {
	got := modeSelectOptions()
	want := control.AllModes()

	if len(got) != len(want) {
		t.Fatalf("modeSelectOptions() has %d options, AllModes() has %d: %v vs %v",
			len(got), len(want), got, want)
	}
	for i, m := range want {
		if got[i] != string(m) {
			t.Errorf("option[%d] = %q, want %q (order must match AllModes)", i, got[i], string(m))
		}
	}
}

// TestModeSelectOptionsCoverEveryValidMode asserts the options are a superset
// of every mode IsValidMode accepts — the property that actually prevents the
// HA rejection, independent of ordering.
func TestModeSelectOptionsCoverEveryValidMode(t *testing.T) {
	opts := make(map[string]bool)
	for _, o := range modeSelectOptions() {
		opts[o] = true
	}
	for _, m := range control.AllModes() {
		if !control.IsValidMode(m) {
			t.Errorf("AllModes contains %q but IsValidMode rejects it", m)
		}
		if !opts[string(m)] {
			t.Errorf("mode %q is valid/published but missing from the HA select options", m)
		}
	}
}

// TestModeSelectOptionsMarshalAsStringArray locks the on-wire shape: the
// discovery `options` field must serialize as a flat JSON array of strings,
// which is what HA's select schema expects.
func TestModeSelectOptionsMarshalAsStringArray(t *testing.T) {
	data, err := json.Marshal(map[string]any{"options": modeSelectOptions()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Options []string `json:"options"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("options field is not a []string: %v (%s)", err, data)
	}
	if len(decoded.Options) != len(control.AllModes()) {
		t.Errorf("round-tripped %d options, want %d", len(decoded.Options), len(control.AllModes()))
	}
}
