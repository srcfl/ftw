package main

import (
	"log/slog"
	"math"
	"strconv"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
)

func newControlStateFromConfig(cfg *config.Config) *control.State {
	ctrl := control.NewState(cfg.Site.GridTargetW, cfg.Site.GridToleranceW, cfg.SiteMeterDriver())
	if cfg.Site.Gain != 0 {
		ctrl.PI.Kp = cfg.Site.Gain
	}
	ctrl.SlewRateW = cfg.Site.SlewRateW
	// applyDefaults() ensures SlewEnabled is non-nil at this point.
	if cfg.Site.SlewEnabled != nil {
		ctrl.SlewEnabled = *cfg.Site.SlewEnabled
	}
	ctrl.MinDispatchIntervalS = cfg.Site.MinDispatchIntervalS
	ctrl.InverterGroups = inverterGroupsFrom(cfg.Drivers)
	ctrl.SupportsPVCurtail = supportsPVCurtailFrom(cfg.Drivers)
	ctrl.SolarFeedDrivers = solarFeedDriversFrom(cfg.Drivers)
	ctrl.DriverLimits = driverLimitsFrom(cfg.ModbusControlDrivers(), cfg.Batteries)
	// Per-phase fuse params for the per-phase clamp inside applyFuseGuard
	// + forceFuseDischarge. Reads l1_a/l2_a/l3_a from the meter driver
	// when SiteFuseAmps > 0; otherwise the per-phase clamp is disabled.
	ctrl.SiteFuseAmps = cfg.Fuse.MaxAmps
	ctrl.SiteFuseVoltage = cfg.Fuse.Voltage
	ctrl.SiteFusePhases = cfg.Fuse.Phases
	// EffectiveSafetyMarginA distinguishes nil ("unset, use default")
	// from explicit 0 ("operator chose to disable"). The earlier
	// `<= 0 -> default` shortcut clobbered the disable case.
	ctrl.SiteFuseSafetyA = cfg.Fuse.EffectiveSafetyMarginA()
	// PV surplus absorber underlay (opt-in). cap == 0 keeps it off.
	ctrl.PVSurplusAbsorbSoCCap = cfg.Site.PVSurplusAbsorbSoCCap
	ctrl.PVSurplusAbsorbThresholdW = cfg.Site.PVSurplusAbsorbThresholdW
	// DC-link protective curtail — opt-in, default off. SoC threshold
	// and margin fall back to dispatch defaults (0.80 / 1000 W) when
	// unset, applied inside ComputePVCurtail.
	ctrl.DCLinkProtectionEnabled = cfg.Site.DCLinkProtectionEnabled
	ctrl.DCLinkProtectionSoCThreshold = cfg.Site.DCLinkProtectionSoCThreshold
	ctrl.DCLinkProtectionMarginW = cfg.Site.DCLinkProtectionMarginW
	// Site export ceiling — opt-in, default off. The fuse guard scales
	// battery discharge back so predicted export stays under max_export_w,
	// protecting inverters that trip below the breaker rating.
	ctrl.MaxExportW = cfg.Site.MaxExportW
	return ctrl
}

// restoredGridTargetW parses the grid target saved in state.db. The Home
// Assistant bridge once stored "NaN" from an MQTT command; restoring that
// would poison the PI setpoint on every boot, so a non-finite value is
// ignored and the configured target stays.
func restoredGridTargetW(v string) (float64, bool) {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// energyDispatchEnabled reports whether planner modes run the
// energy-allocation dispatch. planner.legacy_dispatch: true is the only way
// back to the PI-on-grid-target path, and a config without a planner section
// takes the default. Boot and hot reload both read it here so a reload cannot
// leave a site on a path a fresh boot would not choose.
func energyDispatchEnabled(cfg *config.Config) bool {
	return cfg.Planner == nil || !cfg.Planner.LegacyDispatch
}

// removedModes are control modes FTW no longer runs. Priority held every
// battery at its measured power, because nothing ever set its battery order,
// and weighted split equally because nothing set its weights.
var removedModes = map[string]bool{"priority": true, "weighted": true}

// restoreStoredMode applies the mode saved in state.db. A removed mode becomes
// manual self-consumption and is saved back, so the stored mode matches what
// the site runs. Any other unknown value keeps the default mode, as before.
func restoreStoredMode(ctrl *control.State, st *state.Store) {
	v, ok := st.LoadConfig("mode")
	if !ok {
		return
	}
	if m := control.Mode(v); control.IsValidMode(m) {
		ctrl.Mode = m
		return
	}
	if !removedModes[v] {
		return
	}
	ctrl.Mode = control.ModeSelfConsumption
	if err := st.SaveConfig("mode", string(ctrl.Mode)); err != nil {
		slog.Warn("the stored control mode was removed; running self_consumption, but it could not be saved",
			"stored", v, "err", err)
		return
	}
	slog.Warn("the stored control mode was removed; running and saving self_consumption instead", "stored", v)
}
