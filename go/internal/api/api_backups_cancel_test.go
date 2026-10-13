package api

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestBackupClientDisconnectCancelsBeforePublishing(t *testing.T) {
	for _, body := range []string{"", "{}"} {
		t.Run(fmt.Sprintf("body_bytes_%d", len(body)), func(t *testing.T) {
			root := t.TempDir()
			st, err := state.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			maintenance := &sync.Mutex{}
			maintenance.Lock()
			server := New(&Deps{
				State: st, StatePath: filepath.Join(root, "state.db"), DataDir: root,
				BackupDir: filepath.Join(root, "backups"), DataMaintenanceMu: maintenance,
			})
			httpServer := httptest.NewServer(server.Handler())
			t.Cleanup(httpServer.Close)
			// Unlock before closing the server even when a regression leaves the
			// handler waiting. No test cleanup needs a Core restart.
			t.Cleanup(maintenance.Unlock)
			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(httpServer.URL, "http://"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			_, err = fmt.Fprintf(conn, "POST /api/backups HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
			if err != nil {
				t.Fatal(err)
			}
			waitBackupCondition(t, func() bool { return st.BackupProgress().Phase == "waiting_for_maintenance" })
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			waitBackupCondition(t, func() bool { return st.BackupProgress().Phase == "failed" })
			if got := st.BackupProgress().Error; !strings.Contains(got, "context canceled") {
				t.Fatalf("backup failure after disconnect = %q", got)
			}
			waitBackupCondition(t, func() bool {
				if !server.backupMu.TryLock() {
					return false
				}
				server.backupMu.Unlock()
				if !server.versionUpdateMu.TryLock() {
					return false
				}
				server.versionUpdateMu.Unlock()
				return true
			})
			archives, err := filepath.Glob(filepath.Join(root, "backups", "*.ftwbak"))
			if err != nil || len(archives) != 0 {
				t.Fatalf("archive published after disconnect: %v (error %v)", archives, err)
			}
		})
	}
}

func TestBackupCreateRefusesOversizedIgnoredBody(t *testing.T) {
	root := t.TempDir()
	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	server := New(&Deps{State: st, StatePath: filepath.Join(root, "state.db"), DataDir: root, BackupDir: filepath.Join(root, "backups")})
	r := httptest.NewRequest(http.MethodPost, "/api/backups", strings.NewReader(strings.Repeat(" ", maxJSONBody+1)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized backup body: status %d", w.Code)
	}
	if st.BackupProgress().Phase != "" {
		t.Fatal("oversized request started a backup")
	}
	if _, err := os.Stat(filepath.Join(root, "backups")); !os.IsNotExist(err) {
		t.Fatalf("oversized request created backup directory: %v", err)
	}
}

func waitBackupCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("backup condition did not hold before the deadline")
}
