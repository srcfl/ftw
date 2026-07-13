package mpc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func externalTestFixture() ([]Slot, Params) {
	slots := []Slot{
		{StartMs: 1, LenMin: 60, PriceOre: 20, SpotOre: 10, Confidence: 1, LoadW: 500, Limits: PowerLimits{MaxImportW: 8000, MaxExportW: 8000}},
		{StartMs: 3600001, LenMin: 60, PriceOre: 300, SpotOre: 240, Confidence: 1, LoadW: 2500, Limits: PowerLimits{MaxImportW: 8000, MaxExportW: 8000}},
	}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 20,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		TerminalSoCPrice: 20,
	}
	return slots, p
}

func TestValidatePlanAcceptsContinuousPowerTrajectory(t *testing.T) {
	slots, p := externalTestFixture()
	plan := Plan{
		Mode: p.Mode, HorizonSlots: 2, CapacityWh: p.CapacityWh,
		InitialSoCPct: p.InitialSoCPct, TotalCostOre: 29.085,
		Actions: []Action{
			{SlotStartMs: 1, SlotLenMin: 60, BatteryW: 1234.5, GridW: 1734.5, SoCPct: 31.72775, CostOre: 34.69},
			{SlotStartMs: 3600001, SlotLenMin: 60, BatteryW: -2000, GridW: 500, SoCPct: 10.67511842105263, CostOre: 150},
		},
	}
	// Raw total cost is the sum of both slot costs.
	plan.TotalCostOre = plan.Actions[0].CostOre + plan.Actions[1].CostOre
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("ValidatePlan: %v", err)
	}
}

func TestValidatePlanRejectsBrokenGridBalance(t *testing.T) {
	slots, p := externalTestFixture()
	plan := Optimize(slots, Params{
		Mode: p.Mode, SoCLevels: 21, CapacityWh: p.CapacityWh,
		SoCMinPct: p.SoCMinPct, SoCMaxPct: p.SoCMaxPct, InitialSoCPct: p.InitialSoCPct,
		ActionLevels: 21, MaxChargeW: p.MaxChargeW, MaxDischargeW: p.MaxDischargeW,
		ChargeEfficiency: p.ChargeEfficiency, DischargeEfficiency: p.DischargeEfficiency,
		TerminalSoCPrice: p.TerminalSoCPrice,
	})
	plan.Actions[0].GridW += 100
	if err := ValidatePlan(slots, p, &plan); err == nil {
		t.Fatal("ValidatePlan accepted broken grid balance")
	}
}

func TestValidatePlanAcceptsSubWattSolverResidueInPassiveMode(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 15, PriceOre: 100, Confidence: 1, LoadW: 0}}
	p := Params{
		Mode: ModePassiveArbitrage, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 1, DischargeEfficiency: 1,
	}
	plan := Plan{TotalCostOre: -0.0000025, Actions: []Action{{
		SlotStartMs: 1, SlotLenMin: 15, BatteryW: -0.0001, GridW: -0.0001,
		SoCPct: 49.99999975, CostOre: -0.0000025,
	}}}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("ValidatePlan rejected numerical solver residue: %v", err)
	}
}

func TestValidatePlanModeErrorIncludesPowerValues(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 15, PriceOre: 100, Confidence: 1, LoadW: 0}}
	p := Params{
		Mode: ModePassiveArbitrage, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 1, DischargeEfficiency: 1,
	}
	plan := Plan{TotalCostOre: -0.0025, Actions: []Action{{
		SlotStartMs: 1, SlotLenMin: 15, BatteryW: -0.2, GridW: -0.2,
		SoCPct: 49.9995, CostOre: -0.005,
	}}}
	plan.TotalCostOre = plan.Actions[0].CostOre
	err := ValidatePlan(slots, p, &plan)
	if err == nil || !strings.Contains(err.Error(), "baseline_grid_w=") || !strings.Contains(err.Error(), "battery_w=") {
		t.Fatalf("expected detailed mode error, got %v", err)
	}
}

