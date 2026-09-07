package mpc

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func topologyFixture(nb, ne int) ([]Slot, Params) {
	slots, p := externalTestFixture()
	p.SoCLevels, p.ActionLevels = 21, 21
	p.CapacityWh = 0
	p.InitialSoC = 0
	p.MaxChargeW = 0
	p.MaxDischargeW = 0
	for i := 0; i < nb; i++ {
		p.Storages = append(p.Storages, StorageAssetSpec{ID: fmt.Sprintf("battery-%d", i), CapacityWh: 5000, InitialEnergyWh: 2500, MinEnergyWh: 500, MaxEnergyWh: 4750,
			MaxChargeW: 2000, MaxDischargeW: 2000, ChargeEfficiency: .9 + float64(i%2)*.05, DischargeEfficiency: .95 - float64(i%2)*.05})
		p.CapacityWh += 5000
		p.MaxChargeW += 2000
		p.MaxDischargeW += 2000
	}
	if nb > 0 {
		p.InitialSoC = .5
	}
	for i := 0; i < ne; i++ {
		p.Loadpoints = append(p.Loadpoints, &LoadpointSpec{ID: fmt.Sprintf("ev-%d", i), CapacityWh: 10000, Levels: 11, SoCMax: 1, InitialSoC: .2, PluggedIn: true,
			TargetSoC: .3, TargetSlotIdx: 1, MaxChargeW: 2000, AllowedStepsW: []float64{0, 1000, 2000}, ChargeEfficiency: 1})
	}
	if ne > 0 {
		p.Loadpoint = p.Loadpoints[0]
	}
	return slots, p
}

func TestNativeAllPhysicalTopologies(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	defer o.Close()
	for _, counts := range [][2]int{{0, 0}, {0, 1}, {0, 2}, {1, 1}, {2, 0}, {2, 1}, {2, 2}, {4, 3}} {
		slots, p := topologyFixture(counts[0], counts[1])
		if err := validatePlanningParams(p); err != nil {
			t.Fatal(err)
		}
		req := o.buildRequest(slots, p)
		if len(req.Storages) != counts[0] || len(req.FlexLoads) != counts[1] {
			t.Fatalf("invented/duplicated assets: %+v", req)
		}
		for _, mode := range []Mode{ModeArbitrage, ModeSelfConsumption, ModeCheapCharge, ModePassiveArbitrage} {
			p.Mode = mode
			plan, err := o.Optimize(context.Background(), slots, p)
			if err != nil {
				t.Fatalf("%v %s: %v", counts, mode, err)
			}
			if err = ValidatePlan(slots, p, &plan); err != nil {
				t.Fatal(err)
			}
			for _, a := range plan.Actions {
				if len(a.StoragePowerW) != counts[0] || len(a.LoadpointPowerW) != counts[1] {
					t.Fatalf("lost assets: %+v", a)
				}
			}
		}
	}
}

