package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestRestoreRequiresExplicitConfirmation(t *testing.T) {
	if err := run([]string{"restore", "-archive", "backup.ftwbak", "-data", t.TempDir()}); err == nil {
		t.Fatal("restore ran without -yes")
	}
}

func TestRejectsUnknownCommand(t *testing.T) {
	if err := run([]string{"destroy"}); err == nil {
		t.Fatal("unknown command accepted")
	}
}

func TestCreateProgressShowsBackupPhasesAndCompletion(t *testing.T) {
	data := t.TempDir()
	statePath := filepath.Join(data, "state.db")
	store, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := createWithProgress([]string{
		"-state", statePath, "-data", data,
		"-output", filepath.Join(t.TempDir(), "backups"), "-progress",
	}, &output); err != nil {
		t.Fatal(err)
	}
	phases := map[string]bool{}
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var event struct {
			Event          string `json:"event"`
			Phase          string `json:"phase"`
			CompletedBytes int64  `json:"completed_bytes"`
			TotalBytes     int64  `json:"total_bytes"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Event != "progress" {
			t.Fatalf("unexpected event %q", event.Event)
		}
		if event.Phase == "complete" && (event.CompletedBytes == 0 || event.CompletedBytes != event.TotalBytes) {
			t.Fatalf("invalid completion progress: %+v", event)
		}
		if (event.Phase == "packing_archive" || event.Phase == "verifying_archive") &&
			event.TotalBytes > 0 && event.CompletedBytes == event.TotalBytes {
			phases[event.Phase+"_measured"] = true
		}
		phases[event.Phase] = true
	}
	for _, phase := range []string{"copying_database", "compressing_database", "checking_sources", "packing_archive", "packing_archive_measured", "syncing_archive", "verifying_archive", "verifying_archive_measured", "checking_database", "complete"} {
		if !phases[phase] {
			t.Errorf("missing %s progress", phase)
		}
	}
}
