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
		{"v2.15.0-beta.1", "python", "python"},
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
	if svc == nil || !svc.OptimizerBundledWithCore() || svc.ShadowOptimizer != nil || svc.EnableRecourseShadow {
		t.Fatalf("wrong beta wiring: %+v", svc)
	}
	t.Cleanup(func() { svc.Optimizer.Close() })
}
