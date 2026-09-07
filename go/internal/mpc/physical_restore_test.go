package mpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"
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