func TestExternalOptimizerStopsWorkerAfterIdleTimeout(t *testing.T) {
	if len(os.Args) > 0 && os.Args[len(os.Args)-1] == "external-worker-helper" {
		time.Sleep(10 * time.Second)
		return
	}
	optimizer, err := NewExternalOptimizer(ExternalOptimizerConfig{
		Command: []string{os.Args[0], "-test.run=TestExternalOptimizerStopsWorkerAfterIdleTimeout", "--", "external-worker-helper"},
		Timeout: time.Second, IdleTimeout: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer optimizer.Close()

	optimizer.mu.Lock()
	if err := optimizer.ensureStartedLocked(); err != nil {
		optimizer.mu.Unlock()
		t.Fatal(err)
	}
	firstProcess := optimizer.cmd.Process.Pid
	optimizer.scheduleIdleStopLocked()
	optimizer.mu.Unlock()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		optimizer.mu.Lock()
		stopped := optimizer.cmd == nil
		optimizer.mu.Unlock()
		if stopped {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	optimizer.mu.Lock()
	if optimizer.cmd != nil {
		optimizer.mu.Unlock()
		t.Fatal("worker remained running after idle timeout")
	}
	if err := optimizer.ensureStartedLocked(); err != nil {
		optimizer.mu.Unlock()
		t.Fatal(err)
	}
	secondProcess := optimizer.cmd.Process.Pid
	optimizer.mu.Unlock()
	if secondProcess == firstProcess {
		t.Fatalf("worker did not restart: pid=%d", firstProcess)
	}
}

func TestValidatePlanAllowsButDoesNotWorsenInitialSoCBelowMinimum(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 60, PriceOre: 100, SpotOre: 50, Confidence: 1, LoadW: 500}}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 5,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	}
	plan := Plan{TotalCostOre: 50, Actions: []Action{{
		SlotStartMs: 1, SlotLenMin: 60, BatteryW: 0, GridW: 500, SoCPct: 5, CostOre: 50,
	}}}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("ValidatePlan rejected stable recovery state: %v", err)
	}
	plan.Actions[0] = Action{
		SlotStartMs: 1, SlotLenMin: 60, BatteryW: -100, GridW: 400,
		SoCPct: 3.947368421052632, CostOre: 40,
	}
	plan.TotalCostOre = 40
	if err := ValidatePlan(slots, p, &plan); err == nil {
		t.Fatal("ValidatePlan accepted worsening SoC below minimum")
	}
}

func TestValidatePlanRejectsBatteryFedSurplusLoadpoint(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 60, PriceOre: 100, SpotOre: 70, Confidence: 1, LoadW: 500}}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		Loadpoint: &LoadpointSpec{
			ID: "car", CapacityWh: 40000, Levels: 11, MinPct: 0, MaxPct: 100,
			InitialSoCPct: 25, PluggedIn: true, MaxChargeW: 2000,
			AllowedStepsW: []float64{0, 2000}, ChargeEfficiency: 1,
			SurplusOnly: true,
		},
	}
	plan := Plan{Mode: p.Mode, HorizonSlots: 1, CapacityWh: p.CapacityWh, InitialSoCPct: 50,
		TotalCostOre: 0, Actions: []Action{{
			SlotStartMs: 1, SlotLenMin: 60,
			BatteryW: -2000, GridW: 500, SoCPct: 28.94736842105263,
			LoadpointW: 2000, LoadpointSoCPct: 30, CostOre: 50,
		}}}
	plan.TotalCostOre = 50
	if err := ValidatePlan(slots, p, &plan); err == nil {
		t.Fatal("ValidatePlan accepted battery-fed surplus-only loadpoint")
	}
}

