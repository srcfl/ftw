package mpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/optimizercontract"
)

type externalOptimizerTransportStub struct {
	healthErr    error
	roundTripErr error
}

func (t *externalOptimizerTransportStub) RoundTrip(context.Context, []byte) ([]byte, error) {
	return nil, t.roundTripErr
}

func (t *externalOptimizerTransportStub) Health(context.Context) (OptimizerRuntimeInfo, error) {
	return OptimizerRuntimeInfo{Features: []string{"champion"}}, t.healthErr
}

func (t *externalOptimizerTransportStub) Close() error { return nil }

func externalTestFixture() ([]Slot, Params) {
	slots := []Slot{
		{StartMs: 1, LenMin: 60, PriceOre: 20, SpotOre: 10, Confidence: 1, LoadW: 500, Limits: PowerLimits{MaxImportW: 8000, MaxExportW: 8000}},
		{StartMs: 3600001, LenMin: 60, PriceOre: 300, SpotOre: 240, Confidence: 1, LoadW: 2500, Limits: PowerLimits{MaxImportW: 8000, MaxExportW: 8000}},
	}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.2,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		TerminalSoCPrice: 20,
	}
	return slots, p
}

func TestExternalOptimizerPreservesExplicitZeroServiceCVaRWeight(t *testing.T) {
	zero := 0.0
	optimizer, err := NewExternalOptimizer(ExternalOptimizerConfig{
		Command: []string{"python3"},
		Multistage: MultistageOptimizerConfig{
			ServiceCVaRWeight: &zero,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := *optimizer.cfg.Multistage.ServiceCVaRWeight; got != 0 {
		t.Fatalf("explicit zero weight replaced by default: %g", got)
	}
}

func TestExternalOptimizerUsesSharedDefaultTimeout(t *testing.T) {
	optimizer, err := NewExternalOptimizer(ExternalOptimizerConfig{
		Command: []string{"python3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if optimizer.cfg.Timeout != optimizercontract.DefaultTimeout {
		t.Fatalf("timeout = %s, want %s", optimizer.cfg.Timeout, optimizercontract.DefaultTimeout)
	}
}

func TestExternalOptimizerTimeoutPreservesAutoTransportCauses(t *testing.T) {
	sidecarErr := errors.New("connection closed")
	transport := NewAutoTransport(
		&externalOptimizerTransportStub{healthErr: sidecarErr},
		&externalOptimizerTransportStub{roundTripErr: context.DeadlineExceeded},
	)
	optimizer, err := NewExternalOptimizer(ExternalOptimizerConfig{
		Transport: transport,
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	slots, params := externalTestFixture()
	_, err = optimizer.Optimize(context.Background(), slots, params)
	if err == nil {
		t.Fatal("Optimize succeeded, want timeout")
	}
	if !errors.Is(err, sidecarErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Optimize error does not unwrap sidecar and timeout failures: %v", err)
	}
	message := err.Error()
	sidecarAt := strings.Index(message, sidecarErr.Error())
	fallbackAt := strings.Index(message, context.DeadlineExceeded.Error())
	if !strings.HasPrefix(message, "optimizer timeout after 1s: ") ||
		sidecarAt < 0 || fallbackAt < 0 || sidecarAt >= fallbackAt {
		t.Fatalf("Optimize error = %q, want timeout class with sidecar cause first", message)
	}
}

func TestValidatePlanAcceptsContinuousPowerTrajectory(t *testing.T) {
	slots, p := externalTestFixture()
	plan := Plan{
		Mode: p.Mode, HorizonSlots: 2, CapacityWh: p.CapacityWh,
		InitialSoC: p.InitialSoC, TotalCostOre: 29.085,
		Actions: []Action{
			{SlotStartMs: 1, SlotLenMin: 60, BatteryW: 1234.5, GridW: 1734.5, SoC: 0.317277, CostOre: 34.69},
			{SlotStartMs: 3600001, SlotLenMin: 60, BatteryW: -2000, GridW: 500, SoC: 0.106751, CostOre: 150},
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
		SoCMin: p.SoCMin, SoCMax: p.SoCMax, InitialSoC: p.InitialSoC,
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
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 1, DischargeEfficiency: 1,
	}
	plan := Plan{TotalCostOre: -0.0000025, Actions: []Action{{
		SlotStartMs: 1, SlotLenMin: 15, BatteryW: -0.0001, GridW: -0.0001,
		SoC: 0.5, CostOre: -0.0000025,
	}}}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("ValidatePlan rejected numerical solver residue: %v", err)
	}
}

func TestValidatePlanModeErrorIncludesPowerValues(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 15, PriceOre: 100, Confidence: 1, LoadW: 0}}
	p := Params{
		Mode: ModePassiveArbitrage, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 1, DischargeEfficiency: 1,
	}
	plan := Plan{TotalCostOre: -0.0025, Actions: []Action{{
		SlotStartMs: 1, SlotLenMin: 15, BatteryW: -0.2, GridW: -0.2,
		SoC: 0.499995, CostOre: -0.005,
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
	transport, ok := optimizer.transport.(*ProcessTransport)
	if !ok {
		t.Fatalf("transport = %T, want *ProcessTransport", optimizer.transport)
	}

	transport.mu.Lock()
	if err := transport.ensureStartedLocked(); err != nil {
		transport.mu.Unlock()
		t.Fatal(err)
	}
	firstProcess := transport.cmd.Process.Pid
	transport.scheduleIdleStopLocked()
	transport.mu.Unlock()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		transport.mu.Lock()
		stopped := transport.cmd == nil
		transport.mu.Unlock()
		if stopped {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	transport.mu.Lock()
	if transport.cmd != nil {
		transport.mu.Unlock()
		t.Fatal("worker remained running after idle timeout")
	}
	if err := transport.ensureStartedLocked(); err != nil {
		transport.mu.Unlock()
		t.Fatal(err)
	}
	secondProcess := transport.cmd.Process.Pid
	transport.mu.Unlock()
	if secondProcess == firstProcess {
		t.Fatalf("worker did not restart: pid=%d", firstProcess)
	}
}

func TestValidatePlanAllowsButDoesNotWorsenInitialSoCBelowMinimum(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 60, PriceOre: 100, SpotOre: 50, Confidence: 1, LoadW: 500}}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.05,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	}
	plan := Plan{TotalCostOre: 50, Actions: []Action{{
		SlotStartMs: 1, SlotLenMin: 60, BatteryW: 0, GridW: 500, SoC: 0.05, CostOre: 50,
	}}}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("ValidatePlan rejected stable recovery state: %v", err)
	}
	plan.Actions[0] = Action{
		SlotStartMs: 1, SlotLenMin: 60, BatteryW: -100, GridW: 400,
		SoC: 0.039474, CostOre: 40,
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
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		Loadpoint: &LoadpointSpec{
			ID: "car", CapacityWh: 40000, Levels: 11, SoCMin: 0, SoCMax: 1.0,
			InitialSoC: 0.25, PluggedIn: true, MaxChargeW: 2000,
			AllowedStepsW: []float64{0, 2000}, ChargeEfficiency: 1,
			SurplusOnly: true,
		},
	}
	plan := Plan{Mode: p.Mode, HorizonSlots: 1, CapacityWh: p.CapacityWh, InitialSoC: 0.5,
		TotalCostOre: 0, Actions: []Action{{
			SlotStartMs: 1, SlotLenMin: 60,
			BatteryW: -2000, GridW: 500, SoC: 0.289474,
			LoadpointW: 2000, LoadpointSoC: 0.3, CostOre: 50,
		}}}
	plan.TotalCostOre = 50
	if err := ValidatePlan(slots, p, &plan); err == nil {
		t.Fatal("ValidatePlan accepted battery-fed surplus-only loadpoint")
	}
}

func TestValidatePlanAllowsGridChargeWithIdleSurplusOnlyEV(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 60, PriceOre: 30, SpotOre: 10, Confidence: 1, LoadW: 500}}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.2,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		Loadpoint: &LoadpointSpec{
			ID: "car", CapacityWh: 40000, Levels: 11, SoCMin: 0, SoCMax: 1.0,
			InitialSoC: 0.8, PluggedIn: true, MaxChargeW: 2000,
			AllowedStepsW: []float64{0, 2000}, ChargeEfficiency: 1,
			SurplusOnly: true,
		},
	}
	// 4000 W charge for 1 h at 95% from 20% of 10 kWh → 20 + 38 = 58%.
	plan := Plan{Mode: p.Mode, HorizonSlots: 1, CapacityWh: p.CapacityWh, InitialSoC: 0.2,
		TotalCostOre: 135, Actions: []Action{{
			SlotStartMs: 1, SlotLenMin: 60,
			BatteryW: 4000, GridW: 4500, SoC: 0.58,
			LoadpointW: 0, LoadpointSoC: 0.8, CostOre: 135,
		}}}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("ValidatePlan rejected idle surplus-only EV plus battery grid-charge: %v", err)
	}
}

func TestValidatePlanAllowsEVPVWithBatteryGridCharge(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 60, PriceOre: 20, SpotOre: 10, Confidence: 1, LoadW: 500, PVW: -6500}}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMin: 0.10, SoCMax: 0.95, InitialSoC: 0.20,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		Loadpoint: &LoadpointSpec{
			ID: "car", CapacityWh: 40000, Levels: 11, SoCMin: 0, SoCMax: 1,
			InitialSoC: 0.25, PluggedIn: true, MaxChargeW: 4140,
			AllowedStepsW: []float64{0, 4140}, ChargeEfficiency: 1,
			SurplusOnly: true, NoBatteryToEV: true,
		},
	}
	// leftover PV after house = 6000 W. EV 4140 + battery 5000 →
	// grid = 500-6500+5000+4140 = 3140 import. Battery SoC: 0.20 + 0.475 = 0.675.
	// EV SoC: 0.25 + 4140/40000 = 0.3535.
	plan := Plan{Mode: p.Mode, HorizonSlots: 1, CapacityWh: p.CapacityWh, InitialSoC: 0.20,
		TotalCostOre: 62.8, Actions: []Action{{
			SlotStartMs: 1, SlotLenMin: 60,
			BatteryW: 5000, GridW: 3140, SoC: 0.675,
			LoadpointW: 4140, LoadpointSoC: 0.3535, CostOre: 62.8,
		}}}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("ValidatePlan rejected leftover-PV EV beside battery grid-charge: %v", err)
	}
}

