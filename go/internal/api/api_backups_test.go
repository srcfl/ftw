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
