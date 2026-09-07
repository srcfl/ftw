package mpc

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/srcfl/ftw/go/internal/state"
)

type testPrimaryOptimizer struct{}

type failingPrimaryOptimizer struct{}

type countingPrimaryOptimizer struct {
	calls           atomic.Int32
	shiftFirstStart atomic.Bool
}

const (
	testDecisionID1 = "00000000-0000-4000-8000-000000000001"
	testDecisionID2 = "00000000-0000-4000-8000-000000000002"
)

func (failingPrimaryOptimizer) Optimize(context.Context, []Slot, Params) (Plan, error) {
	return Plan{}, errors.New("test optimizer failure")
}

func (failingPrimaryOptimizer) Close() error { return nil }

func (testPrimaryOptimizer) Optimize(_ context.Context, slots []Slot, p Params) (Plan, error) {
	plan := Optimize(slots, p)
	plan.Solver = &SolverInfo{Engine: "cvxpy", Backend: "highs", Status: "optimal", SolveMs: 12}
	plan.OptimizerInput = json.RawMessage(`{"schema_version":1}`)
	return plan, nil
}

func (testPrimaryOptimizer) Close() error { return nil }

func (o *countingPrimaryOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	o.calls.Add(1)
	plan, err := testPrimaryOptimizer{}.Optimize(ctx, slots, p)
	if err == nil && o.shiftFirstStart.Load() && len(plan.Actions) > 0 {
		plan.Actions[0].SlotStartMs += int64(time.Minute / time.Millisecond)
	}
	return plan, err
}

func (o *countingPrimaryOptimizer) Close() error { return nil }

func (testPrimaryOptimizer) OptimizeRecourse(_ context.Context, slots []Slot, p Params, prefix int) (Plan, error) {
	plan := Optimize(slots, p)
	plan.Solver = &SolverInfo{
		Engine: "cvxpy", Backend: "highs", Status: "optimal",
		Formulation: "stochastic-recourse", ScenarioPolicy: "recourse",
		PolicyVersion:        "storage-recourse-v1",
		NonAnticipativeSlots: prefix,
	}
	return plan, nil
}

func (testPrimaryOptimizer) OptimizeMultistage(_ context.Context, slots []Slot, p Params, prefix int) (Plan, error) {
	plan := Optimize(slots, p)
	plan.Solver = &SolverInfo{
		Engine: "cvxpy", Backend: "highs", Status: "optimal",
		Formulation: "multistage-milp", ScenarioPolicy: "multistage",
		PolicyVersion:        "storage-multistage-v1",
		NonAnticipativeSlots: prefix,
		ServiceCVaRWeight:    1,
		ServiceCVaRAlpha:     0.95,
	}
	return plan, nil
}

