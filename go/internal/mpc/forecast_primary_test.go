package mpc

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestNativePrimaryForecastReachesSolverBeforeRisk(t *testing.T) {
	worker := nativeWorker(t, 500*time.Millisecond)
	t.Cleanup(func() { _ = worker.Close() })
	svc := shadowTestService(t)
	t.Cleanup(func() { svc.replanWG.Wait(); svc.shadowWG.Wait() })
	svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: worker}
	svc.PVForecastSafetyK = 1
	var legacy, archivedBase, archivedPlanning []Slot
	svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
		return ForecastInputs{
			Resolve: func(ctx context.Context, slots []Slot) []Slot {
				if _, bounded := ctx.Deadline(); !bounded {
					t.Error("forecast resolution has no deadline")
				}
				legacy = append([]Slot(nil), slots...)
				for i := range slots {
					slots[i].PVW, slots[i].LoadW = -2400, 700
					// Selection cannot change market data or hardware limits.
					slots[i].PriceOre = -9999
				}
				return slots
			},
			Risk: func(base, planning []Slot, _ float64) {
				for i := range base {
					if base[i].PVW != -2400 || base[i].LoadW != 700 {
						t.Errorf("risk received legacy forecast: %+v", base[i])
					}
					planning[i].PVW, planning[i].LoadW = -2000, 800
				}
			},
			Record: func(base, planning []Slot, decisionID string, issuedAtMS int64) {
				if decisionID == "" || issuedAtMS <= 0 {
					t.Error("forecast archived before a plan was published")
				}
				archivedBase = append([]Slot(nil), base...)
				archivedPlanning = append([]Slot(nil), planning...)
			},
		}
	}
	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil || plan.Solver.Fallback {
		t.Fatalf("native primary plan missing: %+v", plan)
	}
	var input externalRequest
	if err := json.Unmarshal(plan.OptimizerInput, &input); err != nil {
		t.Fatal(err)
	}
	if len(input.Slots) == 0 || len(input.Slots) != len(archivedBase) || len(input.Slots) != len(legacy) {
		t.Fatal("forecast or archive lost the planner horizon")
	}
	for i, slot := range input.Slots {
		if slot.PVW != -2000 || slot.LoadW != 800 {
			t.Fatalf("solver did not receive primary downside: %+v", slot)
		}
		if legacy[i].LoadW != 500 || archivedBase[i].LoadW != 700 || archivedBase[i].PVW != -2400 {
			t.Fatalf("point forecast and legacy shadow were conflated: %+v / %+v", legacy[i], archivedBase[i])
		}
		if archivedPlanning[i].LoadW != slot.LoadW || archivedPlanning[i].PVW != slot.PVW {
			t.Fatal("planning archive differs from actual solver input")
		}
		if archivedBase[i].PriceOre != legacy[i].PriceOre {
			t.Fatal("forecast selection changed prices")
		}
	}
	if svc.plannedPredictions != nil {
		t.Fatal("legacy shadow controls the primary drift trigger")
	}
}

func TestPrimaryForecastDoesNotLockDispatchAndCancelsSupersededWork(t *testing.T) {
	svc := shadowTestService(t)
	t.Cleanup(func() { svc.replanWG.Wait(); svc.shadowWG.Wait() })
	started, canceled := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	svc.ForecastSnapshot = func(time.Time, []state.ForecastPoint) ForecastInputs {
		return ForecastInputs{Resolve: func(ctx context.Context, slots []Slot) []Slot {
			if calls.Add(1) == 1 {
				close(started)
				<-ctx.Done()
				close(canceled)
				for i := range slots {
					slots[i].LoadW = 9999
				}
			} else {
				for i := range slots {
					slots[i].LoadW = 650
				}
			}
			return slots
		}}
	}
	svc.RequestReplan("old")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("primary forecast did not start")
	}
	readDone := make(chan struct{})
	go func() {
		_ = svc.Latest()
		svc.RequestReplan("new")
		close(readDone)
	}()
	select {
	case <-readDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("forecast resolution holds the service lock")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("superseded forecast did not receive cancellation")
	}
	waitFor(t, "new primary plan", func() bool { return svc.Latest() != nil && !svc.IsReplanning() })
	d := svc.Diagnose()
	if d == nil || len(svc.lastSlots) == 0 {
		t.Fatal("new primary plan was not published")
	}
	for _, slot := range svc.lastSlots {
		if slot.LoadW != 650 {
			t.Fatalf("superseded forecast reached active plan: %+v", slot)
		}
	}
}