func TestNativeExternalBoundaryRejectsLostIdentitiesAndTimeline(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	defer o.Close()
	slots, p := topologyFixture(2, 2)
	req := o.buildRequest(slots, p)
	encoded, _ := json.Marshal(req)
	raw, err := o.transport.RoundTrip(context.Background(), encoded)
	if err != nil {
		t.Fatal(err)
	}
	var original externalResponse
	if err = json.Unmarshal(raw, &original); err != nil || !original.OK {
		t.Fatalf("response=%s err=%v", raw, err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*externalPlan)
	}{
		{"lost batteries", func(p *externalPlan) { p.Actions[0].StoragePowerW = nil; p.Actions[0].StorageEnergy = nil }},
		{"lost EV", func(p *externalPlan) { delete(p.Actions[0].FlexPowerW, "ev-1") }},
		{"extra EV", func(p *externalPlan) { p.Actions[0].FlexPowerW["ghost"] = 0 }},
		{"wrong time", func(p *externalPlan) { p.Actions[0].SlotStartMs++ }},
		{"wrong length", func(p *externalPlan) { p.Actions[0].SlotLenMin = 15 }},
		{"extra action", func(p *externalPlan) { p.Actions = append(p.Actions, p.Actions[0]) }},
		{"missed deadline", func(p *externalPlan) { p.Actions[1].FlexEnergyWh["ev-1"] = 2000 }},
		{"PV without capability", func(p *externalPlan) { p.Actions[0].PVCurtailActive = true; p.Actions[0].PVLimitW = 100 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r externalResponse
			json.Unmarshal(raw, &r)
			tc.mutate(&r.Plan)
			if err := validateExternalAssets(req, r.Plan); err == nil {
				t.Fatal("invalid worker contract accepted")
			}
		})
	}
	// The raw response gate checks the driver ceiling before translation.
	minPV, maxPV := 2.0, 15000.0
	req.Settings.PVCurtailmentMinW, req.Settings.PVCurtailmentMaxW = &minPV, &maxPV
	for _, capW := range []float64{16000, 2.5} {
		var r externalResponse
		json.Unmarshal(raw, &r)
		r.Plan.Actions[0].PVCurtailActive, r.Plan.Actions[0].PVLimitW = true, capW
		if err := validateExternalAssets(req, r.Plan); err == nil {
			t.Fatalf("unexecutable PV cap %v accepted", capW)
		}
	}
	original.Plan.Actions[0].SlotStartMs++
	translated := original.toPlan(slots, p)
	if err := ValidatePlan(slots, p, &translated); err == nil {
		t.Fatal("translation hid a wrong timeline")
	}
}

func TestNativeFleetFailureCannotDropAssetsIntoDP(t *testing.T) {
	for _, fault := range []string{"timeout", "invalid_plan"} {
		t.Run(fault, func(t *testing.T) {
			o := nativeWorker(t, 500*time.Millisecond)
			defer o.Close()
			wrapped := &EnergyplanOptimizer{ExternalOptimizer: o}
			svc := shadowTestService(t)
			_, p := topologyFixture(2, 2)
			svc.Defaults = p
			svc.Optimizer = wrapped
			// This service has a single one-hour slot, so both targets fit it.
			svc.Loadpoints = func(int) []*LoadpointSpec { return p.Loadpoints }
			accepted := svc.Replan(context.Background())
			if accepted == nil {
				t.Fatal("no baseline fleet plan")
			}
			before, _ := json.Marshal(accepted)
			o.cfg.Timeout = 25 * time.Millisecond
			o.transport = &energyplanFaultTransport{OptimizerTransport: o.transport, fault: fault}
			if got := svc.Replan(context.Background()); got != accepted {
				t.Fatal("fallback replaced a fleet plan")
			}
			after, _ := json.Marshal(svc.Latest())
			if string(before) != string(after) {
				t.Fatal("failed fallback altered accepted plan")
			}
			if _, ok := svc.SlotDirectiveAt(time.Now()); ok {
				t.Fatal("failed replacement still claims current plan")
			}
		})
	}
}

func TestSharedPVAndStorageMapValidation(t *testing.T) {
	slots, p := topologyFixture(0, 2)
	slots = slots[:1]
	slots[0].PVW = -3500
	slots[0].LoadW = 500
	for _, e := range p.Loadpoints {
		e.SurplusOnly = true
	}
	a := Action{SlotStartMs: slots[0].StartMs, SlotLenMin: 60, GridW: 1000, CostOre: 20, LoadpointPowerW: map[string]float64{"ev-0": 2000, "ev-1": 2000}, LoadpointSoCByID: map[string]float64{"ev-0": .4, "ev-1": .4}}
	plan := Plan{Actions: []Action{a}, TotalCostOre: 20}
	if err := ValidatePlan(slots, p, &plan); err == nil {
		t.Fatal("two EVs consumed the same PV surplus")
	}
	_, p = topologyFixture(2, 0)
	if err := validateAssetMaps(p, Action{}); err == nil {
		t.Fatal("physical storages disappeared")
	}
}

func TestPVCurtailmentProofCannotSurviveDiagnosticRestore(t *testing.T) {
	p := PVCurtailment{Driver: "pv", Proof: "runtime-only", MinW: 2, MaxW: 15000}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored PVCurtailment
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Valid() || restored.Proof != "" {
		t.Fatal("stored diagnostics granted a new process a control capability")
	}
}
