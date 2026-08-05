// Package configreload watches the config.yaml file with fsnotify and applies
// changes to the running system: control state, and (eventually) driver
// registry diff. 500 ms debounce to coalesce editor saves.
package configreload

import (
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

// Applier is the function called when a new config is loaded from disk.
// Receives both the new and old configs so implementations can diff.
type Applier func(new, old *config.Config)

// Watcher watches a config file and re-applies on change.
type Watcher struct {
	path    string
	cfgMu   *sync.RWMutex
	cfg     *config.Config
	ctrlMu  *sync.Mutex
	ctrl    *control.State
	applier Applier

	fsw       *fsnotify.Watcher
	stop      chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

// New creates a watcher. `applier` is called with (new, old) after a
// successful reload; use it to propagate changes to driver registry etc.
func New(
	path string,
	cfgMu *sync.RWMutex, cfg *config.Config,
	ctrlMu *sync.Mutex, ctrl *control.State,
	applier Applier,
) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := fsw.Add(dir); err != nil {
		fsw.Close()
		return nil, err
	}
	return &Watcher{
		path: path, cfgMu: cfgMu, cfg: cfg,
		ctrlMu: ctrlMu, ctrl: ctrl,
		applier: applier, fsw: fsw,
		stop: make(chan struct{}),
	}, nil
}

// Start runs the watcher loop (goroutine).
func (w *Watcher) Start() {
	w.startOnce.Do(func() {
		go w.loop()
	})
}

// Stop terminates the watcher. It is safe to call multiple times.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stop)
		w.fsw.Close()
	})
}

func (w *Watcher) loop() {
	slog.Info("config watcher started", "path", w.path)
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	target := filepath.Base(w.path)
	for {
		select {
		case <-w.stop:
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			// Only care about events on our file
			if filepath.Base(ev.Name) != target {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			// Debounce: reset timer to 500 ms from now
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(500 * time.Millisecond)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			slog.Warn("watcher error", "err", err)
		case <-debounce.C:
			w.reload()
		}
	}
}

func (w *Watcher) reload() {
	newCfg, err := config.Load(w.path)
	if err != nil {
		slog.Warn("config reload failed", "err", err)
		return
	}
	Apply(w.cfgMu, w.cfg, w.ctrlMu, w.ctrl, newCfg, w.applier)
	slog.Info("config reload: applied")
}

// Apply is the single apply path for a changed config: diff newCfg
// against the shared snapshot, hot-apply the control-level fields, swap
// the shared config pointer, then run the applier callback with
// (new, old). The fsnotify watcher calls it after loading the file, and
// POST /api/config calls it directly with the config it just saved.
//
// It has to be one function. The API handler used to apply a hand-picked
// subset of fields and swap the pointer itself, which left this
// package's watcher diffing new against new when the fsnotify event
// arrived — so everything the handler didn't copy, starting with the
// site-meter designation, never reached the running controller until a
// restart (#760).
func Apply(
	cfgMu *sync.RWMutex, cfg *config.Config,
	ctrlMu *sync.Mutex, ctrl *control.State,
	newCfg *config.Config, applier Applier,
) {
	// Snapshot old
	cfgMu.RLock()
	oldCfg := *cfg
	cfgMu.RUnlock()

	// Apply control-level changes
	ctrlMu.Lock()
	if newCfg.Site.GridTargetW != oldCfg.Site.GridTargetW {
		slog.Info("config reload: grid_target_w", "old", oldCfg.Site.GridTargetW, "new", newCfg.Site.GridTargetW)
		ctrl.SetGridTarget(newCfg.Site.GridTargetW)
	}
	if newCfg.Site.GridToleranceW != oldCfg.Site.GridToleranceW {
		ctrl.GridToleranceW = newCfg.Site.GridToleranceW
	}
	if newCfg.Site.SlewRateW != oldCfg.Site.SlewRateW {
		ctrl.SlewRateW = newCfg.Site.SlewRateW
	}
	newEnabled := true
	if newCfg.Site.SlewEnabled != nil {
		newEnabled = *newCfg.Site.SlewEnabled
	}
	oldEnabled := true
	if oldCfg.Site.SlewEnabled != nil {
		oldEnabled = *oldCfg.Site.SlewEnabled
	}
	if newEnabled != oldEnabled {
		slog.Info("config reload: slew_enabled", "old", oldEnabled, "new", newEnabled)
		ctrl.SlewEnabled = newEnabled
	}
	if newCfg.Site.MinDispatchIntervalS != oldCfg.Site.MinDispatchIntervalS {
		ctrl.MinDispatchIntervalS = newCfg.Site.MinDispatchIntervalS
	}
	if newCfg.Site.PVSurplusAbsorbSoCCapPct != oldCfg.Site.PVSurplusAbsorbSoCCapPct {
		slog.Info("config reload: pv_surplus_absorb_soc_cap_pct",
			"old", oldCfg.Site.PVSurplusAbsorbSoCCapPct,
			"new", newCfg.Site.PVSurplusAbsorbSoCCapPct)
		ctrl.PVSurplusAbsorbSoCCapPct = newCfg.Site.PVSurplusAbsorbSoCCapPct
	}
	if newCfg.Site.PVSurplusAbsorbThresholdW != oldCfg.Site.PVSurplusAbsorbThresholdW {
		ctrl.PVSurplusAbsorbThresholdW = newCfg.Site.PVSurplusAbsorbThresholdW
	}
	if newCfg.Site.DCLinkProtectionEnabled != oldCfg.Site.DCLinkProtectionEnabled {
		slog.Info("config reload: dc_link_protection_enabled",
			"old", oldCfg.Site.DCLinkProtectionEnabled,
			"new", newCfg.Site.DCLinkProtectionEnabled)
		ctrl.DCLinkProtectionEnabled = newCfg.Site.DCLinkProtectionEnabled
	}
	if newCfg.Site.DCLinkProtectionSoCThreshold != oldCfg.Site.DCLinkProtectionSoCThreshold {
		ctrl.DCLinkProtectionSoCThreshold = newCfg.Site.DCLinkProtectionSoCThreshold
	}
	if newCfg.Site.DCLinkProtectionMarginW != oldCfg.Site.DCLinkProtectionMarginW {
		ctrl.DCLinkProtectionMarginW = newCfg.Site.DCLinkProtectionMarginW
	}
	// Site-meter swap (operator moved `is_site_meter: true` from one
	// driver to another, or set it for the first time). Without this
	// the dispatcher keeps reading the old driver's meter telemetry —
	// after the old driver stops emitting, grid_w pegs at 0 and the
	// control loop has no idea where the actual grid boundary is. The
	// fix is to update ctrl.SiteMeterDriver under the same lock that
	// gates every dispatch read of it. main.go's applier callback
	// follows up by syncing the field on mpc.Service + loadmodel.Service
	// (those services capture site-meter at construction and need the
	// same hot-update treatment).
	if newCfg.SiteMeterDriver() != oldCfg.SiteMeterDriver() {
		slog.Info("config reload: site_meter",
			"old", oldCfg.SiteMeterDriver(), "new", newCfg.SiteMeterDriver())
		ctrl.SiteMeterDriver = newCfg.SiteMeterDriver()
	}
	ctrlMu.Unlock()

	// Swap global pointer
	cfgMu.Lock()
	*cfg = *newCfg
	cfgMu.Unlock()

	// Let caller handle driver registry etc.
	if applier != nil {
		applier(newCfg, &oldCfg)
	}
}
