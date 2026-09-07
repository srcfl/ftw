package state

import (
	"path/filepath"
	"testing"
)

func TestRetiredCalendarSurvivesUpgradeAndBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const calendar = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:Charge car 80%\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	// The old release created these tables. New installs must not create them.
	for _, query := range []string{
		`CREATE TABLE caldav_calendars (path TEXT PRIMARY KEY NOT NULL, name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '') STRICT`,
		`CREATE TABLE caldav_objects (path TEXT PRIMARY KEY NOT NULL, collection TEXT NOT NULL, etag TEXT NOT NULL, data TEXT NOT NULL, modified_ms INTEGER NOT NULL) STRICT`,
		`INSERT INTO caldav_calendars VALUES ('/ftw/energy/', 'Energy', '')`,
	} {
		if _, err := s.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO caldav_objects VALUES ('/ftw/energy/car.ics', '/ftw/energy/', 'etag', ?, 42)`, calendar); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"caldav_password":              "old-calendar-password",
		"forecast/site_id":             "stable-site",
		"forecast/energyplan_state_v1": "{ \"opaque\": true }",
		"loadmodel/home":               "learned",
		"loadmodel/profile":            "away",
	} {
		if err := s.SaveConfig(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upgraded.Close() })
	if err := upgraded.RetireCalendarProfile(); err != nil {
		t.Fatal(err)
	}
	if got, _ := upgraded.LoadConfig("loadmodel/profile"); got != "home" {
		t.Fatalf("old away selection survived: %q", got)
	}
	if err := upgraded.SaveConfig("loadmodel/profile", "away"); err != nil {
		t.Fatal(err)
	}
	if err := upgraded.RetireCalendarProfile(); err != nil {
		t.Fatal(err)
	}
	if got, _ := upgraded.LoadConfig("loadmodel/profile"); got != "away" {
		t.Fatal("migration replaced a later manual choice")
	}
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	if err := upgraded.SnapshotTo(backupPath); err != nil {
		t.Fatal(err)
	}
	backup, err := Open(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { backup.Close() })
	for _, store := range []*Store{upgraded, backup} {
		var data string
		if err := store.db.QueryRow(`SELECT data FROM caldav_objects WHERE path = '/ftw/energy/car.ics'`).Scan(&data); err != nil {
			t.Fatal(err)
		}
		if data != calendar {
			t.Fatal("calendar bytes changed")
		}
		for key, want := range map[string]string{
			"caldav_password":              "old-calendar-password",
			"forecast/site_id":             "stable-site",
			"forecast/energyplan_state_v1": "{ \"opaque\": true }",
			"loadmodel/home":               "learned",
		} {
			if got, ok := store.LoadConfig(key); !ok || got != want {
				t.Fatalf("%s changed: %q", key, got)
			}
		}
	}
}
