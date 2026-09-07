package mpc

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidatePlanRejectsEVOverCapacity(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 60, PriceOre: 100, Confidence: 1}}
	p := baseParams(ModeArbitrage)
	p.MaxChargeW, p.MaxDischargeW = 0, 0
	p.Loadpoint = &LoadpointSpec{ID: "ev", Levels: 11, CapacityWh: 10000, InitialSoC: .95, SoCMax: 1,
		PluggedIn: true, MaxChargeW: 1000, ChargeEfficiency: 1, AllowedStepsW: []float64{0, 1000}}
	plan := Plan{TotalCostOre: 100, Actions: []Action{{SlotStartMs: 1, SlotLenMin: 60,
		SoC: p.InitialSoC, LoadpointW: 1000, LoadpointSoC: 1.05, GridW: 1000, CostOre: 100}}}
	if err := ValidatePlan(slots, p, &plan); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("EV overflow accepted or wrong rejection: %v", err)
	}
}

func TestCoreDPNearCeilingReplaysPower(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 15, Confidence: 1}}
	p := Params{Mode: ModeArbitrage, InitialSoC: .947, CapacityWh: 20000, SoCMin: .1, SoCMax: .95,
		ChargeEfficiency: .95, DischargeEfficiency: .95, MaxChargeW: 9000, MaxDischargeW: 9000,
		TerminalSoCPrice: 160, SoCLevels: 201, ActionLevels: 401}
	plan := Optimize(slots, p)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	if got := 18940 + plan.Actions[0].BatteryW*.25*.95; got > 19000+1e-6 {
		t.Fatalf("battery overflow: %f Wh", got)
	}
}

func TestCoreDPShadowCancellation(t *testing.T) {
	slots, p := nativeBenchmarkFixture(true)
	p.SoCLevels, p.ActionLevels = 101, 201
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := OptimizeContext(ctx, slots, p); err == nil {
		t.Fatal("DP ignored timeout")
	}
	if time.Since(start) > time.Second {
		t.Fatal("DP did not stop promptly")
	}
}

func TestNativeEnergyplanDownsideAndAsyncShadow(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	t.Cleanup(func() { o.Close() })
	svc := shadowTestService(t)
	svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: o}
	info, err := svc.Optimizer.(*EnergyplanOptimizer).Health(context.Background())
	if err != nil || info.Name != "ftw-solver" || info.Version != "0.1.1" {
		t.Fatalf("bundled worker health: %+v %v", info, err)
	}
	svc.PVUncertaintyW = func() float64 { return 200 }
	svc.PVForecastSafetyK = 1
	svc.PV = func(time.Time, float64) float64 { return 1500 }
	var published atomic.Bool
	var measured atomic.Bool
	svc.SaveDiag = func(d *Diagnostic, _ string) error {
		if d.DPShadow == nil {
			published.Store(true)
		} else {
			if !published.Load() {
				t.Error("shadow ran before publication")
			}
			measured.Store(true)
		}
		return nil
	}
	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver.Fallback || plan.Solver.Backend != "value_curve_rust" {
		t.Fatalf("Energyplan inactive: %+v", plan)
	}
	before, _ := json.Marshal(plan.Actions)
	waitFor(t, "Core DP shadow", func() bool { return measured.Load() })
	svc.shadowWG.Wait()
	d := svc.Diagnose()
	if d.DPShadow == nil || d.DPShadow.Solver.Engine != "core" || d.DPShadow.ComparedSlots == 0 {
		t.Fatalf("missing Core comparison: %+v", d.DPShadow)
	}
	if math.Abs(d.DPShadow.TotalCostOre) < .001 {
		t.Fatal("shadow grid cost missing")
	}
	after, _ := json.Marshal(svc.Latest().Actions)
	if string(before) != string(after) {
		t.Fatal("shadow changed active actions")
	}
	var input externalRequest
	if err := json.Unmarshal(plan.OptimizerInput, &input); err != nil {
		t.Fatal(err)
	}
	for _, slot := range input.Slots {
		if math.Abs(slot.PVW-(-1300)) > .001 {
			t.Fatalf("wrong downside PV: %f", slot.PVW)
		}
	}
	if len(input.Scenarios) != 0 || input.Settings.CVaRWeight != 0 {
		t.Fatal("Energyplan received scenarios")
	}
}

func TestCoreDPShadowDoesNotAttachToNewerPlan(t *testing.T) {
	svc := shadowTestService(t)
	old := Plan{DecisionID: "old"}
	svc.last = &Plan{DecisionID: "new"}
	svc.recordCoreDPShadow(old, nil, Params{}, "test", 0, &ShadowPlan{TotalCostOre: 123})
	if svc.Latest().DPShadow != nil {
		t.Fatal("old comparison attached to new plan")
	}
}

func TestNativeEnergyplanRecoveryKeepsRealBatteryEnergy(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	t.Cleanup(func() { o.Close() })
	svc := shadowTestService(t)
	svc.Defaults.InitialSoC = .025
	svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: o}
	plan := svc.Replan(context.Background())
	if plan == nil || !plan.Solver.Fallback || plan.InitialSoC != .025 {
		t.Fatalf("recovery must keep real SoC and report fallback: %+v", plan)
	}
	if err := ValidatePlan(svc.lastSlots, svc.lastParams, plan); err != nil {
		t.Fatal(err)
	}
	if plan.Actions[0].BatteryW < 0 {
		t.Fatal("recovery discharged below floor")
	}
}

func TestNativeEnergyplanRejectsUnsafeFallback(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	t.Cleanup(func() { o.Close() })
	svc := shadowTestService(t)
	svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: o}
	svc.BaseLoad, svc.FuseMaxW = 20000, 1000
	if plan := svc.Replan(context.Background()); plan != nil {
		t.Fatalf("infeasible worker and unsafe DP fallback published: %+v", plan)
	}
}
