package mpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
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
	template := nativeWorker(t, 500*time.Millisecond)
	defer template.Close()
	o, err := NewEnergyplanOptimizer(template.cfg.Command[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	svc := shadowTestService(t)
	svc.Optimizer = o
	info, err := svc.Optimizer.(*EnergyplanOptimizer).Health(context.Background())
	if err != nil || info.Name != "ftw-solver" || info.Version != "0.4.1" {
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
	svc.PVRelativeUncertainty = func() float64 { return .1 }
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
		// The learned relative error replaces the absolute fallback: 1500 - 10%.
		if math.Abs(slot.PVW-(-1350)) > .001 {
			t.Fatalf("wrong downside PV: %f", slot.PVW)
		}
	}
	if len(input.Scenarios) != 0 || input.Settings.CVaRWeight != 0 {
		t.Fatal("Energyplan received scenarios")
	}
	if input.Settings.TimeLimitS != .5 {
		t.Fatalf("deterministic downside request budget=%g, want 0.5", input.Settings.TimeLimitS)
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

func TestNativeEnergyplanUsesBoundedFleetBudget(t *testing.T) {
	template := nativeWorker(t, time.Second)
	defer template.Close()
	engine, err := NewEnergyplanOptimizer(template.cfg.Command[0])
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	slots, params := topologyFixture(2, 2)
	first := slots[0]
	slots = make([]Slot, 193)
	for i := range slots {
		slots[i] = first
		slots[i].StartMs = first.StartMs + int64(i*first.LenMin)*60000
	}
	plan, err := engine.Optimize(context.Background(), slots, params)
	if err != nil {
		t.Fatal(err)
	}
	var request externalRequest
	if err := json.Unmarshal(plan.OptimizerInput, &request); err != nil {
		t.Fatal(err)
	}
	if request.Settings.TimeLimitS != 5 {
		t.Fatalf("fleet request has wrong budget: %v", request.Settings.TimeLimitS)
	}
	if err := ValidatePlan(slots, params, &plan); err != nil {
		t.Fatal(err)
	}
}

func TestBatterylessEVBudgetDoesNotInventStorage(t *testing.T) {
	slots, p := topologyFixture(0, 2)
	p.PVUncertaintyW, p.PVRelativeUncertainty = 200, .1
	horizon := make([]Slot, 193)
	for i := range horizon {
		horizon[i] = slots[0]
	}
	if got := energyplanTimeBudget(horizon, p); got != 500*time.Millisecond {
		t.Fatalf("batteryless EV budget=%v", got)
	}
	p.CapacityWh = 20000
	if got := energyplanTimeBudget(horizon, p); got != 5*time.Second {
		t.Fatalf("real aggregate battery budget=%v", got)
	}
}

func TestVillaBatteryAndEVGetsFleetBudget(t *testing.T) {
	slots, p := topologyFixture(1, 1)
	horizon := make([]Slot, 193)
	for i := range horizon {
		horizon[i] = slots[0]
	}
	if got := energyplanTimeBudget(horizon, p); got != energyplanFleetBudget {
		t.Fatalf("1b+1EV 193-slot budget=%v, want fleet", got)
	}
}

func energyplanFleetHorizon() ([]Slot, Params) {
	slots, params := topologyFixture(2, 2)
	first := slots[0]
	horizon := make([]Slot, 193)
	for i := range horizon {
		horizon[i] = first
		horizon[i].StartMs = first.StartMs + int64(i*first.LenMin)*60000
	}
	return horizon, params
}

func trimFirstSlotRemaining(slots []Slot, remaining time.Duration) {
	end := slots[0].StartMs + int64(slots[0].LenMin)*60000
	slots[0].ExecutionStartMs = end - remaining.Milliseconds()
}

type countingTransport struct {
	n            int
	last         []byte
	deadlineWait time.Duration
	hasDeadline  bool
}

func (c *countingTransport) RoundTrip(ctx context.Context, payload []byte) ([]byte, error) {
	c.n++
	c.last = append([]byte(nil), payload...)
	if d, ok := ctx.Deadline(); ok {
		c.hasDeadline = true
		c.deadlineWait = time.Until(d)
	}
	return nil, errors.New("stop")
}
func (c *countingTransport) Health(context.Context) (OptimizerRuntimeInfo, error) {
	return OptimizerRuntimeInfo{}, nil
}
func (c *countingTransport) Close() error { return nil }

func TestEnergyplanTimeBudgetRespectsRemainingSlot(t *testing.T) {
	t.Parallel()
	slots, p := energyplanFleetHorizon()
	if got := energyplanTimeBudget(slots, p); got != energyplanFleetBudget {
		t.Fatalf("full-slot fleet budget=%v, want %v", got, energyplanFleetBudget)
	}
	trimFirstSlotRemaining(slots, 200*time.Millisecond)
	if got := energyplanTimeBudget(slots, p); got != 0 {
		t.Fatalf("200ms remaining fleet budget=%v, want fail-fast", got)
	}
	trimFirstSlotRemaining(slots, 2*time.Second)
	got := energyplanTimeBudget(slots, p)
	remaining := remainingFirstSlot(slots)
	if got <= 0 || got > remaining || remaining-got != energyplanPublishMargin {
		t.Fatalf("2s remaining fleet budget=%v remaining=%v", got, remaining)
	}
	small, smallParams := topologyFixture(1, 0)
	trimFirstSlotRemaining(small, 2*time.Second)
	if got := energyplanTimeBudget(small, smallParams); got != energyplanSmallBudget {
		t.Fatalf("2s remaining small budget=%v, want %v", got, energyplanSmallBudget)
	}
}

func TestEnergyplanFailsFastWhenRemainingBelowSmallBudget(t *testing.T) {
	t.Parallel()
	capture := &countingTransport{}
	engine := &EnergyplanOptimizer{ExternalOptimizer: &ExternalOptimizer{
		cfg:        ExternalOptimizerConfig{Timeout: 7 * time.Second},
		transport:  capture,
		timeBudget: energyplanTimeBudget,
	}}
	slots, p := energyplanFleetHorizon()
	trimFirstSlotRemaining(slots, 200*time.Millisecond)
	_, err := engine.Optimize(context.Background(), slots, p)
	if err == nil || !strings.Contains(err.Error(), "below the Energyplan budget") {
		t.Fatalf("expected fail-fast, got %v", err)
	}
	if capture.n != 0 {
		t.Fatalf("doomed worker started %d times", capture.n)
	}
}

func TestEnergyplanCapsWorkerBudgetAndWaitToRemaining(t *testing.T) {
	t.Parallel()
	capture := &countingTransport{}
	engine := &EnergyplanOptimizer{ExternalOptimizer: &ExternalOptimizer{
		cfg:        ExternalOptimizerConfig{Timeout: 7 * time.Second},
		transport:  capture,
		timeBudget: energyplanTimeBudget,
	}}
	slots, p := energyplanFleetHorizon()
	trimFirstSlotRemaining(slots, 2*time.Second)
	_, _ = engine.Optimize(context.Background(), slots, p)
	if capture.n != 1 {
		t.Fatalf("worker trips=%d", capture.n)
	}
	var request externalRequest
	if err := json.Unmarshal(capture.last, &request); err != nil {
		t.Fatal(err)
	}
	want := energyplanTimeBudget(slots, p).Seconds()
	if request.Settings.TimeLimitS != want || request.Settings.TimeLimitS > 2 {
		t.Fatalf("TimeLimitS=%g, want %g (≤ remaining)", request.Settings.TimeLimitS, want)
	}
	if !capture.hasDeadline || capture.deadlineWait > 2*time.Second || capture.deadlineWait < time.Second {
		t.Fatalf("process wait %v, want ≤ remaining 2s", capture.deadlineWait)
	}
}

func TestEnergyplanLateFleetReplanDoesNotStartDoomedSolve(t *testing.T) {
	capture := &countingTransport{}
	engine := &EnergyplanOptimizer{ExternalOptimizer: &ExternalOptimizer{
		cfg:        ExternalOptimizerConfig{Timeout: 7 * time.Second},
		transport:  capture,
		timeBudget: energyplanTimeBudget,
	}}
	start := time.Date(2026, 9, 8, 4, 45, 0, 0, time.UTC)
	now := start.Add(15*time.Minute - 200*time.Millisecond)
	st, err := state.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for i := 0; i < 16; i++ {
		slot := start.Add(time.Duration(i) * 15 * time.Minute)
		if err := st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: slot.UnixMilli(), SlotLenMin: 15,
			SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: start.UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	tele := telemetry.NewStore()
	soc := 0.5
	svc := New(st, tele, "SE3", Params{
		Mode: ModeArbitrage, SoCLevels: 11, ActionLevels: 5,
		CapacityWh: 10000, InitialSoC: 0.5, SoCMin: 0.1, SoCMax: 0.95,
		MaxChargeW: 4000, MaxDischargeW: 4000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.now = func() time.Time { return now }
	svc.Optimizer = engine
	svc.Horizon = 4 * time.Hour
	svc.BaseLoad = 500
	for _, id := range []string{"battery-0", "battery-1"} {
		tele.Update(id, telemetry.DerBattery, 0, &soc, nil)
		tele.DriverHealthMut(id).RecordSuccess()
	}
	svc.UpdateBatteryFleet([]BatteryFleetMember{
		{Driver: "battery-0", CapacityWh: 5000, MaxChargeW: 2000, MaxDischargeW: 2000},
		{Driver: "battery-1", CapacityWh: 5000, MaxChargeW: 2000, MaxDischargeW: 2000},
	}, 10000, 4000, 4000)
	previous := Plan{DecisionID: "keep-me", GeneratedAtMs: now.UnixMilli(), Actions: []Action{{SlotStartMs: start.UnixMilli(), SlotLenMin: 15}}}
	svc.InstallPlan(previous, svc.Defaults, "")
	plan := svc.Replan(context.Background())
	if capture.n != 0 {
		t.Fatalf("doomed worker started %d times", capture.n)
	}
	if svc.PlanSnapshot().Reason == "slot_elapsed" {
		t.Fatal("queued slot_elapsed from a plan that could not land")
	}
	if plan == nil || plan.DecisionID != "keep-me" {
		t.Fatalf("wanted previous plan, got %+v", plan)
	}
}
