package mpc

import (
	"math"
	"testing"
)

func TestOptimizePlansPassValidatePlan(t *testing.T) {
	cases := []struct {
		name  string
		slots []Slot
		p     Params
	}{
		{
			name:  "self_consumption flat load",
			slots: flatLoadSlots([]float64{100, 200, 50, 300}),
			p: func() Params {
				p := baseParams(ModeSelfConsumption)
				p.InitialSoCPct = 80
				return p
			}(),
		},
		{
			name: "self_consumption pv surplus",
			slots: []Slot{
				{StartMs: 0, LenMin: 60, PriceOre: 100, Confidence: 1, LoadW: 2000, PVW: -3500},
			},
			p: baseParams(ModeSelfConsumption),
		},
		{
			name:  "passive_arbitrage",
			slots: flatLoadSlots([]float64{40, 200, 40, 250}),
			p:     baseParams(ModePassiveArbitrage),
		},
		{
			name:  "arbitrage",
			slots: flatLoadSlots([]float64{20, 250, 20, 300}),
			p:     baseParams(ModeArbitrage),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := Optimize(tc.slots, tc.p)
			if len(plan.Actions) == 0 {
				t.Fatal("Optimize returned no actions")
			}
			if err := ValidatePlan(tc.slots, tc.p, &plan); err != nil {
				t.Fatalf("ValidatePlan: %v", err)
			}
		})
	}
}

func TestValidatePlanAcceptsGoDPCurtailHint(t *testing.T) {
	slots := []Slot{{
		StartMs: 1, LenMin: 60, PriceOre: 100, SpotOre: -50, Confidence: 1,
		LoadW: 500, PVW: -5000,
	}}
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 90
	plan := Optimize(slots, p)
	if len(plan.Actions) != 1 {
		t.Fatalf("got %d actions", len(plan.Actions))
	}
	if plan.Actions[0].PVLimitW <= 0 {
		t.Fatalf("expected a curtail hint, got pv_limit_w=%f grid_w=%f", plan.Actions[0].PVLimitW, plan.Actions[0].GridW)
	}
	uncurtailed := plan.Actions[0].LoadW + plan.Actions[0].PVW + plan.Actions[0].BatteryW
	if math.Abs(plan.Actions[0].GridW-uncurtailed) > 2 {
		t.Fatalf("Go DP GridW = %f, want uncurtailed %f", plan.Actions[0].GridW, uncurtailed)
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("ValidatePlan rejected DP curtail hint: %v", err)
	}
}

func TestValidatePlanRejectsFuseViolatingIdle(t *testing.T) {
	slots := []Slot{{
		StartMs: 1, LenMin: 60, PriceOre: 100, Confidence: 1,
		LoadW: 0, PVW: -8000,
		Limits: PowerLimits{MaxExportW: 100},
	}}
	p := baseParams(ModeSelfConsumption)
	p.MaxChargeW = 0
	p.MaxDischargeW = 0
	p.InitialSoCPct = 90
	plan := Optimize(slots, p)
	if len(plan.Actions) != 1 {
		t.Fatalf("got %d actions", len(plan.Actions))
	}
	err := ValidatePlan(slots, p, &plan)
	if err == nil {
		t.Fatal("ValidatePlan accepted idle export past MaxExportW")
	}
}

func TestValidatePlanAcceptsActiveZeroPVCap(t *testing.T) {
	slots := []Slot{{
		StartMs: 1, LenMin: 60, PriceOre: 100, SpotOre: -100, Confidence: 1,
		LoadW: 0, PVW: -5000,
	}}
	p := baseParams(ModeArbitrage)
	p.InitialSoCPct = 95
	plan := Plan{
		Mode: p.Mode, HorizonSlots: 1, CapacityWh: p.CapacityWh, InitialSoCPct: 95,
		Actions: []Action{{
			SlotStartMs: 1, SlotLenMin: 60,
			BatteryW: 0, GridW: 0, SoCPct: 95, CostOre: 0,
			PVLimitW: 0, PVCurtailActive: true,
		}},
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("active zero cap: %v", err)
	}

	plan.Actions[0].PVCurtailActive = false
	if err := ValidatePlan(slots, p, &plan); err == nil {
		t.Fatal("zero grid without pv_curtail_active must not replay as uncurtailed PV")
	}
}

func TestValidatePlanReplaysAggregateWhenStorageMapsEmpty(t *testing.T) {
	slots := flatLoadSlots([]float64{100, 200})
	p := baseParams(ModeSelfConsumption)
	p.InitialSoCPct = 80
	p.Storages = []StorageAssetSpec{{
		ID: "home", CapacityWh: p.CapacityWh,
		InitialEnergyWh: p.CapacityWh * p.InitialSoCPct / 100,
		MinEnergyWh:     p.CapacityWh * p.SoCMinPct / 100,
		MaxEnergyWh:     p.CapacityWh * p.SoCMaxPct / 100,
		MaxChargeW:      p.MaxChargeW, MaxDischargeW: p.MaxDischargeW,
		ChargeEfficiency: p.ChargeEfficiency, DischargeEfficiency: p.DischargeEfficiency,
	}}
	plan := Optimize(slots, p)
	if len(plan.Actions[0].StoragePowerW) != 0 {
		t.Fatal("Go DP should not invent per-storage maps")
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("aggregate replay: %v", err)
	}
}
