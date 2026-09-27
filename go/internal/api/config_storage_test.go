package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
)

func storedConfigServer(t *testing.T) (*Server, *config.Config, *state.Store) {
	t.Helper()
	srv, _, cfg := postConfigServer(t, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	database := filepath.Join(dir, "state.db")
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg.Site = config.Site{Name: "Before", SmoothingAlpha: .3}
	cfg.Fuse = config.Fuse{MaxAmps: 16, Phases: 3, Voltage: 230}
	cfg.API.Port = 8080
	cfg.EVCharger = &config.EVCharger{Provider: "easee", Username: "driver"}
	if err := st.SaveConfig("ev_charger_password", "before-secret"); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.InitializeStorage(path, database, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	srv.deps.Cfg = cfg
	srv.deps.State = st
	srv.deps.ConfigPath = path
	srv.deps.SaveConfig = func(path string, cfg *config.Config) error { return config.SaveStored(st, path, cfg) }
	return srv, cfg, st
}

func TestFailedConfigCommitLeavesLiveConfigAndControlAlone(t *testing.T) {
	srv, cfg, st := storedConfigServer(t)
	candidate := *cfg
	candidate.Site.Name = "After"
	candidate.Site.GridTargetW = 999
	cp := *candidate.EVCharger
	cp.Password = "after-secret"
	candidate.EVCharger = &cp
	raw, _ := json.Marshal(candidate)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 500 || cfg.Site.Name != "Before" || cfg.EVCharger.Password != "before-secret" || srv.deps.Ctrl.GridTargetW == 999 {
		t.Fatalf("failed commit leaked into runtime: status=%d", rec.Code)
	}
	restarted, err := config.Load(srv.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Site.Name != "Before" || restarted.EVCharger.Password != "before-secret" {
		t.Fatal("failed request changed durable config")
	}
}

func TestSettingsETagRejectsAnOlderBrowserForm(t *testing.T) {
	srv, cfg, _ := storedConfigServer(t)
	get := httptest.NewRecorder()
	srv.Handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	etag := get.Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET did not carry a revision")
	}
	if strings.Contains(get.Body.String(), "before-secret") {
		t.Fatal("GET leaked EV credential")
	}
	candidate := *cfg
	candidate.Site.Name = "First save"
	raw, _ := json.Marshal(candidate)
	first := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(string(raw)))
	req.Header.Set("If-Match", etag)
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(first, req)
	if first.Code != 200 || first.Header().Get("ETag") == etag {
		t.Fatalf("first save: %d %s", first.Code, first.Body)
	}
	candidate.Site.Name = "Stale overwrite"
	raw, _ = json.Marshal(candidate)
	stale := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(string(raw)))
	req.Header.Set("If-Match", etag)
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(stale, req)
	if stale.Code != 409 || cfg.Site.Name != "First save" {
		t.Fatalf("stale form overwrote settings: %d", stale.Code)
	}
}

func TestFailedPlannerSaveLeavesLivePreferencesAndModeAlone(t *testing.T) {
	srv, ctrl, st := plannerPrefsServer(t, control.ModePlannerPassiveArbitrage)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/planner/prefs", strings.NewReader(`{"safety_k":2,"battery_export":"allowed"}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rec, req)
	_, export, k := srv.deps.PlannerPrefs.Get()
	if rec.Code != 500 || k != 1 || export != config.BatteryExportUnknown || ctrl.Mode != control.ModePlannerPassiveArbitrage {
		t.Fatalf("failed prefs save changed runtime: status=%d k=%v export=%s mode=%s", rec.Code, k, export, ctrl.Mode)
	}
}
