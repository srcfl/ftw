package mpc

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// The optional native process is pinned and checked by make native-solver-test.
// Ordinary Go tests remain usable without a Rust toolchain.
func nativeWorker(t testing.TB, budget time.Duration) *ExternalOptimizer {
	t.Helper()
	binary := os.Getenv("FTW_NATIVE_SOLVER")
	if binary == "" {
		t.Skip("run make native-solver-test to include the Rust worker")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("FTW_NATIVE_SOLVER must be an absolute executable path")
	}
	o, err := NewExternalOptimizer(ExternalOptimizerConfig{Command: []string{binary, "--time-limit=" + budget.String()}, ModuleDir: filepath.Dir(binary), Timeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func nativeFixture() ([]Slot, Params) {
	slots, p := externalTestFixture()
	p.SoCLevels, p.ActionLevels = 41, 21
	p.Loadpoint = &LoadpointSpec{ID: "garage", CapacityWh: 10000, Levels: 11, SoCMax: 1, InitialSoC: .2, PluggedIn: true, TargetSoC: .35, TargetSlotIdx: 1, MaxChargeW: 2000, AllowedStepsW: []float64{0, 1400, 2000}, ChargeEfficiency: .9, NoBatteryToEV: true}
	return slots, p
}

func TestNativeProcessCoreContract(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	defer o.Close()
	slots, p := nativeFixture()
	floor := -5.0
	p.ExportBonusOreKwh, p.ExportFeeOreKwh, p.ExportFloorOreKwh = 12, 3, &floor
	p.PVChargeBonusOreKwh, p.MinArbitrageSpreadOreKwh = 30, 20
	slots[0].Confidence, slots[1].Confidence = .4, .9
	slots[0].PVW = -4500
	for _, mode := range []Mode{ModeArbitrage, ModeSelfConsumption, ModePassiveArbitrage, ModeCheapCharge} {
		p.Mode = mode
		plan, err := o.Optimize(context.Background(), slots, p)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if err := ValidatePlan(slots, p, &plan); err != nil {
			t.Fatal(err)
		}
		if plan.Solver.Backend != "value_curve_rust" || plan.Actions[1].LoadpointSoC < p.Loadpoint.TargetSoC {
			t.Fatalf("unexpected plan: %+v", plan)
		}
		for _, a := range plan.Actions {
			if a.PVLimitW != 0 {
				t.Fatal("unexpected curtailment")
			}
		}
	}
	p.PVForecastSafetyK, p.PVUncertaintyW = 1, 300
	if plan, err := o.Optimize(context.Background(), slots, p); err != nil || plan.Solver.ScenarioCount != 3 {
		t.Fatalf("scenario model failed: plan=%+v err=%v", plan.Solver, err)
	}
	p.PVForecastSafetyK = 0
	if _, err := o.Optimize(context.Background(), slots, p); err != nil {
		t.Fatalf("worker did not recover after rejection: %v", err)
	}
}

func TestNativeProcessRejectsMaximumOnlyPVSettings(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	defer o.Close()
	slots, p := externalTestFixture()
	for _, tc := range []struct {
		name    string
		maximum float64
		nullMin bool
	}{
		{"zero", 0, false},
		{"positive", 1500, false},
		{"zero with null minimum", 0, true},
		{"positive with null minimum", 1500, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := o.buildRequest(slots, p)
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(encoded, &payload); err != nil {
				t.Fatal(err)
			}
			// Inject the invalid setting at the host/worker boundary. Core's
			// normal request builder emits both verified capability bounds.
			settings := payload["settings"].(map[string]any)
			settings["pv_curtailment_max_w"] = tc.maximum
			if tc.nullMin {
				settings["pv_curtailment_min_w"] = nil
			}
			encoded, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			raw, err := o.transport.RoundTrip(ctx, encoded)
			if err != nil {
				t.Fatal(err)
			}
			var response externalResponse
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatal(err)
			}
			if response.RequestID != request.RequestID || response.OK || response.Error == nil || response.Error.Code != "invalid_request" || len(response.Plan.Actions) != 0 {
				t.Errorf("maximum-only setting must return invalid_request without a plan: %s", raw)
			}
			// Reuse this transport and let Core validate the next valid plan.
			if _, err := o.Optimize(ctx, slots, p); err != nil {
				t.Fatalf("valid request after rejection failed: %v", err)
			}
		})
	}
}

func TestNativeFullHorizonPhysicalBoundaries(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	defer o.Close()
	for _, mode := range []Mode{ModeSelfConsumption, ModeArbitrage, ModePassiveArbitrage, ModeCheapCharge} {
		for _, blocksBattery := range []bool{false, true} {
			slots, p := nativeBenchmarkFixture(true)
			p.Mode, p.Loadpoint.NoBatteryToEV = mode, blocksBattery
			plan, err := o.Optimize(context.Background(), slots, p)
			if err != nil {
				t.Fatalf("%s, battery blocked=%t: %v", mode, blocksBattery, err)
			}
			if err := ValidatePlan(slots, p, &plan); err != nil {
				t.Fatal(err)
			}
			for i, a := range plan.Actions {
				if a.LoadpointW > 0 && a.GridW < -1e-6 && a.BatteryW < 0 {
					t.Fatalf("slot %d: intended zero power became discharge", i)
				}
			}
		}
	}
}

func TestNativeProcessConcurrentAndImmutable(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	defer o.Close()
	slots, p := nativeFixture()
	before, _ := json.Marshal(struct {
		Slots  []Slot
		Params Params
	}{slots, p})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := o.Optimize(context.Background(), slots, p); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	after, _ := json.Marshal(struct {
		Slots  []Slot
		Params Params
	}{slots, p})
	if string(before) != string(after) {
		t.Fatal("shared input mutated")
	}
}

func TestNativeServiceAndFallback(t *testing.T) {
	for _, budget := range []time.Duration{time.Second, time.Nanosecond} {
		o := nativeWorker(t, budget)
		defer o.Close()
		st, err := state.Open(filepath.Join(t.TempDir(), "site.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		_, p := externalTestFixture()
		p.SoCLevels, p.ActionLevels = 11, 5
		now := time.Now().UTC().Truncate(time.Minute)
		if err := st.SavePrices([]state.PricePoint{{Zone: "SE3", SlotTsMs: now.Add(-time.Minute).UnixMilli(), SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli()}}); err != nil {
			t.Fatal(err)
		}
		s := New(st, nil, "SE3", p)
		s.BaseLoad = 500
		s.Optimizer = o
		plan := s.Replan(context.Background())
		if plan == nil || plan.Solver == nil {
			t.Fatal("service returned no plan")
		}
		if plan.Solver.Fallback != (budget == time.Nanosecond) {
			t.Fatalf("fallback metadata: %+v", plan.Solver)
		}
		if budget == time.Second && plan.Solver.Backend != "value_curve_rust" {
			t.Fatalf("Rust worker did not supply service plan: %+v", plan.Solver)
		}
	}
}
