package mpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func physicalRestoreFixture(t *testing.T, batteries, evs int, pvSlot int) (Plan, Params, *Diagnostic, time.Time) {
	t.Helper()
	o := nativeWorker(t, 500*time.Millisecond)
	defer o.Close()
	slots, p := topologyFixture(batteries, evs)
	now := time.Now()
	start := now.Add(-time.Minute).Truncate(time.Minute)
	for i := range slots {
		slots[i].StartMs = start.Add(time.Duration(i) * time.Hour).UnixMilli()
	}
	if pvSlot >= 0 {
		p.PVCurtailment = PVCurtailment{Driver: "pv", Proof: "previous-process", MinW: 2, MaxW: 15000}
		slots[pvSlot].PVW, slots[pvSlot].SpotOre = -6000, -100
	}
	plan, err := o.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	plan.DecisionID = testDecisionID1
	plan.GeneratedAtMs = now.UnixMilli()
	d := buildDiagnostic(&plan, slots, p, "SE4", now.UnixMilli(), "review")
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Diagnostic
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	return plan, p, &persisted, now
}

func TestNativeRestoredPVPlanCannotRegainExecution(t *testing.T) {
	plan, p, persisted, now := physicalRestoreFixture(t, 0, 2, 0)
	if !plan.Actions[0].PVCurtailActive || len(plan.Actions[0].LoadpointPowerW) != 2 {
		t.Fatalf("fixture did not use PV control and two EVs: %+v", plan.Actions[0])
	}
	svc := &Service{PVExecutionAllowed: func(PVCurtailment) bool { return false }}
	svc.InstallPlan(plan, p, p.Loadpoint.ID)
	if _, ok := svc.SlotDirectiveAt(now); ok {
		t.Fatal("fixture did not revoke the original live plan")
	}
	if !svc.RestoreDiagnostic(persisted, now, "restart") {
		return // refusing activation is safe
	}
	dir, active := svc.SlotDirectiveAt(now)
	_, _, _, legacy := svc.SlotAt(now)
	if active || legacy || !svc.PlanSnapshot().Outdated {
		t.Fatalf("restored PV plan regained execution: active=%v legacy=%v outdated=%v proof=%+v EV budgets=%v", active, legacy, svc.PlanSnapshot().Outdated, dir.PVCurtailment, dir.LoadpointEnergyWh)
	}
}

func TestNativeRestoredFleetRetainsPhysicalBudgets(t *testing.T) {
	plan, p, persisted, now := physicalRestoreFixture(t, 2, 0, -1)
	svc := &Service{}
	svc.InstallPlan(plan, p, "")
	before, ok := svc.SlotDirectiveAt(now)
	if !ok || len(before.StorageEnergyWh) != 2 {
		t.Fatalf("fixture has no fleet budgets: %+v", before)
	}
	if !svc.RestoreDiagnostic(persisted, now, "restart") {
		return // refusing activation is safe
	}
	after, active := svc.SlotDirectiveAt(now)
	if active || svc.Latest() == nil || !svc.PlanSnapshot().Outdated {
		t.Fatalf("restored fleet must remain archived until fresh physical inputs: before=%v after=%v aggregate=%g active=%v", before.StorageEnergyWh, after.StorageEnergyWh, after.BatteryEnergyWh, active)
	}
}

func TestNativeRestoreFuturePVDependencyRequiresNewPlan(t *testing.T) {
	plan, p, persisted, now := physicalRestoreFixture(t, 0, 2, 1)
	if plan.Actions[0].PVCurtailActive || !plan.Actions[1].PVCurtailActive {
		t.Fatal("fixture must depend on PV only in a future slot")
	}
	svc := &Service{PVExecutionAllowed: func(PVCurtailment) bool { return true }}
	svc.InstallPlan(plan, p, p.Loadpoint.ID)
	if _, ok := svc.SlotDirectiveAt(now); !ok {
		t.Fatal("live fixture is not executable")
	}
	if !svc.RestoreDiagnostic(persisted, now, "restart") {
		t.Fatal("archive lost")
	}
	if svc.Latest() == nil || !svc.PlanSnapshot().Outdated {
		t.Fatal("archive/execution states collapsed")
	}
	if _, ok := svc.SlotDirectiveAt(now); ok {
		t.Fatal("future PV dependency regained execution")
	}
	if _, _, _, ok := svc.SlotAt(now); ok {
		t.Fatal("legacy path regained execution")
	}
	svc.InstallPlan(plan, p, p.Loadpoint.ID)
	if _, ok := svc.SlotDirectiveAt(now); !ok {
		t.Fatal("new validated plan did not restore execution")
	}
}

func TestNativeShadowPreservesCurrentExecutionButCannotActivateArchive(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	t.Cleanup(func() { o.Close() })
	svc := shadowTestService(t)
	// Hourly fixtures can fall outside Service's 15-minute lookback. Keep a
	// current slot so this test checks execution permission at any wall time.
	if _, err := svc.Store.ClearPrices(); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Minute).Truncate(time.Minute)
	for i := 0; i < 4; i++ {
		if err := svc.Store.SavePrices([]state.PricePoint{{
			Zone: svc.Zone, SlotTsMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(),
			SlotLenMin: 15, SpotOreKwh: 50 + float64(i)*40, TotalOreKwh: 100 + float64(i)*80,
			Source: "test", FetchedAtMs: time.Now().UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	svc.Optimizer = &EnergyplanOptimizer{ExternalOptimizer: o}
	plan := svc.Replan(context.Background())
	if plan == nil || plan.Solver == nil || plan.Solver.Fallback || len(plan.Actions[0].StoragePowerW) == 0 {
		t.Fatalf("fixture needs a native physical plan: %+v", plan)
	}
	svc.shadowWG.Wait()
	if svc.Latest().DPShadow == nil {
		t.Fatal("Core DP shadow did not finish")
	}
	assertExecution := func(want bool) {
		t.Helper()
		_, active := svc.SlotDirectiveAt(time.Now())
		_, _, _, legacy := svc.SlotAt(time.Now())
		if active != want || legacy != want || svc.PlanSnapshot().Outdated == want {
			t.Fatalf("execution=%v legacy=%v outdated=%v, want execution=%v", active, legacy, svc.PlanSnapshot().Outdated, want)
		}
	}
	assertExecution(true)
	encoded, err := json.Marshal(svc.Diagnose())
	if err != nil {
		t.Fatal(err)
	}
	var archive Diagnostic
	if err := json.Unmarshal(encoded, &archive); err != nil {
		t.Fatal(err)
	}
	if !svc.RestoreDiagnostic(&archive, time.Now(), "restart") {
		t.Fatal("archive was not retained")
	}
	assertExecution(false)
	// The same decision ID may finish its shadow after an archive is restored.
	// Comparison data must not grant execution to that archive.
	svc.recordCoreDPShadow(*plan, nil, Params{}, "late shadow", time.Now().UnixMilli(), &ShadowPlan{})
	assertExecution(false)
	fresh := svc.Replan(context.Background())
	svc.shadowWG.Wait()
	if fresh == nil || fresh.DecisionID == plan.DecisionID {
		t.Fatal("fresh replan did not publish a new decision")
	}
	assertExecution(true)
}
