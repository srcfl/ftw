package mpc

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/srcfl/ftw/go/internal/state"
	"math"
	"sync/atomic"
	"testing"
	"time"
)

func TestPartialSlotCoreAndWorkerReplay(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprint("native=", native), func(t *testing.T) {
			var worker *ExternalOptimizer
			if native {
				worker = nativeWorker(t, 500*time.Millisecond)
				defer worker.Close()
			}
			for _, elapsed := range []time.Duration{0, 450 * time.Second, 750 * time.Second, 748939 * time.Millisecond, 899 * time.Second} {
				for _, ev := range []bool{false, true} {
					start := time.Date(2026, 9, 8, 4, 45, 0, 0, time.UTC)
					slots := []Slot{{StartMs: start.UnixMilli(), LenMin: 15, PriceOre: 100, SpotOre: 50, Confidence: 1, LoadW: 794.2886383422652}}
					if !trimFirstExecutionSlot(slots, start.Add(elapsed)) {
						t.Fatal("trim failed")
					}
					p := baseParams(ModeArbitrage)
					p.InitialSoC = .875
					p.CapacityWh = 9600
					p.SoCMin = .05
					p.SoCMax = .95
					p.ChargeEfficiency = .95
					p.DischargeEfficiency = .95
					p.MaxChargeW = 4800
					p.MaxDischargeW = 4800
					p.TerminalSoCPrice = 1000
					if ev {
						p.Loadpoint = &LoadpointSpec{ID: "ev", CapacityWh: 60000, Levels: 11, InitialSoC: .5, SoCMax: 1, PluggedIn: true, MaxChargeW: 6000, ChargeEfficiency: .9, AllowedStepsW: []float64{0, 3000, 6000}}
					}
					var plan Plan
					var err error
					if native {
						plan, err = worker.Optimize(context.Background(), slots, p)
					} else {
						plan, err = OptimizeContext(context.Background(), slots, p)
					}
					if err != nil {
						t.Fatal(elapsed, ev, err)
					}
					if err = ValidatePlan(slots, p, &plan); err != nil {
						t.Fatal(err)
					}
					a := plan.Actions[0]
					hours := (15*time.Minute - elapsed).Hours()
					if math.Abs(a.DurationHours()-hours) > 1e-12 || a.ExecutionStart() != start.Add(elapsed).UnixMilli() {
						t.Fatalf("wrong interval: %+v", a)
					}
					delta := a.BatteryW * hours
					if delta >= 0 {
						delta *= .95
					} else {
						delta /= .95
					}
					if math.Abs(a.SoC*9600-8400-delta) > 1e-6 {
						t.Fatalf("phantom energy: %+v", a)
					}
					if elapsed == 748939*time.Millisecond && a.SoC > .89493166 {
						t.Fatalf("04:57 plan exceeds physically available 201.415 AC Wh: %+v", a)
					}
					if ev && math.Abs(a.LoadpointSoC-(.5+a.LoadpointW*hours*.9/60000)) > 1e-6 {
						t.Fatal("EV time drift")
					}
					expectedCost := SlotGridCostOre(slots[0], a.GridW*hours/1000, p)
					if math.Abs(a.CostOre-expectedCost) > 1e-6 {
						t.Fatal("cost uses elapsed time")
					}
					// The active plan, diagnostic restore and command budgets retain exactly the same interval.
					svc := &Service{}
					svc.InstallPlan(plan, p, "ev")
					dir, ok := svc.SlotDirectiveAt(start.Add(elapsed))
					if !ok {
						t.Fatal("no directive")
					}
					if dir.SlotStart.UnixMilli() != a.ExecutionStart() || !dir.PriceSlotStart.Equal(start) || math.Abs(dir.BatteryEnergyWh-a.BatteryW*hours) > 1e-9 {
						t.Fatalf("directive=%+v", dir)
					}
					if ev && math.Abs(dir.LoadpointEnergyWh["ev"]-a.LoadpointW*hours) > 1e-9 {
						t.Fatal("EV budget time drift")
					}
					if elapsed > 0 {
						if _, ok := svc.SlotDirectiveAt(start); ok {
							t.Fatal("executes before decision")
						}
					}
				}
			}
		})
	}
}

