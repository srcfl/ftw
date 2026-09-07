package mpc

import (
	"context"
	"errors"
	"os"
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

func TestValidatePlanGridLimitAllowsOnlySubWattSolverResidue(t *testing.T) {
	const limitW = 11040.0
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 1, DischargeEfficiency: 1,
	}
	tests := []struct {
		name    string
		gridW   float64
		wantErr bool
	}{
		{name: "import solver residue", gridW: limitW + 0.000001},
		{name: "export solver residue", gridW: -limitW - 0.000001},
		{name: "import real violation", gridW: limitW + 1, wantErr: true},
		{name: "export real violation", gridW: -limitW - 1, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			slot := Slot{
				StartMs: 1, LenMin: 15, PriceOre: 100, SpotOre: 50, Confidence: 1,
				Limits: PowerLimits{MaxImportW: limitW, MaxExportW: limitW},
			}
			if tc.gridW > 0 {
				slot.LoadW = tc.gridW
			} else {
				slot.PVW = tc.gridW
			}
			costOre := SlotGridCostOre(slot, tc.gridW*0.25/1000, p)
			plan := Plan{TotalCostOre: costOre, Actions: []Action{{
				SlotStartMs: 1, SlotLenMin: 15, GridW: tc.gridW,
				SoC: 0.5, CostOre: costOre,
			}}}
			err := ValidatePlan([]Slot{slot}, p, &plan)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidatePlan() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
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

// gridLimitFixture builds a single arbitrage slot whose baseline grid flow is
// exactly gridW, under an 11 040 W fuse (16 A x 3 x 230 V) and an 8 000 W
// export cap, plus the matching zero-battery plan. Grid balance, mode and cost
// all reconcile, so only the grid-limit check can reject it.
func gridLimitFixture(gridW float64) ([]Slot, Params, Plan) {
	slot := Slot{
		StartMs: 1, LenMin: 15, PriceOre: 100, SpotOre: 80, Confidence: 1,
		Limits: PowerLimits{MaxImportW: 11040, MaxExportW: 8000},
	}
	if gridW >= 0 {
		slot.LoadW = gridW
	} else {
		slot.PVW = gridW
	}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 1, DischargeEfficiency: 1,
	}
	costOre := SlotGridCostOre(slot, gridW*0.25/1000, p)
	plan := Plan{TotalCostOre: costOre, Actions: []Action{{
		SlotStartMs: 1, SlotLenMin: 15, BatteryW: 0, GridW: gridW,
		SoC: p.InitialSoC, CostOre: costOre,
	}}}
	return []Slot{slot}, p, plan
}

func TestValidatePlanAcceptsGridFlowRidingTheLimit(t *testing.T) {
	slots, p, plan := gridLimitFixture(11040 + 1e-9)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("ValidatePlan rejected solver residue at the import limit: %v", err)
	}
	slots, p, plan = gridLimitFixture(-8000 - 1e-9)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("ValidatePlan rejected solver residue at the export limit: %v", err)
	}
}

func TestValidatePlanRejectsGridFlowPastTheLimit(t *testing.T) {
	slots, p, plan := gridLimitFixture(11040 + 5)
	err := ValidatePlan(slots, p, &plan)
	if err == nil || !strings.Contains(err.Error(), "violates grid limits") {
		t.Fatalf("ValidatePlan 5 W over the import limit = %v, want a grid-limit rejection", err)
	}
	slots, p, plan = gridLimitFixture(-8000 - 5)
	err = ValidatePlan(slots, p, &plan)
	if err == nil || !strings.Contains(err.Error(), "violates grid limits") {
		t.Fatalf("ValidatePlan 5 W past the export limit = %v, want a grid-limit rejection", err)
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

// The champion's scenarios and the Go fallback's downside slots have to
// describe the same physics. Once the twin has learned a relative error, the
// scenario spread is a share of each slot's own generation, not one watt
// figure repeated across the horizon.
func TestBuildRequestScenarioSpreadIsPerSlotWhenRelativeIsLearned(t *testing.T) {
	start := time.Date(2026, 8, 30, 4, 0, 0, 0, time.UTC).UnixMilli()
	gen := []float64{0, 500, 6000}
	slots := make([]Slot, len(gen))
	for i, g := range gen {
		slots[i] = Slot{
			StartMs: start + int64(i)*15*60*1000, LenMin: 15,
			PriceOre: 100, SpotOre: 50, LoadW: 400, PVW: -g, Confidence: 1,
		}
	}
	p := Params{
		Mode: ModeArbitrage, SoCMin: 0.1, SoCMax: 0.95, SoCLevels: 11,
		InitialSoC: 0.5, ActionLevels: 7, CapacityWh: 10000,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		PVUncertaintyW: 1891, PVRelativeUncertainty: 0.25, PVForecastSafetyK: 1,
	}

	req := (&ExternalOptimizer{}).buildRequest(slots, p)
	if len(req.Scenarios) != 3 {
		t.Fatalf("got %d scenarios, want 3", len(req.Scenarios))
	}
	down, ok := req.Scenarios[1]["pv_w"].([]float64)
	if !ok {
		t.Fatalf("downside pv_w has type %T", req.Scenarios[1]["pv_w"])
	}
	up, ok := req.Scenarios[2]["pv_w"].([]float64)
	if !ok {
		t.Fatalf("upside pv_w has type %T", req.Scenarios[2]["pv_w"])
	}
	for i, g := range gen {
		wantDown, wantUp := -(g * 0.75), -(g * 1.25)
		if g == 0 {
			wantDown, wantUp = 0, 0 // night slots carry no spread either way
		}
		if down[i] != wantDown {
			t.Errorf("downside[%d] = %v, want %v", i, down[i], wantDown)
		}
		if up[i] != wantUp {
			t.Errorf("upside[%d] = %v, want %v", i, up[i], wantUp)
		}
	}
}

// With no learned relative error the scenarios keep the flat watt spread, so
// a fresh site's champion sees exactly what it saw before.
func TestBuildRequestScenarioSpreadStaysFlatWhenRelativeIsUnlearned(t *testing.T) {
	start := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC).UnixMilli()
	slots := []Slot{
		{StartMs: start, LenMin: 15, PriceOre: 100, SpotOre: 50,
			LoadW: 400, PVW: -6000, Confidence: 1},
		{StartMs: start + 15*60*1000, LenMin: 15, PriceOre: 100, SpotOre: 50,
			LoadW: 400, PVW: -500, Confidence: 1},
	}
	p := Params{
		Mode: ModeArbitrage, SoCMin: 0.1, SoCMax: 0.95, SoCLevels: 11,
		InitialSoC: 0.5, ActionLevels: 7, CapacityWh: 10000,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		PVUncertaintyW: 2000, PVForecastSafetyK: 1,
	}

	req := (&ExternalOptimizer{}).buildRequest(slots, p)
	if len(req.Scenarios) != 3 {
		t.Fatalf("got %d scenarios, want 3", len(req.Scenarios))
	}
	down := req.Scenarios[1]["pv_w"].([]float64)
	if down[0] != -4000 {
		t.Errorf("downside[0] = %v, want -4000 (6000 − 2000)", down[0])
	}
	if down[1] != 0 {
		t.Errorf("downside[1] = %v, want 0 (500 W shoulder minus a 2000 W flat cut)", down[1])
	}
}