func TestValidatePlanRejectsSurplusOnlyEVAboveLeftoverPV(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 60, PriceOre: 20, SpotOre: 10, Confidence: 1, LoadW: 500, PVW: -6500}}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMin: 0.10, SoCMax: 0.95, InitialSoC: 0.20,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		Loadpoint: &LoadpointSpec{
			ID: "car", CapacityWh: 40000, Levels: 11, SoCMin: 0, SoCMax: 1,
			InitialSoC: 0.25, PluggedIn: true, MaxChargeW: 11000,
			AllowedStepsW: []float64{0, 7000}, ChargeEfficiency: 1,
			SurplusOnly: true, NoBatteryToEV: true,
		},
	}
	// leftover after house = 6000 W. EV 7000 exceeds it even though
	// the home battery is the one importing.
	plan := Plan{Mode: p.Mode, HorizonSlots: 1, CapacityWh: p.CapacityWh, InitialSoC: 0.20,
		TotalCostOre: 120, Actions: []Action{{
			SlotStartMs: 1, SlotLenMin: 60,
			BatteryW: 5000, GridW: 6000, SoC: 0.675,
			LoadpointW: 7000, LoadpointSoC: 0.425, CostOre: 120,
		}}}
	if err := ValidatePlan(slots, p, &plan); err == nil {
		t.Fatal("ValidatePlan accepted surplus-only EV above leftover PV")
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
	if plan.Solver == nil || plan.Solver.Engine != "highspy" || plan.Solver.Backend != "highs" ||
		plan.Solver.ScenarioPolicy != "shared" || plan.Solver.PolicyVersion != "shared-v1" {
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
	multistage, err := optimizer.OptimizeMultistage(context.Background(), slots, p, 1)
	if err != nil {
		t.Fatalf("OptimizeMultistage: %v", err)
	}
	if multistage.Solver == nil || multistage.Solver.ScenarioPolicy != "multistage" || multistage.Solver.PolicyVersion != "storage-multistage-v1" {
		t.Fatalf("unexpected multistage metadata: %+v", multistage.Solver)
	}
	if multistage.Solver.PolicyConfig == "" || multistage.Solver.ModelVariables == 0 || multistage.Solver.ModelConstraints == 0 {
		t.Fatalf("missing direct multistage topology metadata: %+v", multistage.Solver)
	}
	transport, ok := optimizer.transport.(*ProcessTransport)
	if !ok {
		t.Fatalf("transport = %T, want *ProcessTransport", optimizer.transport)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		transport.mu.Lock()
		stopped := transport.cmd == nil
		transport.mu.Unlock()
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
		{ID: "car-a", CapacityWh: 40000, Levels: 11, SoCMin: 0, SoCMax: 1.0, InitialSoC: 0.25, PluggedIn: true, TargetSoC: 0.3, TargetSlotIdx: 1, MaxChargeW: 4000, AllowedStepsW: []float64{0, 2000, 4000}, ChargeEfficiency: 1},
		{ID: "car-b", CapacityWh: 60000, Levels: 11, SoCMin: 0, SoCMax: 1.0, InitialSoC: 0.2, PluggedIn: true, TargetSoC: 0.25, TargetSlotIdx: 1, MaxChargeW: 3000, AllowedStepsW: []float64{0, 3000}, ChargeEfficiency: 1},
	}
	plan, err := optimizer.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatalf("Optimize: %v", err)
	}
	last := plan.Actions[len(plan.Actions)-1]
	if last.LoadpointSoCByID["car-a"] < 0.30-0.02 || last.LoadpointSoCByID["car-b"] < 0.25-0.02 {
		t.Fatalf("targets not met: %+v", last.LoadpointSoCByID)
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
