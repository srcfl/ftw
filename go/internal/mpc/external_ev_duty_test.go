package mpc

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestExternalEVDutyRoundTripAndLegacyRejection(t *testing.T) {
	slots, p := nearTargetReserveFixture()
	want := coreReservePlan(context.Background(), slots, p)
	wire := externalResponse{Plan: externalPlan{TotalCostOre: want.TotalCostOre, FlexShortfallWh: want.LoadpointShortfallWh}}
	for _, a := range want.Actions {
		wire.Plan.Actions = append(wire.Plan.Actions, externalAction{SlotStartMs: a.SlotStartMs, ExecutionStartMs: a.ExecutionStartMs, SlotLenMin: a.SlotLenMin, BatteryW: a.BatteryW, GridW: a.GridW, SoCPct: a.SoC * 100, CostOre: a.CostOre, FlexPowerW: a.LoadpointPowerW, FlexMaxPowerW: a.LoadpointMaxPowerW, FlexEnergyWh: map[string]float64{"easee": a.LoadpointSoC * 75000}})
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var decoded externalResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	got := decoded.toPlan(slots, p)
	if err := ValidatePlan(slots, p, &got); err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.Actions[0].LoadpointW-want.Actions[0].LoadpointW) > 1e-6 || got.Actions[0].LoadpointMaxPowerW["easee"] != 11000 {
		t.Fatal("duty budget or on-power lost")
	}
	for i := range decoded.Plan.Actions {
		decoded.Plan.Actions[i].FlexMaxPowerW = nil
	}
	legacy := decoded.toPlan(slots, p)
	if err := ValidatePlan(slots, p, &legacy); err == nil {
		t.Fatal("fractional mean accepted without explicit duty contract")
	}
}

func TestNativeEVDutyNearTargetPassesCoreReplay(t *testing.T) {
	o := nativeWorker(t, 2*time.Second)
	defer o.Close()
	slots, p := nearTargetReserveFixture()
	slots = slots[:12]
	plan, err := o.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	if math.Abs(plan.Actions[11].LoadpointSoC-.8) > 1e-5 {
		t.Fatalf("target missed: %.9f", plan.Actions[11].LoadpointSoC)
	}
	partial := false
	for _, a := range plan.Actions {
		if a.LoadpointW > 0 && a.LoadpointW < a.LoadpointMaxPowerW["easee"] {
			partial = true
		}
	}
	if !partial {
		t.Fatal("worker did not return a partial final interval")
	}
}
