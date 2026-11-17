package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
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

func TestHealthForecastArchiveIsInformational(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	body := getHealth(t, st)
	if _, ok := body["forecast_archive"]; ok {
		t.Fatal("health published an archive block before the copier started")
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %v", body["status"])
	}
	st.ReportForecastLongArchive(state.ForecastLongArchiveStatus{
		State: "unavailable", LastError: "forecast archive rebuilt", CapBytes: state.ForecastLongMaxBytes,
	})
	body = getHealth(t, st)
	if body["status"] != "ok" {
		t.Fatalf("archive failure changed box status to %v", body["status"])
	}
	block, ok := body["forecast_archive"].(map[string]any)
	if !ok || block["state"] != "unavailable" {
		t.Fatalf("forecast_archive = %#v", body["forecast_archive"])
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

func TestHealthArchiveHealKeepsBootStorageEvent(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.RecordHealEvent(state.HealEvent{Tier: "state", Action: "restored", AtMs: 100, Detail: "state.db restored"})
	st.RecordHealEvent(state.HealEvent{Tier: "forecast_archive", Action: "rebuilt", AtMs: 200, Detail: "archive rebuilt"})
	storage, ok := getHealth(t, st)["storage"].(map[string]any)
	if !ok {
		t.Fatal("no storage object in /api/health")
	}
	if storage["state"] != "restored" || storage["detail"] != "state.db restored" || storage["last_event_ms"] != float64(100) {
		t.Fatalf("archive heal replaced the boot event: %v", storage)
	}
	if _, ok := storage["forecast_archive"]; ok {
		t.Fatalf("storage carries the archive tier: %v", storage)
	}
}
