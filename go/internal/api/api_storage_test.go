package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestHandleStorageInventoryReportsCoreStateAndDataHeadroom(t *testing.T) {
	dataDir := t.TempDir()
	statePath := filepath.Join(dataDir, "state.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	markerPath := filepath.Join(dataDir, "leave-me-alone")
	if err := os.WriteFile(markerPath, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := New(&Deps{State: st, DataDir: dataDir})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/storage/inventory", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}

	var got storageInventoryResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rr.Body.String())
	}
	if got.Format != storageInventoryFormat || !got.ReadOnly || got.GeneratedAtMs == 0 {
		t.Errorf("contract fields = %+v", got)
	}
	if got.Databases.State.AllocatedPages == 0 || got.Databases.Cache.AllocatedPages == 0 {
		t.Errorf("database inventory missing: %+v", got.Databases)
	}
	if got.Filesystem.TotalBytes == 0 || got.Filesystem.AvailableBytes > got.Filesystem.TotalBytes {
		t.Errorf("filesystem inventory = %+v", got.Filesystem)
	}
	if strings.Contains(rr.Body.String(), dataDir) || strings.Contains(rr.Body.String(), statePath) {
		t.Fatalf("response exposed a storage path: %s", rr.Body.String())
	}
	if body, err := os.ReadFile(markerPath); err != nil || string(body) != "unchanged" {
		t.Fatalf("inventory changed an unrelated file: body=%q err=%v", body, err)
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
