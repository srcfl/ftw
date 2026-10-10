package config

import (
	"regexp"
	"strings"
)

var plannerReleaseVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-beta\.[0-9]+)?$`)

// EngineForBuild applies the same release default for planner admission and
// API diagnostics. Explicit choices, including legacy aliases, take precedence.
// Beta and stable releases default to the bundled Energyplan worker on a
// supported host; development builds default to Core.
func (p *Planner) EngineForBuild(version, goos, goarch string) string {
	if p != nil && strings.TrimSpace(p.Engine) != "" {
		return p.EngineName()
	}
	if plannerReleaseVersion.MatchString(version) && SupportsBundledEnergyplan(goos, goarch) {
		return PlannerEngineEnergyplan
	}
	return PlannerEngineCore
}

// SupportsBundledEnergyplan is true on Linux ARM64 and AMD64, the only
// required Energyplan worker targets. macOS uses Core unless engine is set.
func SupportsBundledEnergyplan(goos, goarch string) bool {
	return goos == "linux" && (goarch == "amd64" || goarch == "arm64")
}
