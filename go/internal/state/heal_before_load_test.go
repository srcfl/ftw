package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Without a usable snapshot the pre-load heal leaves a damaged state.db in
// place: the owner's settings are still in it, and Load reports the error.
func TestHealBeforeLoadWithoutSnapshotChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	damaged := bytes.Repeat([]byte{0xAB}, 8192)
	if err := os.WriteFile(path, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	ev, err := HealBeforeLoad(path)
	if err != nil || ev != nil {
		t.Fatalf("heal = %+v, %v", ev, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, damaged) {
		t.Fatalf("damaged state.db was moved or changed: %v", err)
	}
}

func TestHealBeforeLoadIgnoresMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ev, err := HealBeforeLoad(path)
	if err != nil || ev != nil {
		t.Fatalf("heal = %+v, %v", ev, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing state.db was created: %v", err)
	}
}
