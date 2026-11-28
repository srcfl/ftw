package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The startup replan runs 2 s after start, often before the first battery
// reading and the first price fetch, and then fails. A box in a planner mode
// retries a missing plan on its own; any other mode waited for the next
// scheduled replan, up to 15 minutes. firstPlanWhenReady makes one more
// replan as soon as the planner's inputs are first ready, once per process,
// unless a current plan has landed by then.
const (
	firstPlanPoll  = time.Second
	firstPlanLimit = 15 * time.Minute // the scheduled replan takes over after this
)

type firstPlanner interface {
	PlanSnapshot() mpc.PlanSnapshot
	ReplanWithReason(context.Context, string) *mpc.Plan
}

func hasCurrentPlan(snap mpc.PlanSnapshot) bool {
	return snap.Plan != nil && len(snap.Plan.Actions) > 0 && !snap.Outdated
}

// firstPlanWhenReady reports whether it asked for the replan.
func firstPlanWhenReady(ctx context.Context, svc firstPlanner, ready func(time.Time) bool, poll, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		snap := svc.PlanSnapshot()
		if hasCurrentPlan(snap) {
			return false
		}
		// A replan already on its way may bring the plan; wait for it.
		if !snap.Pending && ready(time.Now()) {
			slog.Info("mpc: planning inputs ready, making the first plan")
			svc.ReplanWithReason(ctx, "inputs_ready")
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// planInputsReady follows the planner's own gates: prices for the slot in
// progress, and for a site that plans a battery, a fresh charge level from an
// online battery (telemetry.FreshBatterySoCOnline, the planner's own limits).
func planInputsReady(svc *mpc.Service, tel *telemetry.Store, needBattery bool) func(time.Time) bool {
	return func(now time.Time) bool {
		if svc == nil || svc.Store == nil {
			return false
		}
		prices, err := svc.Store.LoadPrices(svc.Zone, now.Add(-15*time.Minute).UnixMilli(), now.Add(time.Hour).UnixMilli())
		if err != nil || len(prices) == 0 {
			return false
		}
		if !needBattery {
			return true
		}
		return tel != nil && tel.FreshBatterySoCOnline(now)
	}
}
