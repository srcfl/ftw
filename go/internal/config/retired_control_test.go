package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

// An operator who set use_energy_dispatch: false before v0.27 chose the legacy
// dispatch path on purpose. Removing the key must not flip that site to the
// energy path, and the old key kept winning over legacy_dispatch when both were
// set.
func TestUseEnergyDispatchLoadsAsLegacyDispatch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		planner    string
		wantLegacy bool
		wantNotice bool
	}{
		{"false picks legacy", "  use_energy_dispatch: false\n", true, true},
		{"true overrides legacy_dispatch", "  legacy_dispatch: true\n  use_energy_dispatch: true\n", false, true},
		{"unset leaves legacy_dispatch alone", "  legacy_dispatch: true\n", true, false},
		{"neither set runs the energy path", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(minimalYAML+"planner:\n  enabled: true\n"+tc.planner), "/tmp")
			if err != nil {
				t.Fatal(err)
			}
			if c.Planner.LegacyDispatch != tc.wantLegacy {
				t.Fatalf("legacy_dispatch = %v, want %v", c.Planner.LegacyDispatch, tc.wantLegacy)
			}
			if got := len(c.Retired) == 1 && strings.Contains(c.Retired[0], "use_energy_dispatch"); got != tc.wantNotice {
				t.Fatalf("retired notices = %q, want a use_energy_dispatch notice: %v", c.Retired, tc.wantNotice)
			}
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "use_energy_dispatch") {
				t.Fatalf("the removed key survives loading: %s", raw)
			}
		})
	}
}

// Settings live in state.db after first boot. A stored use_energy_dispatch
// must keep steering the site through legacy_dispatch, and both it and the
// never-read battery weight must leave the stored document.
func TestStoredRetiredControlSettingsMigrateAndAreDeleted(t *testing.T) {
	dir := t.TempDir()
	path, database := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state.db")
	if err := os.WriteFile(path, []byte(minimalYAML), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := InitializeStorage(path, database, cfg, st); err != nil {
		t.Fatal(err)
	}
	// Settings saved by a Core that still had both keys.
	current, _, err := st.Configuration()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(current.Document, &doc); err != nil {
		t.Fatal(err)
	}
	stored := doc["config"].(map[string]any)
	stored["planner"] = map[string]any{"enabled": true, "use_energy_dispatch": false}
	stored["batteries"] = map[string]any{"ferroamp": map[string]any{"soc_min": 0.1, "weight": 2.0}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveConfiguration(raw, current.Revision, nil); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("stored retired control settings stopped startup: %v", err)
	}
	if loaded.Planner == nil || !loaded.Planner.LegacyDispatch {
		t.Fatalf("planner = %+v; the stored legacy choice no longer steers the site", loaded.Planner)
	}
	if len(loaded.Retired) != 1 {
		t.Fatalf("retired notices = %q; want the use_energy_dispatch notice", loaded.Retired)
	}
	if _, err := InitializeStorage(path, database, loaded, st); err != nil {
		t.Fatalf("startup storage check after migrating retired settings: %v", err)
	}

	removed, err := DropRetiredSettings(st, path, loaded)
	if err != nil || len(removed) != 2 {
		t.Fatalf("DropRetiredSettings = %v, %v; want use_energy_dispatch and the battery weight", removed, err)
	}
	after, _, err := st.Configuration()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after.Document), "use_energy_dispatch") ||
		strings.Contains(string(after.Document), `"weight"`) {
		t.Fatalf("stored settings still carry removed control settings: %s", after.Document)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Planner.LegacyDispatch {
		t.Fatal("the legacy choice was lost when the old key was deleted")
	}
	if soc := reloaded.Batteries["ferroamp"].SoCMin; soc == nil || *soc != 0.1 {
		t.Fatalf("battery soc_min = %v; deleting the weight lost the battery's other settings", soc)
	}
	if len(reloaded.Retired) != 0 {
		t.Fatalf("retired notices after cleanup = %q", reloaded.Retired)
	}
	if removed, err := DropRetiredSettings(st, path, reloaded); err != nil || len(removed) != 0 {
		t.Fatalf("second DropRetiredSettings = %v, %v; want nothing", removed, err)
	}
}
