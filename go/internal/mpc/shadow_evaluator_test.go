package mpc

import (
	"math"
	"testing"
	"time"
)

func TestStatefulShadowEvaluatorScoresSameRealizationWithIndependentSoC(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	slots := []Slot{{
		StartMs: start.UnixMilli(), LenMin: 60, PriceOre: 100, SpotOre: 50,
		Limits: PowerLimits{MaxImportW: 8000, MaxExportW: 8000},
	}}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000, InitialSoCPct: 50,
		SoCMinPct: 10, SoCMaxPct: 90, MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 1, DischargeEfficiency: 1,
	}
	champion := &Plan{Actions: []Action{{SlotStartMs: start.UnixMilli(), SlotLenMin: 60, BatteryW: 1000}}}
	challenger := &Plan{Actions: []Action{{SlotStartMs: start.UnixMilli(), SlotLenMin: 60, BatteryW: -1000}}}

	evaluator := newStatefulShadowEvaluator()
	evaluator.SetPlans(champion, challenger, slots, p, start)
	score := evaluator.Observe(start, 1000, 0)
	for i := 1; i <= 30; i++ {
		score = evaluator.Observe(start.Add(time.Duration(i)*time.Minute), 1000, 0)
	}

	if math.Abs(score.ChampionCostOre-100) > 1e-6 {
		t.Fatalf("champion cost = %v, want 100", score.ChampionCostOre)
	}
	if math.Abs(score.ChallengerCostOre) > 1e-6 || math.Abs(score.ChallengerMinusChampionOre+100) > 1e-6 {
		t.Fatalf("unexpected challenger score: %+v", score)
	}
	if math.Abs(score.ChampionVirtualSoCPct-55) > 1e-6 || math.Abs(score.ChallengerVirtualSoCPct-45) > 1e-6 {
		t.Fatalf("virtual SoC did not evolve independently: %+v", score)
	}
}

func TestStatefulShadowEvaluatorProjectsModeAndEnergyBounds(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	slots := []Slot{{StartMs: start.UnixMilli(), LenMin: 60, PriceOre: 100, SpotOre: 50}}
	p := Params{
		Mode: ModePassiveArbitrage, CapacityWh: 10000, InitialSoCPct: 10,
		SoCMinPct: 10, SoCMaxPct: 90, MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 1, DischargeEfficiency: 1,
	}
	plan := &Plan{Actions: []Action{{SlotStartMs: start.UnixMilli(), SlotLenMin: 60, BatteryW: -5000}}}
	evaluator := newStatefulShadowEvaluator()
	evaluator.SetPlans(plan, plan, slots, p, start)
	score := evaluator.Observe(start, 500, 0)
	for i := 1; i <= 30; i++ {
		score = evaluator.Observe(start.Add(time.Duration(i)*time.Minute), 500, 0)
	}

	if score.ChampionClampCount != 30 || score.ChallengerClampCount != 30 {
		t.Fatalf("expected SoC/mode clamps: %+v", score)
	}
	if math.Abs(score.ChampionVirtualSoCPct-10) > 1e-6 || math.Abs(score.ChallengerVirtualSoCPct-10) > 1e-6 {
		t.Fatalf("minimum SoC was violated: %+v", score)
	}
}

func TestStatefulShadowEvaluatorRestoresPersistedRun(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	p := Params{Mode: ModeArbitrage, CapacityWh: 10000, InitialSoCPct: 50, SoCMinPct: 10, SoCMaxPct: 90, MaxChargeW: 5000, MaxDischargeW: 5000, ChargeEfficiency: 1, DischargeEfficiency: 1}
	slots := []Slot{{StartMs: now.UnixMilli(), LenMin: 60, PriceOre: 100}}
	plan := &Plan{Actions: []Action{{SlotStartMs: now.UnixMilli(), SlotLenMin: 60}}}
	persisted := &ShadowEvaluation{
		StartedAtMs: 123, Samples: 42, ChampionCostOre: 12, ChallengerCostOre: 10,
		ChampionStorageEnergyWh:   map[string]float64{"home-battery": 4300},
		ChallengerStorageEnergyWh: map[string]float64{"home-battery": 4700},
	}
	evaluator := newStatefulShadowEvaluator()
	evaluator.Restore(persisted)
	evaluator.SetPlans(plan, plan, slots, p, now)
	score := evaluator.Snapshot()
	if score.StartedAtMs != 123 || score.Samples != 42 || score.ChampionVirtualSoCPct != 43 || score.ChallengerVirtualSoCPct != 47 {
		t.Fatalf("persisted evaluation was not resumed: %+v", score)
	}
}

func TestShadowEvaluatorNeverScoresAnticipativeRecourseTail(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	plan := &Plan{
		Solver: &SolverInfo{ScenarioPolicy: "recourse", NonAnticipativeSlots: 1},
		Actions: []Action{
			{SlotStartMs: start.UnixMilli(), SlotLenMin: 15},
			{SlotStartMs: start.Add(15 * time.Minute).UnixMilli(), SlotLenMin: 15},
		},
	}
	if _, ok := shadowActionAt(plan, start.Add(time.Minute)); !ok {
		t.Fatal("shared recourse prefix was not scoreable")
	}
	if _, ok := shadowActionAt(plan, start.Add(16*time.Minute)); ok {
		t.Fatal("anticipative recourse tail was scored as executable")
	}
}

func TestShadowEvaluatorDoesNotInventRecoveryFromInitialBandViolation(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	p := Params{Mode: ModeArbitrage, CapacityWh: 10000, InitialSoCPct: 5, SoCMinPct: 10, SoCMaxPct: 90, MaxChargeW: 5000, MaxDischargeW: 5000, ChargeEfficiency: 1, DischargeEfficiency: 1}
	slots := []Slot{{StartMs: start.UnixMilli(), LenMin: 60, PriceOre: 100}}
	plan := &Plan{Actions: []Action{{SlotStartMs: start.UnixMilli(), SlotLenMin: 60, BatteryW: -1000}}}
	evaluator := newStatefulShadowEvaluator()
	evaluator.SetPlans(plan, plan, slots, p, start)
	_ = evaluator.Observe(start, 500, 0)
	score := evaluator.Observe(start.Add(time.Minute), 500, 0)
	if score.ChampionVirtualSoCPct != 5 || score.ChallengerVirtualSoCPct != 5 {
		t.Fatalf("evaluator invented operating-band recovery: %+v", score)
	}
	if score.ChampionClampCount != 1 || score.ChallengerClampCount != 1 {
		t.Fatalf("below-min discharge was not blocked: %+v", score)
	}
}
