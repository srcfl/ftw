package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

// A config saved through the API must reach the running controller the
// same way a file edit does. The regression this guards (#760): the
// handler applied a hand-picked subset of control fields and swapped the
// shared config pointer itself, which left the configreload watcher
// diffing new against new — so a site meter set for the first time never
// reached the controller, grid_w pegged at 0 and load_w inflated to
// |pv_w| until a process restart.

const firstSiteMeterConfig = `{
  "site": {"name": "Test", "smoothing_alpha": 0.3},
  "fuse": {"max_amps": 16, "phases": 3, "voltage": 230},
  "api": {"port": 8080},
  "drivers": [{
    "name": "foxess",
    "lua": "drivers/foxess_h3_smart.lua",
    "is_site_meter": true,
    "capabilities": {"modbus": {"host": "192.0.2.10", "port": 502, "unit_id": 247}}
  }]
}`

func postConfigServer(t *testing.T, applier func(newCfg, oldCfg *config.Config)) (*Server, *control.State, *config.Config) {
	t.Helper()
	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	cfg := &config.Config{}
	ctrl := control.NewState(0, 42, cfg.SiteMeterDriver())
	srv := New(&Deps{
		Ctrl: ctrl, CtrlMu: &ctrlMu,
		Cfg: cfg, CfgMu: &cfgMu,
		ConfigPath:    t.TempDir() + "/config.yaml",
		DriverDir:     t.TempDir(),
		UserDriverDir: t.TempDir(),
		SaveConfig:    func(string, *config.Config) error { return nil },
		ConfigApplier: applier,
	})
	return srv, ctrl, cfg
}

func postConfig(t *testing.T, srv *Server, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Logf("response: %s", rr.Body.String())
	}
	return rr.Code
}

func TestPostConfigFirstSiteMeterReachesControl(t *testing.T) {
	srv, ctrl, cfg := postConfigServer(t, nil)

	if code := postConfig(t, srv, firstSiteMeterConfig); code != 200 {
		t.Fatalf("POST /api/config = %d, want 200", code)
	}

	if got := ctrl.SiteMeterDriver; got != "foxess" {
		t.Fatalf("Ctrl.SiteMeterDriver = %q after POST, want %q — the saved site meter never reached the running controller", got, "foxess")
	}
	if got := cfg.SiteMeterDriver(); got != "foxess" {
		t.Fatalf("shared config SiteMeterDriver() = %q, want %q", got, "foxess")
	}
}

func TestPostConfigRunsTheSharedApplierWithOldSnapshot(t *testing.T) {
	var gotNew, gotOld *config.Config
	srv, _, _ := postConfigServer(t, func(newCfg, oldCfg *config.Config) {
		gotNew, gotOld = newCfg, oldCfg
	})

	if code := postConfig(t, srv, firstSiteMeterConfig); code != 200 {
		t.Fatalf("POST /api/config = %d, want 200", code)
	}

	if gotNew == nil {
		t.Fatal("ConfigApplier was never called — registry/capacities/mpc sync all silently skipped")
	}
	if got := gotNew.SiteMeterDriver(); got != "foxess" {
		t.Fatalf("applier newCfg.SiteMeterDriver() = %q, want %q", got, "foxess")
	}
	if got := gotOld.SiteMeterDriver(); got != "" {
		t.Fatalf("applier oldCfg.SiteMeterDriver() = %q, want the pre-POST snapshot %q", got, "")
	}
}