// TestReplanCallsSaveDiag — after a successful replan, the SaveDiag
// hook fires once with (non-nil Diagnostic, reason). Verifies the hook
// wiring end-to-end without having to spin up the full stack.
func TestReplanCallsSaveDiag(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open state: %v", err)
	}
	defer st.Close()

	// Seed price and weather rows with fixed provenance. The weather row starts
	// before the price query margin but still covers the first price slot.
	now := time.Now().UTC().Truncate(time.Minute)
	priceStart := now.Add(-5 * time.Minute)
	priceFetchedAt := now.Add(-10 * time.Minute).UnixMilli()
	for i := 0; i < 4; i++ {
		err := st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: priceStart.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(),
			SlotLenMin: 15, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "entsoe", FetchedAtMs: priceFetchedAt,
		}})
		if err != nil {
			t.Fatalf("SavePrices: %v", err)
		}
	}
	weatherFetchedAt := now.Add(-20 * time.Minute).UnixMilli()
	pvW := 1200.0
	if err := st.SaveForecasts([]state.ForecastPoint{{
		SlotTsMs:   priceStart.Add(-30 * time.Minute).UnixMilli(),
		SlotLenMin: 60, PVWEstimated: &pvW,
		Source: "met.no", FetchedAtMs: weatherFetchedAt,
	}}); err != nil {
		t.Fatalf("SaveForecasts: %v", err)
	}

	svc := New(st, nil, "SE3", Params{
		Mode:                ModeSelfConsumption,
		SoCLevels:           11,
		CapacityWh:          10000,
		SoCMin:              0.1,
		SoCMax:              0.95,
		InitialSoC:          0.5,
		ActionLevels:        5,
		MaxChargeW:          3000,
		MaxDischargeW:       3000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    80,
	})
	svc.BaseLoad = 500
	svc.decisionIDFactory = func() string { return testDecisionID1 }

	var called atomic.Int32
	var gotReason atomic.Value
	var gotZone atomic.Value
	svc.SaveDiag = func(d *Diagnostic, reason string) error {
		called.Add(1)
		gotReason.Store(reason)
		gotZone.Store(d.Zone)
		if d.DecisionID != testDecisionID1 {
			t.Errorf("Diagnostic.DecisionID = %q, want %q", d.DecisionID, testDecisionID1)
		}
		if len(d.Slots) == 0 {
			t.Error("Diagnostic.Slots empty — DP ran but no slots reached the snapshot")
		}
		js, err := json.Marshal(d)
		if err != nil {
			return err
		}
		return st.SaveDiagnostic(d.ComputedAtMs, reason, d.Zone,
			d.TotalCostOre, d.Horizon, string(js))
	}

	plan := svc.Replan(context.Background())
	if plan == nil {
		t.Fatal("Replan returned nil — buildSlots likely empty")
	}
	if plan.DecisionID != testDecisionID1 {
		t.Fatalf("Plan.DecisionID = %q, want %q", plan.DecisionID, testDecisionID1)
	}
	if n := called.Load(); n != 1 {
		t.Errorf("SaveDiag called %d times, want 1", n)
	}
	if r, _ := gotReason.Load().(string); r == "" {
		t.Error("SaveDiag reason was empty")
	}
	if z, _ := gotZone.Load().(string); z != "SE3" {
		t.Errorf("SaveDiag zone = %q, want SE3", z)
	}
	row, err := st.LoadDiagnosticAt(time.Now().Add(time.Minute).UnixMilli())
	if err != nil || row == nil {
		t.Fatalf("LoadDiagnosticAt: row=%+v err=%v", row, err)
	}
	var persisted Diagnostic
	if err := json.Unmarshal([]byte(row.JSON), &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Slots) == 0 {
		t.Fatal("persisted diagnostic has no slots")
	}
	if persisted.DecisionID != plan.DecisionID {
		t.Fatalf("persisted DecisionID = %q, want %q", persisted.DecisionID, plan.DecisionID)
	}
	if persisted.InputProvenanceSchema != inputProvenanceSchemaVersion {
		t.Fatalf("input provenance schema = %d, want %d", persisted.InputProvenanceSchema, inputProvenanceSchemaVersion)
	}
	if got := persisted.Slots[0]; got.PriceInputSource != "entsoe" ||
		got.PriceInputAvailableAtMs != priceFetchedAt || got.WeatherRowSource != "met.no" ||
		got.WeatherRowAvailableAtMs != weatherFetchedAt {
		t.Fatalf("persisted input provenance = %+v", got)
	}
	slotNow := time.UnixMilli(plan.Actions[0].SlotStartMs).Add(time.Second)
	directive, ok := svc.SlotDirectiveAt(slotNow)
	if !ok {
		t.Fatal("SlotDirectiveAt did not return the active plan slot")
	}
	if directive.DecisionID != plan.DecisionID || directive.SlotStart.UnixMilli() != plan.Actions[0].SlotStartMs {
		t.Fatalf("directive identity = (%q, %d), want (%q, %d)",
			directive.DecisionID, directive.SlotStart.UnixMilli(), plan.DecisionID, plan.Actions[0].SlotStartMs)
	}
	if _, _, decisionID, ok := svc.SlotAt(slotNow); !ok || decisionID != plan.DecisionID {
		t.Fatalf("legacy slot identity = (%q, %t), want (%q, true)",
			decisionID, ok, plan.DecisionID)
	}
}

// TestReplanWithoutSaveDiagAssignsDistinctDecisionIDs checks that the
// persistence hook remains optional and that two accepted plans for the same
// slot cannot be confused by their timestamps alone.
func TestReplanWithoutSaveDiagAssignsDistinctDecisionIDs(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Minute)
	priceStart := now.Add(-5 * time.Minute)
	_ = st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: priceStart.UnixMilli(), SlotLenMin: 60,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test",
		FetchedAtMs: now.UnixMilli(),
	}})
	svc := New(st, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	var ids atomic.Int32
	svc.decisionIDFactory = func() string {
		if ids.Add(1) == 1 {
			return testDecisionID1
		}
		return testDecisionID2
	}
	first := svc.Replan(context.Background())
	second := svc.Replan(context.Background())
	if first == nil || second == nil || len(first.Actions) == 0 || len(second.Actions) == 0 {
		t.Fatalf("replans returned first=%+v second=%+v", first, second)
	}
	if first.Actions[0].SlotStartMs != second.Actions[0].SlotStartMs {
		t.Fatalf("test replans did not cover the same slot: %d != %d",
			first.Actions[0].SlotStartMs, second.Actions[0].SlotStartMs)
	}
	if first.DecisionID != testDecisionID1 || second.DecisionID != testDecisionID2 {
		t.Fatalf("decision IDs = %q, %q", first.DecisionID, second.DecisionID)
	}
}

