package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func newSeriesTestServer(t *testing.T) (*Server, *state.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	coldDir := filepath.Join(dir, "cold")
	return New(&Deps{State: st, ColdDir: coldDir}), st, coldDir
}

func getSeries(t *testing.T, srv *Server, url string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)
	var body map[string]any
	if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "json") {
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad json from %s: %v: %s", url, err, rr.Body.String())
		}
	}
	return rr.Code, body
}

func TestHandleSeriesMultiMetricAndUnits(t *testing.T) {
	srv, st, _ := newSeriesTestServer(t)
	now := time.Now().UnixMilli()
	var samples []state.Sample
	for i := 0; i < 10; i++ {
		samples = append(samples,
			state.Sample{Driver: "hp", Metric: "hp_power_w", TsMs: now - int64(i)*1000, Value: 500, Unit: "W"},
			state.Sample{Driver: "hp", Metric: "hp_temp_c", TsMs: now - int64(i)*1000, Value: 42, Unit: "°C"},
		)
	}
	if err := st.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}

	// Single metric keeps the legacy top-level shape.
	code, body := getSeries(t, srv, "/api/series?driver=hp&metric=hp_power_w&range=1h&points=5")
	if code != 200 {
		t.Fatalf("single metric status = %d", code)
	}
	if body["metric"] != "hp_power_w" || body["unit"] != "W" {
		t.Fatalf("single-metric shape wrong: %v", body)
	}
	pts := body["points"].([]any)
	if len(pts) == 0 {
		t.Fatal("no points returned")
	}
	p0 := pts[0].(map[string]any)
	for _, k := range []string{"ts", "v", "min", "max", "n"} {
		if _, ok := p0[k]; !ok {
			t.Fatalf("point missing %q: %v", k, p0)
		}
	}

	// Multi metric returns a series array.
	code, body = getSeries(t, srv, "/api/series?driver=hp&metric=hp_power_w,hp_temp_c&range=1h&points=5")
	if code != 200 {
		t.Fatalf("multi metric status = %d", code)
	}
	series := body["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("series count = %d, want 2: %v", len(series), body)
	}
	s1 := series[1].(map[string]any)
	if s1["metric"] != "hp_temp_c" || s1["unit"] != "°C" {
		t.Fatalf("second series wrong: %v", s1)
	}
}

func TestHandleSeriesAbsoluteWindowAndCSV(t *testing.T) {
	srv, st, _ := newSeriesTestServer(t)
	base := time.Now().Add(-1 * time.Hour).UnixMilli()
	var samples []state.Sample
	for i := 0; i < 5; i++ {
		samples = append(samples, state.Sample{
			Driver: "meter", Metric: "grid_w", TsMs: base + int64(i)*1000, Value: float64(i)})
	}
	if err := st.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}

	url := fmt.Sprintf("/api/series?driver=meter&metric=grid_w&since=%d&until=%d", base, base+4000)
	code, body := getSeries(t, srv, url)
	if code != 200 {
		t.Fatalf("absolute window status = %d", code)
	}
	if len(body["points"].([]any)) != 5 {
		t.Fatalf("absolute window returned %d points, want 5", len(body["points"].([]any)))
	}

	req := httptest.NewRequest(http.MethodGet, url+"&format=csv", nil)
	rr := httptest.NewRecorder()
	srv.mux.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("csv status/type = %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	lines := strings.Split(strings.TrimSpace(rr.Body.String()), "\n")
	if len(lines) != 6 { // header + 5 rows
		t.Fatalf("csv lines = %d, want 6: %q", len(lines), rr.Body.String())
	}
	if lines[0] != "ts_ms,driver,metric,v,min,max,n" {
		t.Fatalf("csv header = %q", lines[0])
	}
}

func TestHandleSeriesMergesColdParquet(t *testing.T) {
	srv, st, coldDir := newSeriesTestServer(t)

	// Old samples: destined for cold storage.
	oldTs := time.Now().Add(-state.RecentRetention - 48*time.Hour).UnixMilli()
	if err := st.RecordSamples([]state.Sample{
		{Driver: "meter", Metric: "grid_w", TsMs: oldTs, Value: 111},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RolloffToParquet(context.Background(), coldDir); err != nil {
		t.Fatal(err)
	}
	// Fresh sample stays in SQLite.
	nowTs := time.Now().UnixMilli()
	if err := st.RecordSamples([]state.Sample{
		{Driver: "meter", Metric: "grid_w", TsMs: nowTs, Value: 222},
	}); err != nil {
		t.Fatal(err)
	}

	url := fmt.Sprintf("/api/series?driver=meter&metric=grid_w&since=%d&until=%d", oldTs-1000, nowTs+1000)
	code, body := getSeries(t, srv, url)
	if code != 200 {
		t.Fatalf("cold merge status = %d", code)
	}
	pts := body["points"].([]any)
	if len(pts) != 2 {
		t.Fatalf("cold+recent merge returned %d points, want 2: %v", len(pts), pts)
	}
	first := pts[0].(map[string]any)
	last := pts[1].(map[string]any)
	if first["v"].(float64) != 111 || last["v"].(float64) != 222 {
		t.Fatalf("merged values = %v, %v; want 111 then 222", first["v"], last["v"])
	}
}
