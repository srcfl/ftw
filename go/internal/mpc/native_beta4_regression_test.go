package mpc

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"
)

func TestNativeBeta4SharedDeadlinesKeepSafePartialPlan(t *testing.T) {
	worker := nativeWorker(t, time.Second)
	defer worker.Close()
	slots, p := externalTestFixture()
	slots = slots[:2]
	for i := range slots {
		slots[i].StartMs = int64(i+1) * 3600000
		slots[i].LenMin = 60
		slots[i].LoadW, slots[i].PVW = 0, 0
		slots[i].Limits.MaxImportW = 1000
	}
	p.MaxChargeW, p.MaxDischargeW = 0, 0
	p.Loadpoints = []*LoadpointSpec{
		{ID: "early", CapacityWh: 10000, Levels: 11, SoCMax: 1, PluggedIn: true, TargetSoC: .1, TargetSlotIdx: 0, MaxChargeW: 1000, AllowedStepsW: []float64{0, 1000}, ChargeEfficiency: 1},
		{ID: "later", CapacityWh: 10000, Levels: 11, SoCMax: 1, PluggedIn: true, TargetSoC: .2, TargetSlotIdx: 1, MaxChargeW: 1000, AllowedStepsW: []float64{0, 1000}, ChargeEfficiency: 1},
	}
	p.Loadpoint = p.Loadpoints[0]
	plan, err := worker.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(plan.Actions[0].LoadpointSoCByID["early"]-.1) > 1e-7 || math.Abs(plan.Actions[1].LoadpointSoCByID["later"]-.1) > 1e-7 || math.Abs(plan.LoadpointShortfallWh["later"]-1000) > 1e-4 {
		t.Fatalf("shared deadline energy or shortfall lost: %+v", plan)
	}
}

// Site captures stay outside the repo. This checks a single-battery protocol
// request against Core's real validator, either with a local worker or with
// a response produced on another CPU. It never sends a hardware command.
func TestNativeCapturedSiteReplay(t *testing.T) {
	capture := os.Getenv("FTW_NATIVE_CAPTURE_REQUEST")
	if capture == "" {
		t.Skip("set FTW_NATIVE_CAPTURE_REQUEST to an optimizer_input JSON file")
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var q externalRequest
	if err := json.Unmarshal(data, &q); err != nil {
		t.Fatal(err)
	}
	if len(q.Storages) != 1 || len(q.Scenarios) != 0 || len(q.DemandCharges) != 0 || q.Settings.PVCurtailmentMinW != nil || len(q.ThermalLoads) != 0 {
		t.Fatal("capture replay supports one battery without scenarios, demand charges, thermal loads or PV controls")
	}
	b := q.Storages[0]
	p := Params{Mode: q.Settings.Mode, CapacityWh: b.CapacityWh, InitialSoC: b.InitialEnergyWh / b.CapacityWh, SoCMin: b.MinEnergyWh / b.CapacityWh, SoCMax: b.MaxEnergyWh / b.CapacityWh,
		MaxChargeW: b.MaxChargeW, MaxDischargeW: b.MaxDischargeW, ChargeEfficiency: b.ChargeEfficiency, DischargeEfficiency: b.DischargeEfficiency,
		TerminalSoCPrice: b.TerminalPriceOreKWh, ExportOrePerKWh: q.Settings.ExportOrePerKWh, ExportBonusOreKwh: q.Settings.ExportBonusOreKwh, ExportFeeOreKwh: q.Settings.ExportFeeOreKwh, ExportFloorOreKwh: q.Settings.ExportFloorOreKwh,
		MinArbitrageSpreadOreKwh: q.Settings.MinArbitrageSpreadOreKwh, PVChargeBonusOreKwh: q.Settings.PVChargeBonusOreKwh,
		Storages: []StorageAssetSpec{{ID: b.ID, CapacityWh: b.CapacityWh, InitialEnergyWh: b.InitialEnergyWh, MinEnergyWh: b.MinEnergyWh, MaxEnergyWh: b.MaxEnergyWh, MaxChargeW: b.MaxChargeW, MaxDischargeW: b.MaxDischargeW, ChargeEfficiency: b.ChargeEfficiency, DischargeEfficiency: b.DischargeEfficiency}},
	}
	for _, e := range q.FlexLoads {
		p.Loadpoints = append(p.Loadpoints, &LoadpointSpec{ID: e.ID, Levels: 101, CapacityWh: e.CapacityWh, InitialSoC: e.InitialEnergyWh / e.CapacityWh, SoCMax: e.MaxEnergyWh / e.CapacityWh, TargetSoC: e.TargetEnergyWh / e.CapacityWh,
			TargetSlotIdx: e.TargetSlot, ChargeEfficiency: e.ChargeEfficiency, MaxChargeW: e.MaxChargeW, AllowedStepsW: e.AllowedStepsW, SurplusOnly: e.SurplusOnly, NoBatteryToEV: e.NoStorageToLoad, PluggedIn: true})
	}
	if len(p.Loadpoints) > 0 {
		p.Loadpoint = p.Loadpoints[0]
	}
	var slots []Slot
	for _, s := range q.Slots {
		slots = append(slots, Slot{StartMs: s.StartMs, ExecutionStartMs: s.ExecutionStartMs, LenMin: s.LenMin, PriceOre: s.PriceOre, SpotOre: s.SpotOre, LoadW: s.LoadW, PVW: s.PVW, Limits: PowerLimits{MaxImportW: s.MaxImportW, MaxExportW: s.MaxExportW}})
	}
	var response []byte
	if path := os.Getenv("FTW_NATIVE_CAPTURE_RESPONSE"); path != "" {
		response, err = os.ReadFile(path)
	} else {
		worker := nativeWorker(t, 5*time.Second)
		defer worker.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		response, err = worker.transport.RoundTrip(ctx, data)
	}
	if err != nil {
		t.Fatal(err)
	}
	var result externalResponse
	if err := json.Unmarshal(response, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.RequestID != q.RequestID {
		t.Fatalf("capture response mismatch: %s", response)
	}
	if err := validateExternalAssets(q, result.Plan); err != nil {
		t.Fatal(err)
	}
	plan := result.toPlan(slots, p)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	t.Logf("Core accepted %d slots; solver=%+v; shortfall=%v", len(slots), plan.Solver, plan.LoadpointShortfallWh)
}
