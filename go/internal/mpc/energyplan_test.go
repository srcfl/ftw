package mpc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
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
	if err != nil || info.Name != "ftw-solver" || info.Version != "0.2.1" {
		t.Fatalf("bundled worker health: %+v %v", info, err)
	}
	start := time.Now().UTC().Truncate(time.Hour)
	cloud := 10.0
	for i := 0; i < 4; i++ {
		if err := svc.Store.SaveForecasts([]state.ForecastPoint{{SlotTsMs: start.Add(time.Duration(i) * time.Hour).UnixMilli(), SlotLenMin: 60,
			FetchedAtMs: start.UnixMilli(), Source: "test", CloudCoverPct: &cloud}}); err != nil {
			t.Fatal(err)
		}
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
	for _, initial := range []float64{0, .025, .975, 1} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			o := nativeWorker(t, 500*time.Millisecond)
			t.Cleanup(func() { o.Close() })
			svc := shadowTestService(t)
			svc.Defaults.InitialSoC = initial
			svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: o}
			plan := svc.Replan(context.Background())
			if plan == nil || plan.Solver.Fallback || plan.Solver.Backend != "value_curve_rust" || plan.InitialSoC != initial {
				t.Fatalf("Energyplan must plan from real SoC without fallback: %+v", plan)
			}
			if err := ValidatePlan(svc.lastSlots, svc.lastParams, plan); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "recovery Core DP shadow", func() bool { return svc.Latest().DPShadow != nil })
			svc.shadowWG.Wait()
			shadow := svc.Latest().DPShadow
			if shadow.Solver.Engine != "core" || shadow.ComparedSlots != len(plan.Actions) {
				t.Fatalf("recovery shadow unavailable: %+v", shadow)
			}
		})
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

func TestNativeEnergyplanRecoveryWithEV(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	t.Cleanup(func() { o.Close() })
	for _, mode := range []Mode{ModeArbitrage, ModeSelfConsumption, ModePassiveArbitrage, ModeCheapCharge} {
		for _, initial := range []float64{.025, .975} {
			slots, p := nativeFixture()
			p.Mode, p.InitialSoC = mode, initial
			slots[0].PVW = -4500
			slots[0].PriceOre, slots[0].SpotOre = -10, -30
			plan, err := o.Optimize(context.Background(), slots, p)
			if err != nil {
				t.Fatalf("mode=%s soc=%f: %v", mode, initial, err)
			}
			if err := ValidatePlan(slots, p, &plan); err != nil {
				t.Fatalf("mode=%s soc=%f: %v", mode, initial, err)
			}
			if plan.InitialSoC != initial || plan.Solver.Fallback || plan.Actions[1].LoadpointSoC < p.Loadpoint.TargetSoC {
				t.Fatalf("recovery lost real energy or EV target: %+v", plan)
			}
		}
	}
}

func TestCoreDPShadowKeepsNewestPendingComparison(t *testing.T) {
	svc := shadowTestService(t)
	base := svc.Replan(context.Background())
	if base == nil {
		t.Fatal("no base plan")
	}
	svc.shadowBusy = true
	first := *base
	first.DecisionID = "first"
	svc.last = &first
	svc.startCoreDPShadow(first, svc.lastSlots, svc.lastParams, "first", 1)
	second := *base
	second.DecisionID = "second"
	svc.last = &second
	svc.startCoreDPShadow(second, svc.lastSlots, svc.lastParams, "second", 2)
	if svc.pendingCoreShadow == nil || svc.pendingCoreShadow.champion.DecisionID != "second" {
		t.Fatal("newest comparison was not retained")
	}
	svc.finishCoreDPShadow()
	svc.shadowWG.Wait()
	latest := svc.Latest()
	if latest.DecisionID != "second" || latest.DPShadow == nil || latest.DPShadow.ComparedSlots == 0 {
		t.Fatalf("newest comparison did not finish: %+v", latest.DPShadow)
	}
}

func TestCoreDPShadowCancellationPreservesPreviousComparison(t *testing.T) {
	svc := shadowTestService(t)
	slots, p := nativeBenchmarkFixture(true)
	p.SoCLevels, p.ActionLevels = 101, 201
	previous := &ShadowPlan{TotalCostOre: 123}
	champion := Plan{DecisionID: "same", DPShadow: previous}
	svc.last = &champion
	svc.startCoreDPShadow(champion, slots, p, "cancel", 0)
	svc.mu.Lock()
	svc.shadowCancel()
	svc.mu.Unlock()
	svc.shadowWG.Wait()
	if svc.Latest().DPShadow != previous {
		t.Fatal("cancellation replaced a comparison with rejection")
	}
}
