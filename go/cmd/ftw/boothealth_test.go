package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestBootProgressNeverClaimsAPIReadiness(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"boot.html": "<!doctype html><h1>Preparing FTW</h1>", "history-migration.js": "export const fixture=true;"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	boot := newBootPhaseHandler(dir)
	boot.setMigration(state.HistoryMigrationStatus{State: "starting", Phase: "seed", RowsDone: 2048})
	for _, path := range []string{"/api/health", "/api/status", "/api/version/update/status", "/", "/history-migration.js"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			boot.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
			want := http.StatusServiceUnavailable
			if path == "/api/health" || path == "/history-migration.js" {
				want = http.StatusOK
			}
			if w.Code != want {
				t.Fatalf("status=%d want=%d", w.Code, want)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("startup responses must not be cached")
			}
			if strings.HasPrefix(path, "/api/") {
				var body struct {
					Status    string
					Error     string
					Migration state.HistoryMigrationStatus
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Migration.RowsDone != 2048 || body.Migration.HistoryComplete {
					t.Fatalf("wrong progress: %+v", body)
				}
				if path == "/api/health" && body.Status != "starting" {
					t.Fatal("health must say starting")
				}
				if path != "/api/health" && body.Error != "starting" {
					t.Fatal("full API must stay unavailable")
				}
			} else if path == "/" && !strings.Contains(w.Body.String(), "Preparing FTW") {
				t.Fatal("browser must get startup page")
			}
		})
	}
}

func TestBootProgressConcurrentReadersAndUpdates(t *testing.T) {
	boot := newBootPhaseHandler("")
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int64(0); i < 50; i++ {
				boot.setMigration(state.HistoryMigrationStatus{State: "starting", Phase: "seed", RowsDone: i})
				w := httptest.NewRecorder()
				boot.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/health", nil))
				if !json.Valid(w.Body.Bytes()) {
					t.Error("partial progress response")
				}
			}
		}()
	}
	wg.Wait()
}
