package config

import (
	"os"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

func damageHeader(t *testing.T, database string) {
	t.Helper()
	f, err := os.OpenFile(database, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	junk := make([]byte, 4096)
	for i := range junk {
		junk[i] = 0xAB
	}
	if _, err := f.WriteAt(junk, 0); err != nil {
		t.Fatal(err)
	}
}

// A damaged state.db with a good snapshot. Boot reads settings (Load) before
// state.Open, so the heal must run before Load, with or without the
// verified-good marker that a power loss leaves in place.
func TestCorruptStateDBWithGoodSnapshotHealsBeforeLoad(t *testing.T) {
	for _, keepMarker := range []bool{false, true} {
		name := "marker-removed-by-background-verify"
		if keepMarker {
			name = "marker-present-default"
		}
		t.Run(name, func(t *testing.T) {
			path, database, st := importedLiveSettings(t)
			if err := st.SnapshotState(); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			if !keepMarker {
				if err := os.Remove(database + ".clean"); err != nil {
					t.Fatal(err)
				}
			}
			damageHeader(t, database)

			// Same order as main.go.
			ev, err := HealSettingsDatabase(path)
			if err != nil {
				t.Fatal(err)
			}
			if ev == nil || ev.Tier != "state" || ev.Action != "restored" {
				t.Fatalf("heal event = %+v", ev)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("config.Load after heal: %v", err)
			}
			if cfg.Site.Name != "Live Settings" {
				t.Fatalf("site = %q", cfg.Site.Name)
			}
			healed, err := state.Open(database)
			if err != nil {
				t.Fatal(err)
			}
			defer healed.Close()
			if _, err := InitializeStorage(path, database, cfg, healed); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHealthyStateDBIsNotHealedBeforeLoad(t *testing.T) {
	path, database, st := importedLiveSettings(t)
	if err := st.SnapshotState(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	ev, err := HealSettingsDatabase(path)
	if err != nil || ev != nil {
		t.Fatalf("heal = %+v, %v", ev, err)
	}
	if _, err := os.Stat(database + ".clean"); err != nil {
		t.Fatalf("verified-good marker removed: %v", err)
	}
}

// Load read newer settings from a damaged but readable state.db, then
// state.Open restored an older snapshot. Core runs the snapshot's settings
// and reports the step back instead of exiting.
func TestOlderSnapshotSettingsRunAndAreReported(t *testing.T) {
	path, database, st := importedLiveSettings(t)
	if err := st.SnapshotState(); err != nil {
		t.Fatal(err)
	}
	newer, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	newer.Site.Name = "Newer Settings"
	if err := SaveStored(st, path, newer); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(database + ".clean"); err != nil {
		t.Fatal(err)
	}
	damageHeader(t, database)

	healed, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer healed.Close()
	got, err := InitializeStorage(path, database, loaded, healed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Site.Name != "Live Settings" || got.Revision >= loaded.Revision {
		t.Fatalf("settings = %q revision %d, loaded revision %d", got.Site.Name, got.Revision, loaded.Revision)
	}
	events := healed.HealEvents()
	if len(events) == 0 || !strings.Contains(events[len(events)-1].Detail, "settings went back from revision") {
		t.Fatalf("heal events = %+v", events)
	}
}
