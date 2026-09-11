package main

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/srcfl/ftw/go/internal/config"
)

func plannerEngine(pl *config.Planner, version string) string {
	return pl.EngineForBuild(version, runtime.GOOS, runtime.GOARCH)
}

func resolveEnergyplanBinary() string {
	name := "ftw-solver-" + runtime.GOOS + "-" + runtime.GOARCH
	candidates := []string{
		"optimizer/native/bundle/" + name,
		"../optimizer/native/bundle/" + name,
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append([]string{filepath.Join(filepath.Dir(exe), "optimizer/native/bundle", name)}, candidates...)
	}
	for _, candidate := range candidates {
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
			absolute, err := filepath.Abs(candidate)
			if err == nil {
				return absolute
			}
		}
	}
	// Keep an unavailable primary attached: its failure produces an explicit
	// fallback reason instead of silently changing the configured engine.
	return filepath.Join("/app/optimizer/native/bundle", name)
}

func energyplanSupported(goos, goarch string) bool {
	return config.SupportsBundledEnergyplan(goos, goarch)
}
