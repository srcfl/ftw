package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestCreateVerifyAndRestoreCompleteBackup(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "cold", "2026", "07"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "driver-repository", "installed", "official", "meter", "1.2.3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "driver-repository", "active"), 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "state.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig("backup-test", "preserved"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dataDir, "config.yaml"), "site:\n  name: backup-test\n")
	writeTestFile(t, filepath.Join(dataDir, "cold", "2026", "07", "17.parquet"), "parquet-test")
	installed := filepath.Join(dataDir, "driver-repository", "installed", "official", "meter", "1.2.3", "meter.lua")
	writeTestFile(t, installed, "DRIVER = { id = 'meter', version = '1.2.3' }")
	active := filepath.Join(dataDir, "driver-repository", "active", "meter.lua")
	if err := os.Symlink(filepath.Join("..", "installed", "official", "meter", "1.2.3", "meter.lua"), active); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "snapshots", "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dataDir, "snapshots", "ignored", "state.db.gz"), "nested-backup")

	createdAt := time.Date(2026, 7, 18, 8, 30, 0, 0, time.UTC)
	info, err := Create(context.Background(), CreateOptions{
		State: st, StatePath: statePath, DataDir: dataDir,
		OutputDir: filepath.Join(dataDir, "backups"), Now: createdAt,
		Components: ComponentInventory{
			Core:    ComponentVersion{Version: "v1.3.1"},
			Drivers: []DriverVersion{{ID: "meter", Version: "1.2.3", SHA256: strings.Repeat("a", 64)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Verified || info.SizeBytes == 0 || len(info.SHA256) != 64 {
		t.Fatalf("backup info = %+v", info)
	}
	manifest, err := Verify(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Components.Core.Version != "v1.3.1" || manifest.DatabaseFile != "state.db" {
		t.Fatalf("manifest = %+v", manifest)
	}
	paths := make(map[string]bool)
	for _, entry := range manifest.Files {
		paths[entry.Path] = true
		if strings.Contains(entry.Path, "snapshots/") || strings.Contains(entry.Path, "backups/") || strings.HasSuffix(entry.Path, "state.db-wal") {
			t.Fatalf("transient/recursive file included: %s", entry.Path)
		}
	}
	for _, want := range []string{
		"data/state.db.gz", "data/config.yaml", "data/cold/2026/07/17.parquet",
		"data/driver-repository/installed/official/meter/1.2.3/meter.lua",
		"data/driver-repository/active/meter.lua",
	} {
		if !paths[want] {
			t.Errorf("manifest missing %s", want)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	writeTestFile(t, filepath.Join(dataDir, "config.yaml"), "site:\n  name: changed-after-backup\n")
	result, err := Restore(info.Path, dataDir, time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if result.SafetyDir == "" {
		t.Fatal("restore did not preserve the previous data directory")
	}
	got, err := os.ReadFile(filepath.Join(dataDir, "config.yaml"))
	if err != nil || !strings.Contains(string(got), "backup-test") {
		t.Fatalf("restored config = %q err=%v", got, err)
	}
	changed, err := os.ReadFile(filepath.Join(result.SafetyDir, "config.yaml"))
	if err != nil || !strings.Contains(string(changed), "changed-after-backup") {
		t.Fatalf("safety config = %q err=%v", changed, err)
	}
	restored, err := state.Open(filepath.Join(dataDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got, ok := restored.LoadConfig("backup-test"); !ok || got != "preserved" {
		t.Fatalf("restored database value = %q ok=%v", got, ok)
	}
	link, err := os.Readlink(filepath.Join(dataDir, "driver-repository", "active", "meter.lua"))
	if err != nil || !strings.Contains(link, "installed") {
		t.Fatalf("managed driver symlink = %q err=%v", link, err)
	}
}

func TestVerifyRejectsCorruptArchive(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "broken.ftwbak")
	if err := os.WriteFile(filename, []byte("not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(filename); err == nil {
		t.Fatal("Verify accepted a corrupt archive")
	}
}

func TestValidateManifestRejectsSymlinkTraversal(t *testing.T) {
	manifest := Manifest{
		Format: Format, SchemaVersion: SchemaVersion, CreatedAt: time.Now(),
		DatabaseFile: "state.db", DatabaseEntry: "data/state.db.gz",
		Files: []FileEntry{
			{Path: "data/state.db.gz", Type: "file", Size: 1, SHA256: strings.Repeat("a", 64)},
			{Path: "data/escape", Type: "symlink", LinkTarget: "../../outside", SHA256: strings.Repeat("b", 64)},
			{Path: "data/escape/file", Type: "file", Size: 1, SHA256: strings.Repeat("c", 64)},
		},
	}
	if err := validateManifest(manifest); err == nil {
		t.Fatal("manifest accepted a file nested below a symlink")
	}
}

func TestValidateManifestRejectsStandaloneEscapingSymlink(t *testing.T) {
	manifest := Manifest{
		Format: Format, SchemaVersion: SchemaVersion, CreatedAt: time.Now(),
		DatabaseFile: "state.db", DatabaseEntry: "data/state.db.gz",
		Files: []FileEntry{
			{Path: "data/state.db.gz", Type: "file", Size: 1, SHA256: strings.Repeat("a", 64)},
			{Path: "data/escape", Type: "symlink", LinkTarget: "../../outside", SHA256: strings.Repeat("b", 64)},
		},
	}
	if err := validateManifest(manifest); err == nil {
		t.Fatal("manifest accepted a symlink target outside data root")
	}
}

func TestValidateManifestRejectsSpecialPermissionBits(t *testing.T) {
	manifest := Manifest{
		Format: Format, SchemaVersion: SchemaVersion, CreatedAt: time.Now(),
		DatabaseFile: "state.db", DatabaseEntry: "data/state.db.gz",
		Files: []FileEntry{{
			Path: "data/state.db.gz", Type: "file", Mode: 0o4755, Size: 1,
			SHA256: strings.Repeat("a", 64),
		}},
	}
	if err := validateManifest(manifest); err == nil {
		t.Fatal("manifest accepted special permission bits")
	}
}

func TestRestoreContentsAndRevertPreserveBothStates(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "mounted-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "state.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig("generation", "backup"); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dataDir, "config.yaml"), "generation: backup\n")
	archiveDir := filepath.Join(root, "external")
	info, err := Create(context.Background(), CreateOptions{
		State: st, StatePath: statePath, DataDir: dataDir, OutputDir: archiveDir,
		Components: ComponentInventory{Core: ComponentVersion{Version: "v1.3.1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dataDir, "config.yaml"), "generation: current\n")
	writeTestFile(t, filepath.Join(dataDir, "current-only"), "keep me")

	restored, err := RestoreContents(info.Path, dataDir, time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dataDir, "config.yaml"))
	if err != nil || string(got) != "generation: backup\n" {
		t.Fatalf("active restored config = %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(restored.SafetyDir, "current-only")); err != nil {
		t.Fatalf("current state not retained: %v", err)
	}

	reverted, err := RevertContents(dataDir, restored.SafetyDir)
	if err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dataDir, "config.yaml"))
	if err != nil || string(got) != "generation: current\n" {
		t.Fatalf("reverted config = %q err=%v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(reverted.SafetyDir, "config.yaml"))
	if err != nil || string(got) != "generation: backup\n" {
		t.Fatalf("rejected restore not retained = %q err=%v", got, err)
	}
}

func writeTestFile(t *testing.T, filename, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