func TestNativePartialFleetAndOldResponseRejected(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	defer o.Close()
	for _, counts := range [][2]int{{0, 0}, {0, 2}, {2, 0}, {2, 2}, {4, 3}} {
		slots, p := topologyFixture(counts[0], counts[1])
		slots[0].ExecutionStartMs = slots[0].StartMs + 57*60000 + 29000
		plan, err := o.Optimize(context.Background(), slots, p)
		if err != nil {
			t.Fatalf("%v: %v", counts, err)
		}
		if err = ValidatePlan(slots, p, &plan); err != nil {
			t.Fatal(err)
		}
		if plan.Actions[0].DurationHours() != 151./3600 {
			t.Fatal("wrong fleet interval")
		}
		// An older worker that ignores the extension cannot pass raw response admission.
		req := o.buildRequest(slots, p)
		payload, _ := json.Marshal(req)
		raw, err := o.transport.RoundTrip(context.Background(), payload)
		if err != nil {
			t.Fatal(err)
		}
		var response externalResponse
		if err = json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		response.Plan.Actions[0].ExecutionStartMs = 0
		if err = validateExternalAssets(req, response.Plan); err == nil {
			t.Fatal("worker dropped execution interval")
		}
	}
}

// Move the injected clock across a price boundary while the first solve runs.
// Only the retry may publish, and it must rebuild from the new interval.
func TestReplanCrossingSlotEndCannotPublish(t *testing.T) {
	var clockMs atomic.Int64
	var calls atomic.Int32
	start := time.Now().UTC().Truncate(time.Hour).Add(45 * time.Minute)
	clockMs.Store(start.Add(15*time.Minute - time.Second).UnixMilli())
	o := &partialClockOptimizer{optimize: func(slots []Slot, p Params) (Plan, error) {
		if calls.Add(1) == 1 {
			clockMs.Store(start.Add(15*time.Minute + time.Second).UnixMilli())
		}
		return OptimizeContext(context.Background(), slots, p)
	}}
	svc := newCancellationTestService(t, o)
	if err := svc.Store.SavePrices([]state.PricePoint{{Zone: "SE3", SlotTsMs: start.UnixMilli(), SlotLenMin: 15, SpotOreKwh: 40, TotalOreKwh: 90, Source: "test", FetchedAtMs: start.UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return time.UnixMilli(clockMs.Load()) }
	svc.Replan(context.Background())
	waitForRequestedReplans(t, svc)
	plan := svc.Latest()
	if plan == nil || calls.Load() != 2 {
		t.Fatalf("plan=%v calls=%d", plan, calls.Load())
	}
	if plan.Actions[0].SlotStartMs != start.Add(15*time.Minute).UnixMilli() || plan.Actions[0].ExecutionStart() != clockMs.Load() {
		t.Fatalf("published expired plan: %+v", plan.Actions[0])
	}
	if svc.PlanSnapshot().Reason != "slot_elapsed" {
		t.Fatal("missing retry provenance")
	}
}

type partialClockOptimizer struct {
	optimize func([]Slot, Params) (Plan, error)
}

func (o *partialClockOptimizer) Optimize(_ context.Context, s []Slot, p Params) (Plan, error) {
	return o.optimize(s, p)
}
func (*partialClockOptimizer) Close() error { return nil }

func Test0457PlanRejectsFullQuarterEnergy(t *testing.T) {
	start := time.Date(2026, 9, 8, 4, 45, 0, 0, time.UTC)
	slots := []Slot{{StartMs: start.UnixMilli(), LenMin: 15, ExecutionStartMs: start.Add(748939 * time.Millisecond).UnixMilli(), PriceOre: 162.6900225, SpotOre: 60.152018, LoadW: 794.2886383422652, Confidence: 1}}
	p := baseParams(ModeArbitrage)
	p.InitialSoC = .875
	p.CapacityWh = 9600
	p.SoCMax = .95
	p.MaxChargeW = 4800
	p.MaxDischargeW = 4800
	p.ChargeEfficiency = .95
	p.DischargeEfficiency = .95
	watts := 1514.315252387789
	grid := watts + slots[0].LoadW
	hours := 151.061 / 3600
	a := Action{SlotStartMs: slots[0].StartMs, SlotLenMin: 15, ExecutionStartMs: slots[0].ExecutionStartMs, BatteryW: watts, GridW: grid, SoC: .875 + watts*hours*.95/9600, CostOre: SlotGridCostOre(slots[0], grid*hours/1000, p)}
	plan := Plan{Actions: []Action{a}, TotalCostOre: a.CostOre}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	plan.Actions[0].SoC = .9124635283793855
	if err := ValidatePlan(slots, p, &plan); err == nil {
		t.Fatal("accepted the observed full-quarter phantom SoC")
	}
}

func TestPartialIntervalAdmissionRejectsInvalidStart(t *testing.T) {
	slots, p := externalTestFixture()
	for _, index := range []int{0, 1} {
		original := slots[index]
		for _, execution := range []int64{-1, original.StartMs + int64(original.LenMin)*60000, original.StartMs + 1} {
			if index == 0 && execution == original.StartMs+1 {
				continue
			}
			slots[index].ExecutionStartMs = execution
			if _, err := OptimizeContext(context.Background(), slots, p); err == nil {
				t.Fatal("invalid execution interval accepted")
			}
		}
		slots[index] = original
	}
}

func TestPartialTimeSurvivesFallbackShadowAndDiagnostic(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("fallback=", fail), func(t *testing.T) {
			var input []Slot
			o := &partialClockOptimizer{optimize: func(slots []Slot, p Params) (Plan, error) {
				input = append([]Slot(nil), slots...)
				if fail {
					return Plan{}, fmt.Errorf("injected worker failure")
				}
				return OptimizeContext(context.Background(), slots, p)
			}}
			svc := newCancellationTestService(t, o)
			now := time.Now().UTC().Truncate(time.Hour).Add(10 * time.Minute)
			svc.now = func() time.Time { return now }
			svc.Defaults.Mode = ModeArbitrage
			plan := svc.Replan(context.Background())
			if plan == nil {
				t.Fatal("no replacement plan")
			}
			if input[0].ExecutionStartMs != now.UnixMilli() || plan.Actions[0].ExecutionStart() != now.UnixMilli() {
				t.Fatal("partial time lost in fallback or primary")
			}
			if fail && (plan.Solver == nil || !plan.Solver.Fallback) {
				t.Fatal("fallback identity lost")
			}
			if !fail && (plan.DPShadow == nil || plan.DPEvaluationShadow == nil) {
				t.Fatal("missing comparisons")
			}
			diag := svc.Diagnose()
			restored, slots, params, _, ok := planFromDiagnostic(diag)
			if !ok || restored.Actions[0].ExecutionStart() != now.UnixMilli() || slots[0].ExecutionStart() != now.UnixMilli() {
				t.Fatal("diagnostic round trip lost execution time")
			}
			if diag.Slots[0].SlotStartMs == now.UnixMilli() {
				t.Fatal("diagnostic lost price identity")
			}
			if err := ValidatePlan(slots, params, restored); err != nil {
				t.Fatal(err)
			}
			expected := 0.0
			for i, a := range plan.Actions {
				duration := float64(a.SlotStartMs+int64(a.SlotLenMin)*60000-max(now.UnixMilli(), a.SlotStartMs)) / 3600000
				expected += SlotGridCostOre(input[i], a.GridW*duration/1000, svc.lastParams)
			}
			if math.Abs(plan.TotalCostOre-expected) > 1e-8 {
				t.Fatal("full-time cost survived partial model")
			}
		})
	}
}
