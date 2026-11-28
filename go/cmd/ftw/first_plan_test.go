package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type fakeFirstPlanner struct {
	mu      sync.Mutex
	snap    mpc.PlanSnapshot
	reasons []string
}

func (f *fakeFirstPlanner) PlanSnapshot() mpc.PlanSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeFirstPlanner) ReplanWithReason(_ context.Context, reason string) *mpc.Plan {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reasons = append(f.reasons, reason)
	return nil
}

func (f *fakeFirstPlanner) replans() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reasons...)
}

func readyAfter(n int) func(time.Time) bool {
	calls := 0
	return func(time.Time) bool {
		calls++
		return calls > n
	}
}

func TestFirstPlanWhenReadyReplansOnceInputsAreReady(t *testing.T) {
	svc := &fakeFirstPlanner{}
	if !firstPlanWhenReady(context.Background(), svc, readyAfter(3), time.Millisecond, time.Second) {
		t.Fatal("did not replan once the inputs were ready")
	}
	if got := svc.replans(); len(got) != 1 || got[0] != "inputs_ready" {
		t.Fatalf("replans = %v, want one inputs_ready", got)
	}
}

func TestFirstPlanWhenReadyLeavesACurrentPlan(t *testing.T) {
	svc := &fakeFirstPlanner{snap: mpc.PlanSnapshot{Plan: &mpc.Plan{Actions: []mpc.Action{{}}}}}
	if firstPlanWhenReady(context.Background(), svc, readyAfter(0), time.Millisecond, time.Second) {
		t.Fatal("replanned over a current plan")
	}
	if got := svc.replans(); len(got) != 0 {
		t.Fatalf("replans = %v, want none", got)
	}
}

// A plan restored at start is outdated until a replan succeeds.
func TestFirstPlanWhenReadyReplacesAnOutdatedPlan(t *testing.T) {
	svc := &fakeFirstPlanner{snap: mpc.PlanSnapshot{Plan: &mpc.Plan{Actions: []mpc.Action{{}}}, Outdated: true}}
	if !firstPlanWhenReady(context.Background(), svc, readyAfter(0), time.Millisecond, time.Second) {
		t.Fatal("kept waiting behind an outdated plan")
	}
}

func TestFirstPlanWhenReadyWaitsForAReplanInFlight(t *testing.T) {
	svc := &fakeFirstPlanner{snap: mpc.PlanSnapshot{Pending: true, Outdated: true}}
	if firstPlanWhenReady(context.Background(), svc, readyAfter(0), time.Millisecond, 20*time.Millisecond) {
		t.Fatal("replanned over a replan in flight")
	}
}

func TestFirstPlanWhenReadyGivesUpToTheSchedule(t *testing.T) {
	svc := &fakeFirstPlanner{}
	if firstPlanWhenReady(context.Background(), svc, func(time.Time) bool { return false }, time.Millisecond, 20*time.Millisecond) {
		t.Fatal("replanned without ready inputs")
	}
	if got := svc.replans(); len(got) != 0 {
		t.Fatalf("replans = %v, want none", got)
	}
}

func TestPlanInputsReady(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := mpc.New(st, nil, "SE3", mpc.Params{})
	tel := telemetry.NewStore()
	now := time.Now()
	battery, noBattery := planInputsReady(svc, tel, true), planInputsReady(svc, tel, false)
	if battery(now) || noBattery(now) {
		t.Fatal("ready before any prices")
	}
	slot := now.Truncate(15 * time.Minute)
	if err := st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: slot.UnixMilli(), SlotLenMin: 15,
		SpotOreKwh: 50, TotalOreKwh: 100, Source: "test", FetchedAtMs: now.UnixMilli(),
	}}); err != nil {
		t.Fatal(err)
	}
	if !noBattery(now) {
		t.Fatal("a site without a battery needs only prices")
	}
	if battery(now) {
		t.Fatal("ready before the first battery reading")
	}
	soc := 0.5
	tel.Update("sungrow", telemetry.DerBattery, 0, &soc, nil)
	if battery(time.Now()) {
		t.Fatal("ready before the battery driver is known to be online")
	}
	tel.RecordDriverSuccess("sungrow")
	if !battery(time.Now()) {
		t.Fatal("not ready with prices and a fresh charge level")
	}
	if battery(time.Now().Add(telemetry.BatterySoCMaxAge + time.Minute)) {
		t.Fatal("ready on a charge level older than the planner accepts")
	}
}
