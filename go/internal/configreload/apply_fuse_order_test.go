package configreload

import (
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

// The runtime applier in main.go starts with reg.Reload, which can spend
// seconds in driver shutdown, init and startup defaults. Dispatch must already
// use the committed fuse and export limits when the applier starts.
func TestApplyFuseReachesControlBeforeRuntimeApplier(t *testing.T) {
	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	cfg := &config.Config{}
	cfg.Fuse.MaxAmps = 25
	cfg.Fuse.Voltage = 230
	cfg.Fuse.Phases = 3
	cfg.Site.MaxExportW = 11000

	ctrl := control.NewState(0, 50, "meter")
	ctrl.SiteFuseAmps = 25
	ctrl.SiteFuseVoltage = 230
	ctrl.SiteFusePhases = 3
	ctrl.MaxExportW = 11000

	newCfg := *cfg
	newCfg.Fuse.MaxAmps = 16
	newCfg.Site.MaxExportW = 3000

	var cfgAmps, ctrlAmps, ctrlExport float64
	Apply(&cfgMu, cfg, &ctrlMu, ctrl, &newCfg, func(n, o *config.Config) {
		// This is where main.go calls reg.Reload.
		cfgMu.RLock()
		cfgAmps = cfg.Fuse.MaxAmps
		cfgMu.RUnlock()
		ctrlMu.Lock()
		ctrlAmps = ctrl.SiteFuseAmps
		ctrlExport = ctrl.MaxExportW
		ctrlMu.Unlock()
	})

	if cfgAmps != 16 {
		t.Fatalf("cfg fuse at applier start = %v, want 16", cfgAmps)
	}
	if ctrlAmps != cfgAmps {
		t.Errorf("dispatch fuse at applier start = %v A while committed config says %v A", ctrlAmps, cfgAmps)
	}
	if ctrlExport != 3000 {
		t.Errorf("dispatch MaxExportW at applier start = %v, want 3000", ctrlExport)
	}
}
