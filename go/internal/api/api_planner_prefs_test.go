package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
)

func plannerPrefsServer(t *testing.T, mode control.Mode) (*Server, *control.State, *state.Store) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctrl := control.NewState(0, 50, "meter")
	ctrl.Mode = mode
	prefs := config.NewPlannerPrefs(config.ForecastTrustBalanced, config.BatteryExportUnknown)
	srv := New(&Deps{
		Ctrl:         ctrl,
		CtrlMu:       &sync.Mutex{},
		State:        st,
		CfgMu:        &sync.RWMutex{},
		Cfg:          &config.Config{},
		PlannerPrefs: prefs,
	})
	return srv, ctrl, st
}

func TestGetPlannerPrefsDefaults(t *testing.T) {
	srv, _, _ := plannerPrefsServer(t, control.ModePlannerPassiveArbitrage)
	req := httptest.NewRequest(http.MethodGet, "/api/planner/prefs", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["forecast_trust"] != "balanced" {
		t.Errorf("trust=%v", got["forecast_trust"])
	}
	if got["battery_export"] != "unknown" {
		t.Errorf("export=%v", got["battery_export"])
	}
	if got["mapped_mode"] != "planner_passive_arbitrage" {
		t.Errorf("mapped_mode=%v", got["mapped_mode"])
	}
	if got["mapped_k"] != 1.0 {
		t.Errorf("mapped_k=%v, want 1", got["mapped_k"])
	}
}

func TestPostPlannerPrefsUnknownNeverArbitrage(t *testing.T) {
	srv, ctrl, st := plannerPrefsServer(t, control.ModePlannerPassiveArbitrage)
	req := httptest.NewRequest(http.MethodPost, "/api/planner/prefs",
		strings.NewReader(`{"forecast_trust":"bold","battery_export":"unknown"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ctrl.Mode != control.ModePlannerPassiveArbitrage {
		t.Errorf("mode=%s, want passive (unknown must not export)", ctrl.Mode)
	}
	if v, _ := st.LoadConfig(config.StateKeyForecastTrust); v != "bold" {
		t.Errorf("stored trust=%q", v)
	}
	if v, _ := st.LoadConfig(config.StateKeyBatteryExport); v != "unknown" {
		t.Errorf("stored export=%q", v)
	}
}

func TestPostPlannerPrefsAllowedMapsToArbitrage(t *testing.T) {
	srv, ctrl, _ := plannerPrefsServer(t, control.ModePlannerPassiveArbitrage)
	req := httptest.NewRequest(http.MethodPost, "/api/planner/prefs",
		strings.NewReader(`{"forecast_trust":"cautious","battery_export":"allowed"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if ctrl.Mode != control.ModePlannerArbitrage {
		t.Errorf("mode=%s, want planner_arbitrage", ctrl.Mode)
	}
}

func TestPostPlannerPrefsRejectsJunk(t *testing.T) {
	srv, _, _ := plannerPrefsServer(t, control.ModePlannerPassiveArbitrage)
	req := httptest.NewRequest(http.MethodPost, "/api/planner/prefs",
		strings.NewReader(`{"forecast_trust":"spicy","battery_export":"unknown"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rr.Code)
	}
}

func TestSetModeActiveConfirmsExport(t *testing.T) {
	srv, _, st := plannerPrefsServer(t, control.ModePlannerPassiveArbitrage)
	req := httptest.NewRequest(http.MethodPost, "/api/mode",
		strings.NewReader(`{"mode":"planner_arbitrage"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if v, _ := st.LoadConfig(config.StateKeyBatteryExport); v != "allowed" {
		t.Errorf("export=%q, want allowed", v)
	}
}

func TestYAMLCustomKWinsOverTrust(t *testing.T) {
	k := 0.25
	srv, _, _ := plannerPrefsServer(t, control.ModePlannerPassiveArbitrage)
	srv.deps.Cfg.Planner = &config.Planner{PVForecastSafetyK: &k}
	req := httptest.NewRequest(http.MethodGet, "/api/planner/prefs", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["yaml_custom"] != true {
		t.Errorf("yaml_custom=%v", got["yaml_custom"])
	}
	if got["mapped_k"] != 0.25 {
		t.Errorf("mapped_k=%v, want 0.25", got["mapped_k"])
	}
}