func TestReplanRejectsTimelineItCannotRestore(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Minute)
	priceStart := now.Add(-5 * time.Minute)
	prices := []state.PricePoint{
		{
			Zone: "SE3", SlotTsMs: priceStart.UnixMilli(), SlotLenMin: 15,
			SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli(),
		},
		{
			Zone: "SE3", SlotTsMs: priceStart.Add(15 * time.Minute).UnixMilli(), SlotLenMin: 15,
			SpotOreKwh: 60, TotalOreKwh: 110, Source: "test", FetchedAtMs: now.UnixMilli(),
		},
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatal(err)
	}

	svc := New(st, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	optimizer := &countingPrimaryOptimizer{}
	svc.Optimizer = optimizer
	var idCalls atomic.Int32
	svc.decisionIDFactory = func() string {
		if idCalls.Add(1) == 1 {
			return testDecisionID1
		}
		return testDecisionID2
	}
	var saved atomic.Int32
	svc.SaveDiag = func(*Diagnostic, string) error {
		saved.Add(1)
		return nil
	}

	accepted := svc.Replan(context.Background())
	if accepted == nil || accepted.DecisionID != testDecisionID1 {
		t.Fatalf("initial valid plan = %+v", accepted)
	}

	// The state store permits different starts whose durations overlap. Make
	// the first row cover the second, then prove the service retains the last
	// restorable plan without assigning or persisting a new decision ID.
	prices[0].SlotLenMin = 60
	if err := st.SavePrices(prices[:1]); err != nil {
		t.Fatal(err)
	}
	got := svc.Replan(context.Background())
	if got != accepted || svc.Latest() != accepted {
		t.Fatalf("invalid timeline replaced the accepted plan: got=%p accepted=%p latest=%p",
			got, accepted, svc.Latest())
	}
	if idCalls.Load() != 1 {
		t.Fatalf("decision ID factory called %d times, want 1", idCalls.Load())
	}
	if saved.Load() != 1 {
		t.Fatalf("diagnostic saved %d times, want 1", saved.Load())
	}
	if optimizer.calls.Load() != 1 {
		t.Fatalf("optimizer called %d times, want 1", optimizer.calls.Load())
	}
}

func TestReplanRejectsOptimizerTimelineThatChangesActionKey(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Minute)
	priceStart := now.Add(-5 * time.Minute)
	if err := st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: priceStart.UnixMilli(), SlotLenMin: 60,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli(),
	}}); err != nil {
		t.Fatal(err)
	}

	svc := New(st, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	optimizer := &countingPrimaryOptimizer{}
	svc.Optimizer = optimizer
	var idCalls atomic.Int32
	svc.decisionIDFactory = func() string {
		if idCalls.Add(1) == 1 {
			return testDecisionID1
		}
		return testDecisionID2
	}
	var saved atomic.Int32
	svc.SaveDiag = func(*Diagnostic, string) error {
		saved.Add(1)
		return nil
	}

	accepted := svc.Replan(context.Background())
	if accepted == nil || accepted.DecisionID != testDecisionID1 {
		t.Fatalf("initial valid plan = %+v", accepted)
	}
	optimizer.shiftFirstStart.Store(true)
	got := svc.Replan(context.Background())
	if got != accepted || svc.Latest() != accepted {
		t.Fatalf("misaligned optimizer plan replaced the accepted plan: got=%p accepted=%p latest=%p",
			got, accepted, svc.Latest())
	}
	if optimizer.calls.Load() != 2 {
		t.Fatalf("optimizer called %d times, want 2", optimizer.calls.Load())
	}
	if idCalls.Load() != 1 {
		t.Fatalf("decision ID factory called %d times, want 1", idCalls.Load())
	}
	if saved.Load() != 1 {
		t.Fatalf("diagnostic saved %d times, want 1", saved.Load())
	}
}

