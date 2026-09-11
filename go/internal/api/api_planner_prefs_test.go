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
	prefs := config.NewPlannerPrefs(config.ForecastTrustBalanced, config.BatteryExportUnknown, 1)
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
	if got["safety_k"] != 1.0 {
		t.Errorf("safety_k=%v, want 1", got["safety_k"])
	}
}

func TestPostPlannerPrefsSafetyKIsStoredVerbatim(t *testing.T) {
	srv, _, st := plannerPrefsServer(t, control.ModePlannerPassiveArbitrage)
	req := httptest.NewRequest(http.MethodPost, "/api/planner/prefs",
		strings.NewReader(`{"safety_k":0.85,"battery_export":"not_allowed"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if v, _ := st.LoadConfig(config.StateKeySafetyK); v != "0.85" {
		t.Errorf("stored safety_k=%q, want 0.85", v)
	}
	// The enum stays a derived compatibility surface, written alongside.
	if v, _ := st.LoadConfig(config.StateKeyForecastTrust); v != "balanced" {
		t.Errorf("stored trust=%q, want balanced", v)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/planner/prefs", nil)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["safety_k"] != 0.85 || got["mapped_k"] != 0.85 {
		t.Errorf("safety_k=%v mapped_k=%v, want 0.85", got["safety_k"], got["mapped_k"])
	}
	if got["forecast_trust"] != "balanced" {
		t.Errorf("forecast_trust=%v, want balanced (derived from 0.85)", got["forecast_trust"])
	}
}

func TestPostPlannerPrefsClampsSafetyK(t *testing.T) {
	srv, _, st := plannerPrefsServer(t, control.ModePlannerPassiveArbitrage)
	req := httptest.NewRequest(http.MethodPost, "/api/planner/prefs",
		strings.NewReader(`{"safety_k":7,"battery_export":"not_allowed"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if v, _ := st.LoadConfig(config.StateKeySafetyK); v != "2" {
		t.Errorf("stored safety_k=%q, want 2", v)
	}
}

func TestPostPlannerPrefsOldClientEnumSetsK(t *testing.T) {
	// A client that only speaks forecast_trust still moves the primitive,
	// so the two never disagree.
	srv, _, st := plannerPrefsServer(t, control.ModePlannerPassiveArbitrage)
	req := httptest.NewRequest(http.MethodPost, "/api/planner/prefs",
		strings.NewReader(`{"forecast_trust":"cautious","battery_export":"not_allowed"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if v, _ := st.LoadConfig(config.StateKeySafetyK); v != "2" {
		t.Errorf("stored safety_k=%q, want 2", v)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["safety_k"] != 2.0 || got["forecast_trust"] != "cautious" {
		t.Errorf("safety_k=%v trust=%v", got["safety_k"], got["forecast_trust"])
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
	if v, _ := st.LoadConfig(config.StateKeySafetyK); v != "0" {
		t.Errorf("stored safety_k=%q, want 0", v)
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

func TestYAMLKNeverLocksTheSlider(t *testing.T) {
	// A pv_forecast_safety_k in YAML seeds the first boot and nothing
	// else — the slider owns the live value, so the prefs endpoint must
	// report the trust mapping, not the YAML number, and no lock flag.
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
	if _, present := got["yaml_custom"]; present {
		t.Errorf("yaml_custom still reported: %v", got["yaml_custom"])
	}
	if got["mapped_k"] != 1.0 {
		t.Errorf("mapped_k=%v, want 1.0 (balanced mapping, YAML ignored)", got["mapped_k"])
	}
}
