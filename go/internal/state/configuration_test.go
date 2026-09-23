package state

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestConfigurationCommitIsDurableAndAtomic(t *testing.T) {
	s := freshStore(t)
	if err := s.durableConfigWrite(func(tx *sql.Tx) error {
		var sync int
		if err := tx.QueryRow(`PRAGMA synchronous`).Scan(&sync); err != nil {
			return err
		}
		if sync != 2 {
			t.Fatalf("settings synchronous=%d, want FULL", sync)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first := []byte(`{"config":{"name":"before"}}`)
	rev, err := s.SaveConfiguration(first, 0, map[string]string{"password": "before"})
	if err != nil || rev != 1 {
		t.Fatalf("first commit: %d %v", rev, err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_password BEFORE UPDATE ON config WHEN NEW.key = 'password' BEGIN SELECT RAISE(ABORT, 'disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveConfiguration([]byte(`{"config":{"name":"after"}}`), 1, map[string]string{"password": "after"}); err == nil {
		t.Fatal("partial settings save succeeded")
	}
	got, found, err := s.Configuration()
	if err != nil || !found || got.Revision != 1 || string(got.Document) != string(first) {
		t.Fatalf("failed save changed document: %+v %v", got, err)
	}
	if value, _ := s.LoadConfig("password"); value != "before" {
		t.Fatal("failed save changed password")
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_password`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveConfiguration(first, 0, map[string]string{"password": "conflict"}); !errors.Is(err, ErrConfigurationConflict) {
		t.Fatalf("stale writer: %v", err)
	}
}

func TestPlannerPreferencesRollBackTogether(t *testing.T) {
	s := freshStore(t)
	values := map[string]string{"planner_safety_k": "1", "forecast_trust": "balanced", "battery_export": "unknown", "mode": "planner_passive_arbitrage"}
	if err := s.SaveConfigValues(values); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_prefs BEFORE UPDATE ON config WHEN NEW.key = 'planner_safety_k' BEGIN SELECT RAISE(ABORT, 'write failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := s.SavePlannerPreferences(map[string]string{"planner_safety_k": "2", "forecast_trust": "cautious", "battery_export": "allowed"}, []string{"planner_passive_arbitrage"}, "planner_arbitrage")
	if err == nil {
		t.Fatal("failed transaction succeeded")
	}
	for key, want := range values {
		if got, _ := s.LoadConfig(key); got != want {
			t.Fatalf("partial commit: %s=%s", key, got)
		}
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_prefs`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig("mode", "idle"); err != nil {
		t.Fatal(err)
	}
	if err := s.SavePlannerPreferences(map[string]string{"battery_export": "allowed"}, []string{"planner_passive_arbitrage"}, "planner_arbitrage"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoadConfig("mode"); got != "idle" {
		t.Fatal("preference write replaced a manual mode")
	}
}

func TestBackupExportsTheCapturedConfiguration(t *testing.T) {
	s := freshStore(t)
	if _, err := s.SaveConfiguration([]byte(`{"config":{"name":"captured"}}`), 0, nil); err != nil {
		t.Fatal(err)
	}
	updated := false
	doc, found, err := s.BackupWithConfiguration(filepath.Join(t.TempDir(), "state.db.gz"), func(p BackupProgress) {
		// The copy is complete at this point. Simulate another config writer while
		// compression runs; the YAML export must still use the captured revision.
		if p.Phase == BackupPhaseCompressing && !updated {
			updated = true
			if _, err := s.SaveConfiguration([]byte(`{"config":{"name":"later"}}`), 1, nil); err != nil {
				t.Fatal(err)
			}
		}
	})
	if err != nil || !found || !updated || doc.Revision != 1 {
		t.Fatalf("backup settings: %+v %v %v", doc, found, err)
	}
	current, _, err := s.Configuration()
	if err != nil || current.Revision != 2 {
		t.Fatalf("current revision: %+v %v", current, err)
	}
}
