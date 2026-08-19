package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/pvperf"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestPVPerformanceDisabled(t *testing.T) {
	srv := New(&Deps{}) // no PVPerf service
	req := httptest.NewRequest(http.MethodGet, "/api/pv/performance", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Enabled bool  `json:"enabled"`
		Items   []any `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Enabled {
		t.Error("enabled should be false when no PVPerf service is wired")
	}
	if len(resp.Items) != 0 {
		t.Errorf("items should be empty, got %d", len(resp.Items))
	}
}

func TestPVPerformanceEnabledReturnsScores(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Seed two recent scored days (relative to now so the default window covers them).
	now := time.Now()
	pr := 0.9
	for i := 1; i <= 2; i++ {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		if err := st.SavePVPerformance(state.PVPerformanceDay{
			Day: day, ExpectedWh: 10000, ActualWh: 9000, PR: &pr,
		}); err != nil {
			t.Fatal(err)
		}
	}

	srv := New(&Deps{PVPerf: &pvperf.Service{Store: st}})
	req := httptest.NewRequest(http.MethodGet, "/api/pv/performance?days=30", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Enabled          bool     `json:"enabled"`
		PerformanceRatio *float64 `json:"performance_ratio"`
		Attribution      string   `json:"attribution"`
		Items            []struct {
			Day        string  `json:"day"`
			ExpectedWh float64 `json:"expected_wh"`
			ActualWh   float64 `json:"actual_wh"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Enabled {
		t.Error("enabled should be true")
	}
	if len(resp.Items) != 2 {
		t.Fatalf("want 2 scored days, got %d", len(resp.Items))
	}
	if resp.PerformanceRatio == nil || *resp.PerformanceRatio < 0.89 || *resp.PerformanceRatio > 0.91 {
		t.Errorf("energy-weighted PR should be ~0.9, got %v", resp.PerformanceRatio)
	}
	if resp.Attribution == "" {
		t.Error("attribution (SMHI STRÅNG CC BY 4.0) should be present")
	}
}
