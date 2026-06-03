package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

func getHealth(t *testing.T, st *state.Store) map[string]any {
	t.Helper()
	srv := New(&Deps{State: st, Tel: telemetry.NewStore(), Version: "test"})
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health: %v (%s)", err, w.Body.String())
	}
	return body
}

func TestHealthStorageOkOnCleanBoot(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	storage, ok := getHealth(t, st)["storage"].(map[string]any)
	if !ok {
		t.Fatal("no storage object in /api/health")
	}
	if storage["state"] != "ok" || storage["cache"] != "ok" {
		t.Errorf("clean boot storage = %v, want state=ok cache=ok", storage)
	}
}

func TestHealthStorageReportsCacheRebuild(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	cachePath := filepath.Join(dir, "cache.db")

	// Populate cache.db so it spans multiple pages, then close.
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	pts := make([]state.PricePoint, 0, 400)
	for i := 0; i < 400; i++ {
		pts = append(pts, state.PricePoint{
			Zone: "SE3", SlotTsMs: int64(i) * 900000, SlotLenMin: 15,
			SpotOreKwh: 50, TotalOreKwh: 60, Source: "test", FetchedAtMs: 1,
		})
	}
	if err := st.SavePrices(pts); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Flush the WAL into the main cache.db file so the bytes are corruptible.
	checkpoint(t, cachePath)
	corruptFileAt(t, cachePath, 8192)

	// Reopen — cache.db heals (rebuilt), event recorded.
	st2, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	storage, ok := getHealth(t, st2)["storage"].(map[string]any)
	if !ok {
		t.Fatal("no storage object in /api/health")
	}
	if storage["cache"] != "rebuilt" {
		t.Errorf("storage.cache = %v, want rebuilt (full storage=%v)", storage["cache"], storage)
	}
}

func checkpoint(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
}

func corruptFileAt(t *testing.T, path string, offset int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	junk := make([]byte, 256)
	for i := range junk {
		junk[i] = 0xFF
	}
	if _, err := f.WriteAt(junk, offset); err != nil {
		t.Fatal(err)
	}
}
