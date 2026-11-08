package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
)

// A settings POST that omits site tuning must not leave a stored config the
// boot path cannot start from.
func TestPartialConfigPostSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path, database := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state.db")
	seed := []byte("site:\n  name: Test\nfuse:\n  max_amps: 16\n  phases: 3\n  voltage: 230\napi:\n  port: 8080\ndrivers: []\n")
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg, err := config.InitializeStorage(path, database, loaded, st)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Site.ControlIntervalS != 2 {
		t.Fatalf("seed interval = %d, want default 2", cfg.Site.ControlIntervalS)
	}

	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	srv := api.New(&api.Deps{
		Ctrl: control.NewState(0, 42, ""), CtrlMu: &ctrlMu,
		Cfg: cfg, CfgMu: &cfgMu,
		ConfigPath: path,
		DriverDir:  dir, UserDriverDir: dir,
		SaveConfig: func(p string, c *config.Config) error { return config.SaveStored(st, p, c) },
	})

	// No If-Match, no control_interval_s / slew_rate_w / grid_tolerance_w /
	// watchdog_timeout_s / min_dispatch_interval_s. smoothing_alpha is the only
	// site tuning key Validate insists on.
	body := []byte(`{"site":{"name":"Test","smoothing_alpha":0.3},"fuse":{"max_amps":16,"phases":3,"voltage":230},"api":{"port":8080},"drivers":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	t.Logf("POST /api/config = %d %s", rr.Code, rr.Body.String())
	if rr.Code != http.StatusOK {
		return // a rejected partial save is the safe outcome
	}

	restarted, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := restarted.Site
	t.Logf("reloaded: interval=%d slew=%v tol=%v watchdog=%d mindisp=%d",
		s.ControlIntervalS, s.SlewRateW, s.GridToleranceW, s.WatchdogTimeoutS, s.MinDispatchIntervalS)
	if s.ControlIntervalS != 2 || s.WatchdogTimeoutS != 60 {
		t.Fatalf("reloaded interval=%d watchdog=%d, want the defaults 2 and 60", s.ControlIntervalS, s.WatchdogTimeoutS)
	}

	// Same expression as go/cmd/ftw/main.go:2839 and :2854.
	controlInterval := time.Duration(restarted.Site.ControlIntervalS) * time.Second
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("boot ticker panics after a partial settings save: %v", p)
		}
	}()
	ticker := time.NewTicker(controlInterval)
	ticker.Stop()
}
