package main

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The planner's earlier charge budget cannot override the owner's SoC limit.
// No solar or export is needed to reproduce the 10 kW order at 90%.
func TestConfiguredSoCMaxStopsDispatch(t *testing.T) {
	for _, mode := range []control.Mode{control.ModeCharge, control.ModePlannerArbitrage} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := parseBatteryLimitConfig(t, "", "    soc_max: 0.9\n    max_charge_w: 10000\n")
			ctrl := newControlStateFromConfig(cfg)
			ctrl.Mode = mode
			ctrl.SlewEnabled = false
			ctrl.UseEnergyDispatch = true
			ctrl.SlotDirective = func(now time.Time) (control.SlotDirective, bool) {
				return control.SlotDirective{
					SlotStart: now, SlotEnd: now.Add(15 * time.Minute),
					BatteryEnergyWh: 2500, PlannedGridW: 11000, HasPlannedGridW: true,
				}, true
			}
			tel := batteryLimitStore(1000)
			soc := 0.9
			tel.Update("battery", telemetry.DerBattery, 0, &soc, nil)
			got := control.ComputeDispatch(tel, ctrl, map[string]float64{"battery": 15000}, 40000)
			if len(got) != 1 || got[0].TargetW != 0 {
				t.Fatalf("soc_max=0.9 at 90%% sent %+v, want one 0 W target", got)
			}
		})
	}
}

func TestSoCMaxConfigPrecedenceAndReload(t *testing.T) {
	cfg := parseBatteryLimitConfig(t, "", "    soc_max: 0.9\n")
	cfg.Planner = &config.Planner{SoCMax: .8}
	ctrl := newControlStateFromConfig(cfg)
	ctrl.Mode, ctrl.SlewEnabled = control.ModeCharge, false
	tel := batteryLimitStore(1000)
	capacities := map[string]float64{"battery": 10000}
	check := func(soc float64, charging bool) {
		t.Helper()
		tel.Update("battery", telemetry.DerBattery, 0, &soc, nil)
		got := control.ComputeDispatch(tel, ctrl, capacities, 40000)
		if len(got) != 1 || (got[0].TargetW > 0) != charging {
			t.Fatalf("SoC %.2f: got %+v, want charging=%v", soc, got, charging)
		}
	}
	check(.85, true) // The battery's 90% overrides the planner's 80%.
	check(.9, false)
	// The config watcher replaces the limits through this same function.
	cfg.Batteries = nil
	ctrl.DriverLimits = driverLimitsFrom(cfg.ModbusControlDrivers(), cfg.Batteries, cfg.Planner)
	check(.85, false) // With no battery override, the planner's 80% applies.
	cfg.Planner = nil
	ctrl.DriverLimits = driverLimitsFrom(cfg.ModbusControlDrivers(), cfg.Batteries, cfg.Planner)
	check(.99, true) // No configured ceiling keeps the 100% default.
	check(1, false)
	zero := 0.0
	cfg.Batteries = map[string]config.Battery{"battery": {SoCMax: &zero}}
	ctrl.DriverLimits = driverLimitsFrom(cfg.ModbusControlDrivers(), cfg.Batteries, cfg.Planner)
	check(.5, false) // An explicit zero is not an absent limit.
}

// Dispatch keeps the higher of the battery's and the running planner's
// soc_min. The planner's unset soc_min means its 10 % default.
func TestSoCMinReachesDispatch(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name    string
		battery *float64
		planner *config.Planner
		want    float64
	}{
		{"no planner, no battery floor", nil, nil, 0},
		{"planner off", nil, &config.Planner{SoCMin: 0.2}, 0},
		{"planner default", nil, &config.Planner{Enabled: true}, 0.10},
		{"planner set", nil, &config.Planner{Enabled: true, SoCMin: 0.15}, 0.15},
		{"battery higher", f(0.25), &config.Planner{Enabled: true, SoCMin: 0.15}, 0.25},
		{"planner higher", f(0.05), &config.Planner{Enabled: true, SoCMin: 0.15}, 0.15},
		{"battery only", f(0.2), nil, 0.2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseBatteryLimitConfig(t, "", "    max_charge_w: 5000\n")
			cfg.Batteries = map[string]config.Battery{"battery": {SoCMin: tc.battery}}
			got := driverLimitsFrom(cfg.ModbusControlDrivers(), cfg.Batteries, tc.planner)["battery"].SoCMin
			if got != tc.want {
				t.Fatalf("SoCMin = %v, want %v", got, tc.want)
			}
		})
	}
}