func TestExternalOptimizerEndToEnd(t *testing.T) {
	python := os.Getenv("FTW_TEST_OPTIMIZER_PYTHON")
	if python == "" {
		t.Skip("FTW_TEST_OPTIMIZER_PYTHON not set")
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	moduleDir := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "optimizer"))
	optimizer, err := NewExternalOptimizer(ExternalOptimizerConfig{
		Command:   []string{python, "-m", "ftw_optimizer.worker"},
		ModuleDir: moduleDir, Timeout: 20 * time.Second,
		Solver: "HIGHS", Formulation: "auto", MIPRelGap: 0.001,
		IdleTimeout: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer optimizer.Close()
	slots, p := externalTestFixture()
	plan, err := optimizer.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	if plan.Solver == nil || plan.Solver.Engine != "cvxpy" || plan.Solver.Backend != "highs" {
		t.Fatalf("unexpected solver metadata: %+v", plan.Solver)
	}
	if plan.Actions[0].BatteryW <= 0 || plan.Actions[1].BatteryW >= 0 {
		t.Fatalf("expected cheap-charge/expensive-discharge plan: %+v", plan.Actions)
	}
	recourse, err := optimizer.OptimizeRecourse(context.Background(), slots, p, 1)
	if err != nil {
		t.Fatalf("OptimizeRecourse: %v", err)
	}
	if recourse.Solver == nil || recourse.Solver.ScenarioPolicy != "recourse" || recourse.Solver.NonAnticipativeSlots != 1 {
		t.Fatalf("unexpected recourse metadata: %+v", recourse.Solver)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		optimizer.mu.Lock()
		stopped := optimizer.cmd == nil
		optimizer.mu.Unlock()
		if stopped {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("real optimizer worker remained running after idle timeout")
}

func TestExternalOptimizerPlansMultipleLoadpoints(t *testing.T) {
	python := os.Getenv("FTW_TEST_OPTIMIZER_PYTHON")
	if python == "" {
		t.Skip("FTW_TEST_OPTIMIZER_PYTHON not set")
	}
	_, file, _, _ := runtime.Caller(0)
	optimizer, err := NewExternalOptimizer(ExternalOptimizerConfig{
		Command:   []string{python, "-m", "ftw_optimizer.worker"},
		ModuleDir: filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "optimizer")),
		Timeout:   20 * time.Second, Solver: "HIGHS", Formulation: "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer optimizer.Close()
	slots, p := externalTestFixture()
	p.Loadpoints = []*LoadpointSpec{
		{ID: "car-a", CapacityWh: 40000, Levels: 11, MinPct: 0, MaxPct: 100, InitialSoCPct: 25, PluggedIn: true, TargetSoCPct: 30, TargetSlotIdx: 1, MaxChargeW: 4000, AllowedStepsW: []float64{0, 2000, 4000}, ChargeEfficiency: 1},
		{ID: "car-b", CapacityWh: 60000, Levels: 11, MinPct: 0, MaxPct: 100, InitialSoCPct: 20, PluggedIn: true, TargetSoCPct: 25, TargetSlotIdx: 1, MaxChargeW: 3000, AllowedStepsW: []float64{0, 3000}, ChargeEfficiency: 1},
	}
	plan, err := optimizer.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	last := plan.Actions[len(plan.Actions)-1]
	if last.LoadpointSoCPctByID["car-a"] < 30-0.02 || last.LoadpointSoCPctByID["car-b"] < 25-0.02 {
		t.Fatalf("targets not met: %+v", last.LoadpointSoCPctByID)
	}
	if len(last.LoadpointPowerW) != 2 {
		t.Fatalf("expected two loadpoint schedules, got %+v", last.LoadpointPowerW)
	}
}

func TestExternalOptimizerPlansAndValidatesMultipleStorages(t *testing.T) {
	python := os.Getenv("FTW_TEST_OPTIMIZER_PYTHON")
	if python == "" {
		t.Skip("FTW_TEST_OPTIMIZER_PYTHON not set")
	}
	_, file, _, _ := runtime.Caller(0)
	optimizer, err := NewExternalOptimizer(ExternalOptimizerConfig{
		Command:   []string{python, "-m", "ftw_optimizer.worker"},
		ModuleDir: filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "optimizer")),
		Timeout:   20 * time.Second, Solver: "HIGHS", Formulation: "auto",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer optimizer.Close()
	slots, p := externalTestFixture()
	p.Storages = []StorageAssetSpec{
		{ID: "battery-a", CapacityWh: 4000, InitialEnergyWh: 800, MinEnergyWh: 400, MaxEnergyWh: 3800, MaxChargeW: 1500, MaxDischargeW: 2000, ChargeEfficiency: 0.95, DischargeEfficiency: 0.95},
		{ID: "battery-b", CapacityWh: 6000, InitialEnergyWh: 1200, MinEnergyWh: 600, MaxEnergyWh: 5700, MaxChargeW: 3500, MaxDischargeW: 3000, ChargeEfficiency: 0.95, DischargeEfficiency: 0.95},
	}
	plan, err := optimizer.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	for i, action := range plan.Actions {
		if len(action.StoragePowerW) != 2 || len(action.StorageEnergyWh) != 2 {
			t.Fatalf("slot %d missing per-storage result: power=%+v energy=%+v", i, action.StoragePowerW, action.StorageEnergyWh)
		}
	}
	plan.Actions[0].StorageEnergyWh["battery-a"] += 100
	if err := ValidatePlan(slots, p, &plan); err == nil {
		t.Fatal("ValidatePlan accepted a corrupted per-storage energy trajectory")
	}
}
