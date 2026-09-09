package config

import "testing"

func TestPlannerEngineForBuild(t *testing.T) {
	for _, tc := range []struct {
		version, engine, goos, goarch, want string
	}{
		{"v3.1.0-beta.1", "", "linux", "arm64", PlannerEngineEnergyplan},
		{"v3.1.0-beta.1", "", "linux", "amd64", PlannerEngineEnergyplan},
		{"v3.1.0-beta.1", "", "darwin", "arm64", PlannerEngineCore},
		{"v3.1.0-beta.1", "", "windows", "amd64", PlannerEngineCore},
		{"v3.1.0-beta.1", "", "linux", "arm", PlannerEngineCore},
		{"v3.1.0-beta.1", "core", "linux", "arm64", PlannerEngineCore},
		{"v3.1.0", "", "linux", "arm64", PlannerEngineCore},
		{"dev", "", "linux", "arm64", PlannerEngineCore},
		{"dev-beta.invalid", "", "linux", "arm64", PlannerEngineCore},
		{"v3.1.0-beta.", "", "linux", "arm64", PlannerEngineCore},
		{"v3.1.0-beta.1-extra", "", "linux", "arm64", PlannerEngineCore},
		{"v3.1.0-rc.1", "", "linux", "arm64", PlannerEngineCore},
		{"v3.1.0", "Energyplan", "linux", "arm64", PlannerEngineEnergyplan},
		{"dev", "python", "linux", "arm64", PlannerEngineEnergyplan},
	} {
		p := &Planner{Engine: tc.engine}
		if got := p.EngineForBuild(tc.version, tc.goos, tc.goarch); got != tc.want {
			t.Errorf("%+v: engine=%s", tc, got)
		}
	}
}
