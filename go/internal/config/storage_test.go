package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestSQLiteImportPreservesConfigAndRuntimeState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	database := filepath.Join(dir, "state.db")
	cfg, err := Parse([]byte(minimalYAML+`
app_link:
  enabled: false
planner:
  enabled: true
  pv_forecast_safety_k: 0
weather:
  latitude: 59
  longitude: 18
  pv_arrays: []
ev_charger:
  provider: easee
  username: test
`), dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runtime := map[string]string{"ev_charger_password": "old-ev-secret", "lan_auth_password": "old-hash", "forecast/site_id": "stable-site", "forecast/energyplan_state_v1": "{ \"opaque\": true }", "loadmodel/state_utc:home": "learned"}
	for k, v := range runtime {
		if err := st.SaveConfig(k, v); err != nil {
			t.Fatal(err)
		}
	}
	cfg.EVCharger.Password = "old-ev-secret"
	before, _ := json.Marshal(cfg)
	cfg, err = InitializeStorage(path, database, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(reloaded)
	if string(before) != string(after) {
		t.Fatalf("config meaning changed across SQLite import\nbefore=%s\nafter=%s", before, after)
	}
	if reloaded.LANPasswordHash != "old-hash" || reloaded.Revision != 1 {
		t.Fatal("missing private credential or revision")
	}
	for k, want := range runtime {
		if got, _ := st.LoadConfig(k); got != want {
			t.Fatalf("runtime %s changed", k)
		}
	}
	for _, file := range []string{path, database} {
		if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0600 {
			t.Fatalf("owner-only config: %s %v", file, err)
		}
	}
	cfg.Site.Name = "Saved in SQLite"
	cfg.EVCharger.Password = "new-ev-secret"
	if err := SaveStored(st, path, cfg); err != nil {
		t.Fatal(err)
	}
	// Even invalid values in the old seed do not override the committed config.
	if err := os.WriteFile(path, []byte("config_database: state.db\nsite:\n  smoothing_alpha: invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reloaded, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Site.Name != "Saved in SQLite" || reloaded.EVCharger.Password != "new-ev-secret" {
		t.Fatal("YAML overrode SQLite")
	}
	if got, _ := st.LoadConfig("ev_charger_password"); got != "new-ev-secret" {
		t.Fatal("legacy credential row diverged")
	}
}

func TestSQLiteAuthorityNeverFallsBackToYAML(t *testing.T) {
	for _, content := range []string{"", "not a database"} {
		t.Run(content, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(minimalYAML+"\nconfig_database: missing.db\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if content != "" {
				if err := os.WriteFile(filepath.Join(dir, "missing.db"), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Load(path); err == nil {
				t.Fatal("missing/corrupt authority fell back to YAML")
			}
		})
	}
}

func TestInterruptedImportReusesCommittedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	database := filepath.Join(dir, "state.db")
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg, err := Parse([]byte(minimalYAML), dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConfigDatabase = database
	cfg.Site.Name = "Already committed"
	if err := SaveStored(st, path, cfg); err != nil {
		t.Fatal(err)
	}
	old, err := Parse([]byte(minimalYAML), dir)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := InitializeStorage(path, database, old, st)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Site.Name != "Already committed" || recovered.Revision != 1 {
		t.Fatal("old YAML was imported twice")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "config_database: state.db") {
		t.Fatalf("authority pointer: %v", err)
	}
}
