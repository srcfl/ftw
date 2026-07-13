package config

import "testing"

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
