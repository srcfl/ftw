package main

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
)

// slowPlanOptimizer models a solve that takes longer than the missing-plan
// retry cooldown and, like a Go DP or a busy worker, does not stop on cancel.
type slowPlanOptimizer struct {
	solve time.Duration
	calls atomic.Int32
}

func (o *slowPlanOptimizer) Optimize(_ context.Context, slots []mpc.Slot, p mpc.Params) (mpc.Plan, error) {
	o.calls.Add(1)
	time.Sleep(o.solve)
	plan := mpc.Optimize(slots, p)
	plan.Solver = &mpc.SolverInfo{Engine: "test", Backend: "slow", Status: "ok", Formulation: "deterministic"}
	return plan, nil
}

func (*slowPlanOptimizer) Close() error { return nil }

// A solve slower than the retry cooldown must still publish a plan. Before
// the Pending guard, every retry cancelled the solve in flight, so on a slow
// box no plan ever landed. Scale: tick 1 s -> 20 ms, cooldown 5 s -> 100 ms,
// solve 6 s -> 120 ms.
func TestMissingPlanRetryLetsSlowSolvePublish(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now().UTC().Truncate(time.Hour)
	prices := make([]state.PricePoint, 4)
	for i := range prices {
		prices[i] = state.PricePoint{
			Zone: "SE3", SlotTsMs: now.Add(time.Duration(i) * time.Hour).UnixMilli(),
			SlotLenMin: 60, SpotOreKwh: float64(40 + i*20), TotalOreKwh: float64(90 + i*20),
			Source: "test", FetchedAtMs: now.UnixMilli(),
		}
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	svc := mpc.New(st, nil, "SE3", mpc.Params{
		Mode: mpc.ModeSelfConsumption, SoCLevels: 11, ActionLevels: 5,
		CapacityWh: 10000, InitialSoC: 0.5, SoCMin: 0.1, SoCMax: 0.95,
		MaxChargeW: 3000, MaxDischargeW: 3000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc.Horizon = 4 * time.Hour
	svc.BaseLoad = 500
	opt := &slowPlanOptimizer{solve: 120 * time.Millisecond}
	svc.Optimizer = opt
	t.Cleanup(func() {
		until := time.Now().Add(5 * time.Second)
		for svc.IsReplanning() && time.Now().Before(until) {
			time.Sleep(10 * time.Millisecond)
		}
	})

	var last time.Time
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) {
		if missingPlanRetryDue(svc, svc.Latest() == nil, last, 100*time.Millisecond) {
			last = time.Now()
			go svc.ReplanWithReason(context.Background(), "missing_plan_retry")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if svc.Latest() == nil {
		t.Fatalf("no plan published after 2 s (100 s at real scale); %d solves started, each one cancelled by the next retry", opt.calls.Load())
	}
}
