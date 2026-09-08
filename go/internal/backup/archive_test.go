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

func TestDuckDBBackupOmitsImportedSamplesAndLiveFiles(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "source")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "custom.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	oldTS := time.Now().Add(-40 * 24 * time.Hour).UnixMilli()
	if err := st.RecordSamples([]state.Sample{{Driver: "meter", Metric: "power", TsMs: oldTS, Value: 123, Unit: "W"}}); err != nil {
		t.Fatal(err)
	}
	coldDir := filepath.Join(dataDir, "cold")
	_, files, err := st.RolloffToParquet(context.Background(), coldDir)
	if err != nil || len(files) != 1 {
		t.Fatalf("legacy source: %v %v", files, err)
	}
	if err := st.ImportLegacyParquet(context.Background(), coldDir); err != nil {
		t.Fatal(err)
	}
	liveTmp := state.HistoryDatabasePath(statePath) + ".tmp"
	if err := os.MkdirAll(liveTmp, 0700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(liveTmp, "spill.bin"), "transient history data")
	writeTestFile(t, filepath.Join(coldDir, "diagnostics", "2026", "01", "01.parquet"), "diagnostic archive")
	info, err := Create(context.Background(), CreateOptions{State: st, StatePath: statePath, DataDir: dataDir, OutputDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Verify(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range manifest.Files {
		if strings.Contains(f.Path, ".duckdb") || (strings.HasPrefix(f.Path, "data/cold/") && !strings.HasPrefix(f.Path, "data/cold/diagnostics/")) {
			t.Fatalf("live or duplicated history in archive: %s", f.Path)
		}
	}
	st.Close()
	restoredDir := filepath.Join(root, "restored")
	if _, err := Restore(info.Path, restoredDir, time.Now()); err != nil {
		t.Fatal(err)
	}
	// A SQLite-only Core reads the portable database, with no overlapping
	// daily sample files that could make its old merge count samples twice.
	restored, err := state.Open(filepath.Join(restoredDir, "custom.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.ImportLegacyParquet(context.Background(), filepath.Join(restoredDir, "cold")); err != nil {
		t.Fatal(err)
	}
	samples, err := restored.LoadSeries("meter", "power", 0, time.Now().UnixMilli(), 0)
	if err != nil || len(samples) != 1 || samples[0].Value != 123 {
		t.Fatalf("restored history: %+v %v", samples, err)
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

func TestAbsoluteManagedDriverBackupRestoresAtAnotherPath(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "original-data")
	installed := filepath.Join(dataDir, "driver-repository", "installed", "ftw-official", "goodwe", "1.0.1", strings.Repeat("a", 64), "goodwe.lua")
	active := filepath.Join(dataDir, "driver-repository", "active", "goodwe.lua")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(active), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, installed, "DRIVER = { id = 'goodwe', version = '1.0.1' }")
	if err := os.Symlink(installed, active); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "state.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	archive, err := Create(context.Background(), CreateOptions{State: st, StatePath: statePath, DataDir: dataDir, OutputDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Verify(archive.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Files {
		if entry.Type == "symlink" && filepath.IsAbs(entry.LinkTarget) {
			t.Fatalf("archive kept host link: %+v", entry)
		}
	}
	// The archive may not silently alter the active installation.
	if target, _ := os.Readlink(active); target != installed {
		t.Fatalf("source link changed: %q", target)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Remove the source completely before reading the restored link. Otherwise
	// a link back to the old host path could make this test pass by accident.
	if err := os.RemoveAll(dataDir); err != nil {
		t.Fatal(err)
	}
	restoredDir := filepath.Join(root, "new-installation", "data")
	if err := os.MkdirAll(filepath.Dir(restoredDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(archive.Path, restoredDir, time.Now()); err != nil {
		t.Fatal(err)
	}
	restoredLink := filepath.Join(restoredDir, "driver-repository", "active", "goodwe.lua")
	body, err := os.ReadFile(restoredLink)
	if err != nil || !strings.Contains(string(body), "goodwe") {
		t.Fatalf("restored driver = %q, %v", body, err)
	}
	restoredRoot, err := filepath.EvalSymlinks(restoredDir)
	if err != nil {
		t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(restoredLink)
	if err != nil || !pathInside(restoredRoot, target) {
		t.Fatalf("restored link escapes: %s, %v", target, err)
	}
}

func TestValidateManifestChecksSymlinkChains(t *testing.T) {
	for _, tc := range []struct {
		name  string
		links map[string]string
		valid bool
	}{
		{"internal driver chain", map[string]string{"data/active.lua": "installed.lua", "data/installed.lua": "version/driver.lua"}, true},
		{"cycle", map[string]string{"data/a": "b", "data/b": "a"}, false},
		// Lexically each target stays inside data/. Resolving alias first
		// changes the depth, so ../.. then escapes the archive root.
		{"dotdot after directory link", map[string]string{"data/dir/alias": "../target", "data/escape": "dir/alias/../../outside"}, false},
		{"absolute host path", map[string]string{"data/active.lua": "/app/data/driver.lua"}, false},
		{"direct escape", map[string]string{"data/active.lua": "../outside"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := Manifest{Format: Format, SchemaVersion: SchemaVersion, CreatedAt: time.Now(), DatabaseFile: "state.db", DatabaseEntry: "data/state.db.gz", Files: []FileEntry{{Path: "data/state.db.gz", Type: "file", SHA256: strings.Repeat("a", 64)}}}
			for name, target := range tc.links {
				m.Files = append(m.Files, FileEntry{Path: name, Type: "symlink", LinkTarget: target, SHA256: strings.Repeat("b", 64)})
			}
			err := validateManifest(m)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
		})
	}
}

func TestDescribeSourceRejectsExternalAbsoluteLinkWithoutReadingIt(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "secret")
	writeTestFile(t, outside, "must not enter the backup")
	link := filepath.Join(dataDir, "driver.lua")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	_, err := describeSource(context.Background(), dataDir, sourceEntry{archivePath: "data/driver.lua", sourcePath: link})
	if err == nil || !strings.Contains(err.Error(), "escapes data dir") {
		t.Fatalf("external link error = %v", err)
	}
}
