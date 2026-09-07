package config

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/optimizercontract"
)

func TestPlannerOptimizerTimeoutUsesSharedDefault(t *testing.T) {
	cfg, err := Parse([]byte(minimalYAML+"\nplanner:\n  enabled: true\n"), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Planner.OptimizerTimeout(); got != optimizercontract.DefaultTimeout {
		t.Fatalf("OptimizerTimeout = %s, want %s", got, optimizercontract.DefaultTimeout)
	}
	if got := cfg.Planner.OptimizerTimeoutS; got != optimizercontract.DefaultTimeout.Seconds() {
		t.Fatalf("OptimizerTimeoutS = %g, want %g", got, optimizercontract.DefaultTimeout.Seconds())
	}

	explicit := &Planner{OptimizerTimeoutS: 12.5}
	if got := explicit.OptimizerTimeout(); got != 12500*time.Millisecond {
		t.Fatalf("explicit OptimizerTimeout = %s, want 12.5s", got)
	}
}

func TestPlannerOptimizerConfigValidation(t *testing.T) {
	validWeight := 0.2
	serviceWeight := 1.0
	base := Config{Site: Site{SmoothingAlpha: 0.3}, Fuse: Fuse{MaxAmps: 16, Phases: 3, Voltage: 230}, Planner: &Planner{
		Engine: "python", OptimizerSolver: "HIGHS",
		OptimizerFormulation: "auto", OptimizerTimeoutS: 5,
		OptimizerIdleTimeoutS: 120,
		OptimizerMIPRelGap:    0.005, OptimizerCVaRWeight: &validWeight,
		OptimizerCVaRAlpha:        0.9,
		OptimizerChallengerPolicy: "multistage",
		OptimizerMultistage: &OptimizerMultistage{
			ScenarioLimit: 12, BranchIntervalSlots: 4, BranchHorizonSlots: 48,
			MaxBranching: 2, NearHorizonSlots: 16, MidHorizonSlots: 96,
			MidBlockSlots: 2, FarBlockSlots: 4, ServiceCVaRWeight: &serviceWeight,
			ServiceCVaRAlpha: 0.95, EconomicCVaRAlpha: 0.9,
			DecompositionThreshold: 20, DecompositionMethod: "auto",
			PHMaxIterations: 8, PHRho: 50, PHToleranceW: 5,
		},
	}}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid optimizer config: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Planner)
	}{
		{"engine", func(p *Planner) { p.Engine = "unknown" }},
		{"solver", func(p *Planner) { p.OptimizerSolver = "SCIP" }},
		{"formulation", func(p *Planner) { p.OptimizerFormulation = "nonlinear" }},
		{"timeout", func(p *Planner) { p.OptimizerTimeoutS = -1 }},
		{"idle timeout", func(p *Planner) { p.OptimizerIdleTimeoutS = -1 }},
		{"cvar alpha", func(p *Planner) { p.OptimizerCVaRAlpha = 1 }},
		{"recourse prefix", func(p *Planner) { p.OptimizerRecourseNonAnticipativeSlots = -1 }},
		{"challenger policy", func(p *Planner) { p.OptimizerChallengerPolicy = "clairvoyant" }},
		{"multistage branching", func(p *Planner) { p.OptimizerMultistage.MaxBranching = 1 }},
		{"multistage service weight", func(p *Planner) {
			negative := -1.0
			p.OptimizerMultistage.ServiceCVaRWeight = &negative
		}},
		{"multistage alpha", func(p *Planner) { p.OptimizerMultistage.ServiceCVaRAlpha = 1 }},
		{"multistage decomposition", func(p *Planner) { p.OptimizerMultistage.DecompositionMethod = "benders" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := *base.Planner
			ms := *base.Planner.OptimizerMultistage
			p.OptimizerMultistage = &ms
			tt.mutate(&p)
			cfg := Config{Site: base.Site, Fuse: base.Fuse, Planner: &p}
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// TestPlannerEngineDefaultsToCore pins the #1020 flip at the configuration
// boundary: an unset engine is Core, "python" is the explicit legacy opt-out,
// and the spellings existing configs already carry still resolve.
func TestPlannerEngineDefaultsToCore(t *testing.T) {
	tests := map[string]string{
		"":           PlannerEngineCore,
		"core":       PlannerEngineCore,
		"go":         PlannerEngineCore,
		"dp":         PlannerEngineCore,
		"Core":       PlannerEngineCore,
		"python":     PlannerEnginePython,
		"PYTHON":     PlannerEnginePython,
		"Energyplan": PlannerEngineEnergyplan,
	}
	for value, want := range tests {
		p := &Planner{Enabled: true, Engine: value}
		if got := p.EngineName(); got != want {
			t.Errorf("engine %q resolved to %q, want %q", value, got, want)
		}
		cfg := Config{Site: Site{SmoothingAlpha: 0.3},
			Fuse: Fuse{MaxAmps: 16, Phases: 3, Voltage: 230}, Planner: p}
		if err := cfg.Validate(); err != nil {
			t.Errorf("engine %q rejected: %v", value, err)
		}
	}
	if got := (*Planner)(nil).EngineName(); got != PlannerEngineCore {
		t.Errorf("nil planner engine = %q, want core", got)
	}
}

// TestPlannerShadowPythonDefaultsOn — the soak measurement is on unless an
// operator turns it off, and "off" survives being written down.
func TestPlannerShadowPythonDefaultsOn(t *testing.T) {
	if !(&Planner{}).ShadowPythonEnabled() {
		t.Error("unset shadow_python should default on")
	}
	if !(*Planner)(nil).ShadowPythonEnabled() {
		t.Error("nil planner should default on")
	}
	off := false
	if (&Planner{ShadowPython: &off}).ShadowPythonEnabled() {
		t.Error("explicit shadow_python: false was ignored")
	}
	cfg, err := Parse([]byte(minimalYAML+"\nplanner:\n  enabled: true\n  shadow_python: false\n"), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Planner.ShadowPythonEnabled() {
		t.Error("shadow_python: false did not survive parsing")
	}
}
