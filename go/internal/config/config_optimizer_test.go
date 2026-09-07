package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRetiredPythonConfigMigratesToEnergyplan(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML+`
planner:
  enabled: true
  engine: PYTHON
  shadow_python: true
  optimizer_command: /missing/python
  optimizer_transport: unix
  optimizer_socket: /missing/socket
  optimizer_multistage:
    scenario_limit: 12
`), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Planner.Engine != PlannerEngineEnergyplan {
		t.Fatalf("engine = %q", cfg.Planner.Engine)
	}
	b, err := json.Marshal(cfg.Planner)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "optimizer_") || strings.Contains(string(b), "shadow_python") {
		t.Fatalf("retired settings returned: %s", b)
	}
}

func TestPlannerEngineSelection(t *testing.T) {
	for value, want := range map[string]string{"": "core", "core": "core", "go": "core", "dp": "core", "Core": "core", "python": "energyplan", "PYTHON": "energyplan", "Energyplan": "energyplan"} {
		p := &Planner{Enabled: true, Engine: value}
		if got := p.EngineName(); got != want {
			t.Errorf("%q resolved to %q, want %q", value, got, want)
		}
		cfg := Config{Site: Site{SmoothingAlpha: .3}, Fuse: Fuse{MaxAmps: 16, Phases: 3, Voltage: 230}, Planner: p}
		if err := cfg.Validate(); err != nil {
			t.Errorf("%q rejected: %v", value, err)
		}
	}
	cfg, err := Parse([]byte(minimalYAML+"\nplanner:\n  engine: unknown\n"), "/tmp")
	if err == nil {
		t.Fatalf("unknown engine accepted: %+v", cfg)
	}
}
