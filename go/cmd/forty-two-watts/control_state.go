package main

import (
	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/control"
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
	ctrl.DriverLimits = driverLimitsFrom(cfg.Drivers, cfg.Batteries)
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
	ctrl.PVSurplusAbsorbSoCCapPct = cfg.Site.PVSurplusAbsorbSoCCapPct
	ctrl.PVSurplusAbsorbThresholdW = cfg.Site.PVSurplusAbsorbThresholdW
	return ctrl
}
