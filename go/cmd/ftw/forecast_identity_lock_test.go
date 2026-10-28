package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	_ "modernc.org/sqlite"
)

// RefreshIdentity must not hold forecastSiteConfig.mu while SaveConfig waits
// for the SQLite write lock, or the tick's Snapshot() waits with it.
func TestForecastSnapshotDoesNotWaitForIdentitySave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := &forecastSiteConfig{
		store:        st,
		baseRevision: "base",
		required:     map[string]bool{"meter": false},
		identity:     func(string) (string, bool) { return "serial:ABC123", true },
		configuredAt: time.Now().Add(-time.Minute),
	}

	// Another writer (rolloff, backup, history writer) holds the WAL write lock.
	other, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE config SET value = value WHERE 0`); err != nil {
		t.Fatal(err)
	}

	refreshDone := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		s.RefreshIdentity(time.Now())
		refreshDone <- time.Since(start)
	}()
	time.Sleep(100 * time.Millisecond) // RefreshIdentity now waits in SaveConfig

	start := time.Now()
	_ = s.Snapshot() // what the control tick does before persistTelemetryTick
	blocked := time.Since(start)
	_ = tx.Rollback()
	refresh := <-refreshDone
	t.Logf("Snapshot waited %v; RefreshIdentity took %v", blocked, refresh)
	if blocked > time.Second {
		t.Fatalf("tick Snapshot blocked %v behind identity SaveConfig held under forecast lock", blocked)
	}
}

// A failed binding save stays dirty and waits forecastBindingRetry before the
// next attempt, instead of retrying on every 1 s identity poll.
func TestForecastIdentityBindingSaveBacksOffAfterFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	broken, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = broken.Close() // every save now fails

	s := &forecastSiteConfig{
		store:        broken,
		baseRevision: "base",
		required:     map[string]bool{"meter": false},
		identity:     func(string) (string, bool) { return "serial:ABC123", true },
		configuredAt: time.Now().Add(-time.Minute),
	}
	now := time.Now()
	s.RefreshIdentity(now)
	if !s.bindingDirty {
		t.Fatal("failed binding save was marked clean")
	}

	st, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s.store = st
	s.RefreshIdentity(now.Add(time.Second))
	if _, ok := st.LoadConfig(forecastIdentityReceiptKey); ok || !s.bindingDirty {
		t.Fatal("binding save retried inside the back-off window")
	}
	s.RefreshIdentity(now.Add(forecastBindingRetry))
	if _, ok := st.LoadConfig(forecastIdentityReceiptKey); !ok || s.bindingDirty {
		t.Fatal("binding save not retried after the back-off window")
	}
}
