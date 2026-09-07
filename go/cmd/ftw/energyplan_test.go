package main

import (
	"runtime"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestEnergyplanBetaSelection(t *testing.T) {
	for _, tc := range []struct{ version, engine, want string }{
		{"v2.15.0-beta.1", "", "energyplan"},
		{"v2.15.0", "", "core"},
		{"dev", "", "core"},
		{"dev-beta.invalid", "", "core"},
		{"v2.15.0-beta.1", "core", "core"},
		{"v2.15.0-beta.1", "python", "energyplan"},
		{"dev", "Energyplan", "energyplan"},
	} {
		if !energyplanSupported(runtime.GOOS, runtime.GOARCH) && tc.engine == "" {
			tc.want = "core"
		}
		if got := plannerEngine(&config.Planner{Engine: tc.engine}, tc.version); got != tc.want {
			t.Errorf("%s / %q: got %s, want %s", tc.version, tc.engine, got, tc.want)
		}
	}
}

func TestBuildMPCBetaStartsBundledEnergyplan(t *testing.T) {
	if !energyplanSupported(runtime.GOOS, runtime.GOARCH) {
		t.Skip("no Windows worker")
	}
	old := Version
	Version = "v2.15.0-beta.1"
	t.Cleanup(func() { Version = old })
	// The old Python sidecar environment must not route Energyplan to it.
	t.Setenv("FTW_OPTIMIZER_TRANSPORT", "unix")
	t.Setenv("FTW_OPTIMIZER_SOCKET", "/missing/python.sock")
	cfg, capacities := plannerEngineConfig(&config.Planner{Enabled: true})
	svc := buildMPC(cfg, nil, nil, capacities)
	if svc == nil || !svc.OptimizerBundledWithCore() {
		t.Fatalf("wrong beta wiring: %+v", svc)
	}
	t.Cleanup(func() { svc.Optimizer.Close() })
}

func TestBuildMPCWithoutHomeBattery(t *testing.T) {
	cfg, _ := plannerEngineConfig(&config.Planner{Enabled: true, Engine: "energyplan"})
	cfg.Drivers = nil
	svc := buildMPC(cfg, nil, nil, nil)
	if svc == nil || svc.Defaults.CapacityWh != 0 || svc.Defaults.InitialSoC != 0 || svc.Defaults.MaxChargeW != 0 || len(svc.BatteryFleet) != 0 {
		t.Fatalf("batteryless site invented storage: %+v", svc)
	}
	if svc.Optimizer != nil {
		defer svc.Optimizer.Close()
	}
}

func TestBuildMPCBatterylessEngineAdmission(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	for _, tc := range []struct {
		name, version, engine string
		want                  bool
	}{
		{"explicit Core in beta", "v3.1.0-beta.1", "core", false},
		{"stable default", "v3.1.0", "", false},
		{"development default", "dev", "", false},
		{"invalid beta default", "dev-beta.invalid", "", false},
		{"explicit Energyplan", "v3.1.0", "energyplan", true},
		{"beta default", "v3.1.0-beta.1", "", energyplanSupported(runtime.GOOS, runtime.GOARCH)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			Version = tc.version
			cfg, _ := plannerEngineConfig(&config.Planner{Enabled: true, Engine: tc.engine})
			cfg.Drivers = nil
			svc := buildMPC(cfg, nil, nil, nil)
			if svc != nil && svc.Optimizer != nil {
				defer svc.Optimizer.Close()
			}
			if (svc != nil) != tc.want {
				t.Fatalf("batteryless admission=%v, want %v", svc != nil, tc.want)
			}
			if svc != nil && (!svc.OptimizerBundledWithCore() || svc.Defaults.CapacityWh != 0) {
				t.Fatal("batteryless admission requires Energyplan without invented storage")
			}
		})
	}
}