func TestReplanRetainsInputProvenanceAcrossOptimizerPaths(t *testing.T) {
	tests := []struct {
		name         string
		optimizer    PlanOptimizer
		wantFallback bool
		decisionID   string
	}{
		{name: "go only", decisionID: "00000000-0000-4000-8000-000000000011"},
		{name: "primary", optimizer: testPrimaryOptimizer{}, decisionID: "00000000-0000-4000-8000-000000000012"},
		{name: "go fallback", optimizer: failingPrimaryOptimizer{}, wantFallback: true, decisionID: "00000000-0000-4000-8000-000000000013"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			now := time.Now().UTC().Truncate(time.Minute)
			priceStart := now.Add(-5 * time.Minute)
			priceFetchedAt := now.Add(-10 * time.Minute).UnixMilli()
			if err := st.SavePrices([]state.PricePoint{{
				Zone: "SE3", SlotTsMs: priceStart.UnixMilli(), SlotLenMin: 15,
				SpotOreKwh: 50, TotalOreKwh: 100,
				Source: "entsoe", FetchedAtMs: priceFetchedAt,
			}}); err != nil {
				t.Fatal(err)
			}
			weatherFetchedAt := now.Add(-20 * time.Minute).UnixMilli()
			pvW := 1200.0
			if err := st.SaveForecasts([]state.ForecastPoint{{
				SlotTsMs:   priceStart.Add(-30 * time.Minute).UnixMilli(),
				SlotLenMin: 60, PVWEstimated: &pvW,
				Source: "met.no", FetchedAtMs: weatherFetchedAt,
			}}); err != nil {
				t.Fatal(err)
			}

			svc := New(st, nil, "SE3", Params{
				Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10000,
				SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
				ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
				ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
			})
			svc.BaseLoad = 500
			svc.Optimizer = tc.optimizer
			svc.decisionIDFactory = func() string { return tc.decisionID }

			plan := svc.Replan(context.Background())
			if plan == nil || plan.Solver == nil {
				t.Fatalf("Replan returned %+v", plan)
			}
			if plan.Solver.Fallback != tc.wantFallback {
				t.Fatalf("fallback = %v, want %v; solver=%+v", plan.Solver.Fallback, tc.wantFallback, plan.Solver)
			}
			if plan.DecisionID != tc.decisionID {
				t.Fatalf("plan decision ID = %q, want %q", plan.DecisionID, tc.decisionID)
			}
			d := svc.Diagnose()
			if d == nil || len(d.Slots) != 1 {
				t.Fatalf("Diagnose returned %+v", d)
			}
			if d.InputProvenanceSchema != inputProvenanceSchemaVersion {
				t.Fatalf("input provenance schema = %d, want %d", d.InputProvenanceSchema, inputProvenanceSchemaVersion)
			}
			if d.DecisionID != plan.DecisionID {
				t.Fatalf("diagnostic decision ID = %q, want %q", d.DecisionID, plan.DecisionID)
			}
			if got := d.Slots[0]; got.PriceInputSource != "entsoe" ||
				got.PriceInputAvailableAtMs != priceFetchedAt || got.WeatherRowSource != "met.no" ||
				got.WeatherRowAvailableAtMs != weatherFetchedAt {
				t.Fatalf("input provenance = %+v", got)
			}
		})
	}
}

func TestReplanLoadsHourlyWeatherCoveringCurrentPriceSlot(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC().Truncate(time.Minute)
	priceStart := now.Add(-5 * time.Minute)
	if err := st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: priceStart.UnixMilli(), SlotLenMin: 15,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test",
		FetchedAtMs: now.UnixMilli(),
	}}); err != nil {
		t.Fatal(err)
	}
	pvW := 1234.0
	if err := st.SaveForecasts([]state.ForecastPoint{{
		SlotTsMs:   priceStart.Add(-30 * time.Minute).UnixMilli(),
		SlotLenMin: 60, PVWEstimated: &pvW, Source: "weather-test",
		FetchedAtMs: now.Add(-35 * time.Minute).UnixMilli(),
	}}); err != nil {
		t.Fatal(err)
	}

	svc := New(st, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.BaseLoad = 500

	plan := svc.Replan(context.Background())
	if plan == nil || len(plan.Actions) != 1 {
		t.Fatalf("Replan returned %#v, want one action", plan)
	}
	if _, err := uuid.Parse(plan.DecisionID); err != nil {
		t.Fatalf("accepted plan decision ID %q is not a UUID: %v", plan.DecisionID, err)
	}
	if got := plan.Actions[0].PVW; got != -pvW {
		t.Fatalf("current slot PVW = %.1f, want %.1f from covering hourly row", got, -pvW)
	}
}
