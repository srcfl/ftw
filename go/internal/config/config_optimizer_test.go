package config

import "testing"

func TestPlannerOptimizerConfigValidation(t *testing.T) {
	validWeight := 0.2
	base := Config{Site: Site{SmoothingAlpha: 0.3}, Fuse: Fuse{MaxAmps: 16, Phases: 3, Voltage: 230}, Planner: &Planner{
		Engine: "python", OptimizerSolver: "HIGHS",
		OptimizerFormulation: "auto", OptimizerTimeoutS: 5,
		OptimizerMIPRelGap: 0.005, OptimizerCVaRWeight: &validWeight,
		OptimizerCVaRAlpha: 0.9,
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
		{"cvar alpha", func(p *Planner) { p.OptimizerCVaRAlpha = 1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := *base.Planner
			tt.mutate(&p)
			cfg := Config{Site: base.Site, Fuse: base.Fuse, Planner: &p}
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
