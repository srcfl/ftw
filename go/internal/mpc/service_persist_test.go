package mpc

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
)

type testPrimaryOptimizer struct{}

func (testPrimaryOptimizer) Optimize(_ context.Context, slots []Slot, p Params) (Plan, error) {
	plan := Optimize(slots, p)
	plan.Solver = &SolverInfo{Engine: "cvxpy", Backend: "highs", Status: "optimal", SolveMs: 12}
	plan.OptimizerInput = json.RawMessage(`{"schema_version":1}`)
	return plan, nil
}

func (testPrimaryOptimizer) Close() error { return nil }

// TestReplanCallsSaveDiag — after a successful replan, the SaveDiag
// hook fires once with (non-nil Diagnostic, reason). Verifies the hook
// wiring end-to-end without having to spin up the full stack.
func TestReplanCallsSaveDiag(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("Open state: %v", err)
	}
	defer st.Close()

	// Seed a few price rows so buildSlots produces a non-empty plan.
	now := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 4; i++ {
		err := st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}})
		if err != nil {
			t.Fatalf("SavePrices: %v", err)
		}
	}

	svc := New(st, nil, "SE3", Params{
		Mode:                ModeSelfConsumption,
		SoCLevels:           11,
		CapacityWh:          10000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        5,
		MaxChargeW:          3000,
		MaxDischargeW:       3000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    80,
	})
	svc.BaseLoad = 500

	var called atomic.Int32
	var gotReason atomic.Value
	var gotZone atomic.Value
	svc.SaveDiag = func(d *Diagnostic, reason string) error {
		called.Add(1)
		gotReason.Store(reason)
		gotZone.Store(d.Zone)
		if len(d.Slots) == 0 {
			t.Error("Diagnostic.Slots empty — DP ran but no slots reached the snapshot")
		}
		return nil
	}

	if plan := svc.Replan(context.Background()); plan == nil {
		t.Fatal("Replan returned nil — buildSlots likely empty")
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
}

// TestReplanWithoutSaveDiagDoesNotPanic — the persistence hook is
// optional. A service without the hook must replan cleanly.
func TestReplanWithoutSaveDiagDoesNotPanic(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Hour)
	_ = st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: now.UnixMilli(), SlotLenMin: 60,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test",
		FetchedAtMs: now.UnixMilli(),
	}})
	svc := New(st, nil, "SE3", Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	_ = svc.Replan(context.Background())
}

func TestPrimaryOptimizerKeepsDPAsDiagnosticShadow(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 4; i++ {
		_ = st.SavePrices([]state.PricePoint{{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: 50, TotalOreKwh: 100,
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}})
	}
	svc := New(st, nil, "SE3", Params{
		Mode: ModePassiveArbitrage, SoCLevels: 11, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.BaseLoad = 500
	svc.Optimizer = testPrimaryOptimizer{}
	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil || plan.Solver.Engine != "cvxpy" {
		t.Fatalf("primary plan not active: %+v", plan)
	}
	if plan.DPShadow == nil || plan.DPShadow.Solver == nil || plan.DPShadow.Solver.Engine != "go-dp" {
		t.Fatalf("DP shadow missing: %+v", plan.DPShadow)
	}
	if plan.DPShadow.ComparedSlots != len(plan.Actions) || plan.DPShadow.FirstAction == nil {
		t.Fatalf("shadow comparison incomplete: %+v", plan.DPShadow)
	}
	if d := svc.Diagnose(); d == nil || d.DPShadow == nil {
		t.Fatal("persisted diagnostic omitted DP shadow")
	}
}
