package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestHandleStorageInventoryReportsReadOnlyBreakdown(t *testing.T) {
	dataDir := t.TempDir()
	statePath := filepath.Join(dataDir, "state.db")
	coldDir := filepath.Join(dataDir, "cold")
	snapshotDir := filepath.Join(dataDir, "snapshots")
	backupDir := filepath.Join(dataDir, "backups")

	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveConfig("inventory-test", strings.Repeat("x", 8192)); err != nil {
		t.Fatal(err)
	}

	mustWriteStorageFile(t, filepath.Join(coldDir, "2026", "07", "18.parquet"), []byte("telemetry"))
	mustWriteStorageFile(t, filepath.Join(coldDir, "diagnostics", "2026", "07", "18.parquet"), []byte("diagnostic"))
	mustWriteStorageFile(t, statePath+".snapshot", []byte("recovery"))
	mustWriteStorageFile(t, filepath.Join(snapshotDir, "snapshot-1", "state.db.gz"), []byte("rollback"))
	mustWriteStorageFile(t, filepath.Join(snapshotDir, "snapshot-1", "meta.json"), []byte(
		`{"schema_version":2,"created_at":"2026-07-18T12:00:00Z","complete_database":true,"files":["state.db.gz"]}`))
	backupID := "ftw-full-backup-20260718T120000Z.ftwbak"
	mustWriteStorageFile(t, filepath.Join(backupDir, backupID), []byte("full-backup"))
	mustWriteStorageFile(t, filepath.Join(backupDir, backupID+".verified.json"), []byte(
		`{"id":"ftw-full-backup-20260718T120000Z.ftwbak","created_at":"2026-07-18T12:00:00Z","size_bytes":11,"verified":true}`))
	mustWriteStorageFile(t, filepath.Join(dataDir, "config.yaml"), []byte("site: {}\n"))

	cfgMu := &sync.RWMutex{}
	srv := New(&Deps{
		State: st, StatePath: statePath, DataDir: dataDir, ColdDir: coldDir,
		SnapshotDir: snapshotDir, BackupDir: backupDir,
		Cfg: &config.Config{State: &config.StateConf{ColdRetentionDays: 0}}, CfgMu: cfgMu,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/storage/inventory", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	var got storageInventoryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rr.Body.String())
	}
	if got.Databases.State.AllocatedPages == 0 || got.Databases.Cache.AllocatedPages == 0 {
		t.Errorf("database inventory missing: %+v", got.Databases)
	}
	if got.Databases.State.LivePages+got.Databases.State.FreePages != got.Databases.State.AllocatedPages {
		t.Errorf("state pages do not balance: %+v", got.Databases.State)
	}
	if got.Files.Parquet.Bytes != int64(len("telemetry")+len("diagnostic")) {
		t.Errorf("parquet bytes = %d", got.Files.Parquet.Bytes)
	}
	if got.Files.Parquet.DiagnosticsBytes != int64(len("diagnostic")) || got.Files.Parquet.DiagnosticsFiles != 1 {
		t.Errorf("diagnostics footprint = %+v", got.Files.Parquet)
	}
	if got.Files.RecoverySnapshot.Files != 1 || got.Files.RollbackSnapshots.Files != 2 {
		t.Errorf("snapshot footprints = recovery:%+v rollback:%+v", got.Files.RecoverySnapshot, got.Files.RollbackSnapshots)
	}
	if got.Files.FullBackups.Files != 2 || !got.Files.FullBackups.OnDevice {
		t.Errorf("full backup footprint = %+v", got.Files.FullBackups)
	}
	if got.Maintenance.LastParquetSuccessMs == 0 || got.Maintenance.LastRecoverySnapshotSuccessMs == 0 ||
		got.Maintenance.LastRollbackSnapshotSuccessMs == 0 || got.Maintenance.LastFullBackupVerifiedMs == 0 {
		t.Errorf("maintenance observations missing: %+v", got.Maintenance)
	}
	if got.Advisor.Mode != "dry_run" || !got.Advisor.ReadOnly || got.Advisor.BudgetBytes != defaultDryRunBudgetBytes {
		t.Errorf("advisor = %+v", got.Advisor)
	}
	if got.Advisor.ManagedBytes <= 0 || got.Advisor.FilesystemReserveBytes != filesystemReserveBytes {
		t.Errorf("advisor accounting = %+v", got.Advisor)
	}
	if strings.Contains(rr.Body.String(), dataDir) || strings.Contains(rr.Body.String(), statePath) {
		t.Fatalf("response exposed a storage path: %s", rr.Body.String())
	}
	if body, err := os.ReadFile(filepath.Join(coldDir, "2026", "07", "18.parquet")); err != nil || string(body) != "telemetry" {
		t.Fatalf("inventory mutated parquet: body=%q err=%v", body, err)
	}
}

func TestHandleStorageInventoryDoesNotCreateMissingArtifactDirectories(t *testing.T) {
	dataDir := t.TempDir()
	statePath := filepath.Join(dataDir, "state.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	missing := []string{
		filepath.Join(dataDir, "missing-cold"),
		filepath.Join(dataDir, "missing-snapshots"),
		filepath.Join(dataDir, "missing-backups"),
	}
	srv := New(&Deps{
		State: st, StatePath: statePath, DataDir: dataDir,
		ColdDir: missing[0], SnapshotDir: missing[1], BackupDir: missing[2],
	})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/storage/inventory", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	for _, path := range missing {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("inventory created %s (err=%v)", filepath.Base(path), err)
		}
	}
}

func TestHandleStorageInventoryRequiresPersistentWiring(t *testing.T) {
	srv := New(&Deps{})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/storage/inventory", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rr.Code, rr.Body.String())
	}
}

func TestStorageAdvisorMarksOnlyOnDevicePressureCandidates(t *testing.T) {
	files := storageFileInventory{
		Parquet:           parquetFootprint{storageFootprint: storageFootprint{Bytes: 300, OnDevice: true}, DiagnosticsBytes: 100},
		RollbackSnapshots: storageFootprint{Bytes: 200, OnDevice: true},
		FullBackups:       storageFootprint{Bytes: 400, OnDevice: false},
	}
	got := buildStorageAdvisor(defaultDryRunBudgetBytes+1, uint64(filesystemReserveBytes), true, files, nil)
	if got.Status != "action_needed" || got.OverBudgetBytes != 1 || !got.ReadOnly {
		t.Fatalf("advisor = %+v", got)
	}
	consider := map[string]bool{}
	for _, candidate := range got.Candidates {
		consider[candidate.Category] = candidate.WouldConsider
	}
	if !consider["parquet"] || !consider["planner_diagnostics"] || !consider["rollback_snapshots"] {
		t.Errorf("on-device candidates = %v", consider)
	}
	if consider["full_backups"] {
		t.Errorf("external full backups must not be a data-disk pressure candidate: %v", consider)
	}
}

func mustWriteStorageFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
