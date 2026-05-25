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

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/control"
)

// Applier is the function called when a new config is loaded from disk.
// Receives both the new and old configs so implementations can diff.
type Applier func(new, old *config.Config)

// Watcher watches a config file and re-applies on change.
type Watcher struct {
	path     string
	cfgMu    *sync.RWMutex
	cfg      *config.Config
	ctrlMu   *sync.Mutex
	ctrl     *control.State
	applier  Applier

	fsw      *fsnotify.Watcher
	stop     chan struct{}
	stopOnce sync.Once
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
	if err != nil { return nil, err }
	dir := filepath.Dir(path)
	if dir == "" { dir = "." }
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
	go w.loop()
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
			if !ok { return }
			// Only care about events on our file
			if filepath.Base(ev.Name) != target { continue }
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 { continue }
			// Debounce: reset timer to 500 ms from now
			if !debounce.Stop() {
				select { case <-debounce.C: default: }
			}
			debounce.Reset(500 * time.Millisecond)
		case err, ok := <-w.fsw.Errors:
			if !ok { return }
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
	// Snapshot old
	w.cfgMu.RLock()
	oldCfg := *w.cfg
	w.cfgMu.RUnlock()

	// Apply control-level changes
	w.ctrlMu.Lock()
	if newCfg.Site.GridTargetW != oldCfg.Site.GridTargetW {
		slog.Info("config reload: grid_target_w", "old", oldCfg.Site.GridTargetW, "new", newCfg.Site.GridTargetW)
		w.ctrl.SetGridTarget(newCfg.Site.GridTargetW)
	}
	if newCfg.Site.GridToleranceW != oldCfg.Site.GridToleranceW {
		w.ctrl.GridToleranceW = newCfg.Site.GridToleranceW
	}
	if newCfg.Site.SlewRateW != oldCfg.Site.SlewRateW {
		w.ctrl.SlewRateW = newCfg.Site.SlewRateW
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
		w.ctrl.SlewEnabled = newEnabled
	}
	if newCfg.Site.MinDispatchIntervalS != oldCfg.Site.MinDispatchIntervalS {
		w.ctrl.MinDispatchIntervalS = newCfg.Site.MinDispatchIntervalS
	}
	if newCfg.Site.PVSurplusAbsorbSoCCapPct != oldCfg.Site.PVSurplusAbsorbSoCCapPct {
		slog.Info("config reload: pv_surplus_absorb_soc_cap_pct",
			"old", oldCfg.Site.PVSurplusAbsorbSoCCapPct,
			"new", newCfg.Site.PVSurplusAbsorbSoCCapPct)
		w.ctrl.PVSurplusAbsorbSoCCapPct = newCfg.Site.PVSurplusAbsorbSoCCapPct
	}
	if newCfg.Site.PVSurplusAbsorbThresholdW != oldCfg.Site.PVSurplusAbsorbThresholdW {
		w.ctrl.PVSurplusAbsorbThresholdW = newCfg.Site.PVSurplusAbsorbThresholdW
	}
	w.ctrlMu.Unlock()

	// Swap global pointer
	w.cfgMu.Lock()
	*w.cfg = *newCfg
	w.cfgMu.Unlock()

	// Let caller handle driver registry etc.
	if w.applier != nil {
		w.applier(newCfg, &oldCfg)
	}
	slog.Info("config reload: applied")
}
