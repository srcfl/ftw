package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
	"gopkg.in/yaml.v3"
)

func TestImportPreservesLegacySettingsForImageRollback(t *testing.T) {
	dir := t.TempDir()
	path, database := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state.db")
	raw := []byte(minimalYAML + "\ncaldav:\n  enabled: true\n  calendar_path: /house/energy/\n  poll_interval_s: 300\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
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
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var previous, migrated map[string]any
	if err := yaml.Unmarshal(raw, &previous); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(after, &migrated); err != nil {
		t.Fatal(err)
	}
	delete(migrated, "config_database")
	if !reflect.DeepEqual(previous, migrated) {
		t.Fatalf("image rollback lost the original YAML settings: before=%v after=%v", previous, migrated)
	}
}

func TestUpgradeRetryImportsSettingsSavedByTheRolledBackCore(t *testing.T) {
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
	first, err := InitializeStorage(path, database, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	first.Site.Name = "Saved in SQLite"
	if err := SaveStored(st, path, first); err != nil {
		t.Fatal(err)
	}
	// Older Core ignores config_database, then removes it when saving its own
	// typed YAML. Its explicit Settings save must survive the next upgrade.
	oldSave := "site:\n  name: Saved after rollback\nfuse:\n  max_amps: 20\n"
	if err := os.WriteFile(path, []byte(oldSave), 0600); err != nil {
		t.Fatal(err)
	}
	legacy, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := InitializeStorage(path, database, legacy, st)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Site.Name != "Saved after rollback" || retried.Fuse.MaxAmps != 20 || retried.Revision <= first.Revision {
		t.Fatalf("retry reused stale imported settings: site=%s amps=%v revision=%d", retried.Site.Name, retried.Fuse.MaxAmps, retried.Revision)
	}
}

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
	runtimeValues := map[string]string{"ev_charger_password": "old-ev-secret", "lan_auth_password": "old-hash", "forecast/site_id": "stable-site", "forecast/energyplan_state_v1": "{ \"opaque\": true }", "loadmodel/state_utc:home": "learned"}
	for k, v := range runtimeValues {
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
	for k, want := range runtimeValues {
		if got, _ := st.LoadConfig(k); got != want {
			t.Fatalf("runtime %s changed", k)
		}
	}
	for _, file := range []string{path, database} {
		if fi, err := os.Stat(file); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0600) {
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
	if err := os.WriteFile(path, []byte(minimalYAML), 0600); err != nil {
		t.Fatal(err)
	}
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
	if err := saveStored(st, path, cfg, fmt.Sprintf("%x", sha256.Sum256([]byte(minimalYAML)))); err != nil {
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

func TestRecoveryCannotSubstituteAnotherConfigAtTheSameRevision(t *testing.T) {
	root := t.TempDir()
	var first *Config
	var firstSeed, firstDatabase string
	for _, name := range []string{"first", "different"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		database := filepath.Join(dir, "state.db")
		seed := filepath.Join(dir, "config.yaml")
		st, err := state.Open(database)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := Parse([]byte(minimalYAML), dir)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Site.Name = name
		if _, err := InitializeStorage(seed, database, cfg, st); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		if name == "first" {
			first, err = Load(seed)
			if err != nil {
				t.Fatal(err)
			}
			firstSeed, firstDatabase = seed, database
		} else {
			// Simulate recovery replacing both databases after Load but before open.
			// A config-only replacement now fails even earlier on the history binding.
			raw, err := os.ReadFile(database)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(firstDatabase, raw, 0600); err != nil {
				t.Fatal(err)
			}
			history, err := os.ReadFile(state.HistoryDatabasePath(database))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(state.HistoryDatabasePath(firstDatabase), history, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	recovered, err := state.Open(firstDatabase)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if _, err := InitializeStorage(firstSeed, firstDatabase, first, recovered); err == nil {
		t.Fatal("different config with the same revision replaced the loaded settings")
	}
}
