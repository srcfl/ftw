// Package configreload applies committed settings to the running system.
package configreload

import (
	"log/slog"
	"sync"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

// Applier receives the committed config and the previous running config.
type Applier func(new, old *config.Config)

// Apply updates Core, then passes both configs to the runtime callback.
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
	if newCfg.Site.Gain != oldCfg.Site.Gain && ctrl.PI != nil {
		slog.Info("config reload: site.gain", "old", oldCfg.Site.Gain, "new", newCfg.Site.Gain)
		ctrl.PI.Kp = newCfg.Site.Gain
	}
	if newCfg.Site.PVSurplusAbsorbSoCCap != oldCfg.Site.PVSurplusAbsorbSoCCap {
		slog.Info("config reload: pv_surplus_absorb_soc_cap",
			"old", oldCfg.Site.PVSurplusAbsorbSoCCap,
			"new", newCfg.Site.PVSurplusAbsorbSoCCap)
		ctrl.PVSurplusAbsorbSoCCap = newCfg.Site.PVSurplusAbsorbSoCCap
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
