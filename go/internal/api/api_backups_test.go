package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/backup"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestFullBackupLifecycle(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "cold"), 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "state.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveConfig("full-backup", "yes"); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dataDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("site:\n  name: api-backup\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	backupDir := filepath.Join(dataDir, "backups")
	srv := New(&Deps{
		State: st, StatePath: statePath, DataDir: dataDir, BackupDir: backupDir,
		ConfigPath: configPath, ColdDir: filepath.Join(dataDir, "cold"),
		UserDriverDir: filepath.Join(dataDir, "drivers"), DataMaintenanceMu: &sync.Mutex{},
		Version: "v1.3.1",
	})

	create := httptest.NewRecorder()
	srv.Handler().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/backups", nil))
	if create.Code != http.StatusCreated {
		t.Fatalf("create backup = %d %s", create.Code, create.Body.String())
	}
	var created struct {
		Backup backupListEntry `json:"backup"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Backup.Verified || !created.Backup.OnDevice || created.Backup.ID == "" {
		t.Fatalf("created backup = %+v", created.Backup)
	}
	archivePath := filepath.Join(backupDir, created.Backup.ID)
	manifest, err := backup.Verify(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Components.Core.Version != "v1.3.1" {
		t.Fatalf("core inventory = %+v", manifest.Components.Core)
	}

	list := httptest.NewRecorder()
	srv.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/backups", nil))
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"verified":true`) {
		t.Fatalf("list backups = %d %s", list.Code, list.Body.String())
	}
	var listed struct {
		FreeBytes int64 `json:"free_bytes"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil || listed.FreeBytes <= 0 {
		t.Fatalf("backup list reports no free space: %v %s", err, list.Body.String())
	}

	download := httptest.NewRecorder()
	srv.Handler().ServeHTTP(download, httptest.NewRequest(http.MethodGet, "/api/backups/"+created.Backup.ID, nil))
	if download.Code != http.StatusOK || download.Body.Len() == 0 || !strings.Contains(download.Header().Get("Content-Disposition"), created.Backup.ID) {
		t.Fatalf("download backup = %d bytes=%d headers=%v", download.Code, download.Body.Len(), download.Header())
	}

	verify := httptest.NewRecorder()
	srv.Handler().ServeHTTP(verify, httptest.NewRequest(http.MethodPost, "/api/backups/"+created.Backup.ID+"/verify", nil))
	if verify.Code != http.StatusOK || !strings.Contains(verify.Body.String(), `"verified":true`) {
		t.Fatalf("verify backup = %d %s", verify.Code, verify.Body.String())
	}

	remove := httptest.NewRecorder()
	srv.Handler().ServeHTTP(remove, httptest.NewRequest(http.MethodDelete, "/api/backups/"+created.Backup.ID, nil))
	if remove.Code != http.StatusOK {
		t.Fatalf("delete backup = %d %s", remove.Code, remove.Body.String())
	}
	if _, err := os.Stat(archivePath); !os.IsNotExist(err) {
		t.Fatalf("backup archive still exists: %v", err)
	}
}

func TestFullBackupRefusesPersistentPathOutsideDataDir(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "state.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(&Deps{
		State: st, StatePath: statePath, DataDir: dataDir, BackupDir: filepath.Join(dataDir, "backups"),
		ConfigPath: filepath.Join(root, "outside-config.yaml"), Version: "v1.3.1",
	})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/backups", nil))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "refusing") {
		t.Fatalf("outside persistent path = %d %s", rr.Code, rr.Body.String())
	}
}

func TestBackupIDRejectsTraversal(t *testing.T) {
	for _, id := range []string{"", "../x.ftwbak", "x.ftwbak", "ftw-full-backup-x.txt", `ftw-full-backup-..\\x.ftwbak`} {
		if validBackupID(id) {
			t.Errorf("accepted unsafe backup id %q", id)
		}
	}
}

// The home box's backup on 16 October 2026 failed after its caller had left
// and logged nothing. The journal and GET /api/backups must keep the reason.
func TestFailedBackupLogsAndReportsItsReason(t *testing.T) {
	dataDir := t.TempDir()
	statePath := filepath.Join(dataDir, "state.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	backupDir := filepath.Join(dataDir, "backups")
	srv := New(&Deps{State: st, StatePath: statePath, DataDir: dataDir, BackupDir: backupDir, Version: "v1.3.1"})
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	create := httptest.NewRecorder()
	srv.Handler().ServeHTTP(create, httptest.NewRequest(http.MethodPost, "/api/backups", nil).WithContext(ctx))
	if create.Code != http.StatusInternalServerError || !strings.Contains(create.Body.String(), "context canceled") {
		t.Fatalf("create = %d %s", create.Code, create.Body.String())
	}
	if !strings.Contains(logs.String(), `msg="full backup failed" err="context canceled"`) {
		t.Fatalf("journal lacks the reason: %s", logs.String())
	}
	list := httptest.NewRecorder()
	srv.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/backups", nil))
	var listed struct {
		Progress state.BackupProgress `json:"progress"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Progress.Phase != "failed" || listed.Progress.Error != "context canceled" {
		t.Fatalf("progress = %+v", listed.Progress)
	}

	// A run that fails before its first phase must not inherit the last
	// run's table.
	st.ReportBackupProgress(state.BackupProgress{Phase: state.BackupPhaseCopying, Table: "ts_buckets", RowsDone: 4_700_000})
	notDir := filepath.Join(dataDir, "backups-file")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	srv = New(&Deps{State: st, StatePath: statePath, DataDir: dataDir, BackupDir: notDir, Version: "v1.3.1"})
	srv.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/backups", nil))
	if p := st.BackupProgress(); p.Phase != "failed" || p.Table != "" || p.RowsDone != 0 || !strings.Contains(p.Error, "create output dir") {
		t.Fatalf("early failure = %+v", p)
	}
}
