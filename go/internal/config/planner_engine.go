package config

import (
	"regexp"
	"strings"
)

var plannerBetaVersion = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-beta\.[0-9]+$`)

// EngineForBuild applies the same release default for planner admission and
// API diagnostics. Explicit choices, including legacy aliases, take precedence.
func (p *Planner) EngineForBuild(version, goos, goarch string) string {
	if p != nil && strings.TrimSpace(p.Engine) != "" {
		return p.EngineName()
	}
	if plannerBetaVersion.MatchString(version) && SupportsBundledEnergyplan(goos, goarch) {
		return PlannerEngineEnergyplan
	}
	return PlannerEngineCore
}

func SupportsBundledEnergyplan(goos, goarch string) bool {
	return (goos == "linux" && (goarch == "amd64" || goarch == "arm64")) || (goos == "darwin" && goarch == "arm64")
}
