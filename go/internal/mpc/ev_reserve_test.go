package mpc

import (
	"context"
	"math"
	"testing"
	"time"
)

func nearTargetReserveFixture() ([]Slot, Params) {
	p := Params{Mode: ModeArbitrage, CapacityWh: 9600, InitialSoC: .5, SoCMin: .1, SoCMax: .95, SoCLevels: 21, ActionLevels: 21, MaxChargeW: 4800, MaxDischargeW: 4800, ChargeEfficiency: .95, DischargeEfficiency: .95}
	p.Loadpoint = &LoadpointSpec{ID: "easee", CapacityWh: 75000, InitialSoC: .7721774800618489, SoCMin: 0, SoCMax: .8, Levels: 11, PluggedIn: true, TargetSoC: .8, TargetSlotIdx: 11, MaxChargeW: 11000, ChargeEfficiency: .9, NoBatteryToEV: true}
	slots := make([]Slot, 193)
	start := time.Now().UTC().Truncate(15 * time.Minute)
	for i := range slots {
		slots[i] = Slot{StartMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), LenMin: 15, PriceOre: 100, SpotOre: 10, LoadW: 500, Limits: PowerLimits{MaxImportW: 11040, MaxExportW: 11040}, Confidence: 1}
	}
	return slots, p
}

func TestEVReserveReachesExactNearTargetWithoutWholeSlotOvercharge(t *testing.T) {
	for _, remainingFirstMinutes := range []int{15, 2} {
		slots, p := nearTargetReserveFixture()
		if remainingFirstMinutes < 15 {
			slots[0].ExecutionStartMs = slots[0].StartMs + int64(15-remainingFirstMinutes)*60000
		}
		before := time.Now()
		plan := coreReservePlan(context.Background(), slots, p)
		if err := ValidatePlan(slots, p, &plan); err != nil {
			t.Fatal(err)
		}
		t.Logf("193 slots, remaining first slot %dm: %s", remainingFirstMinutes, time.Since(before))
		if plan.InitialSoC != p.InitialSoC {
			t.Fatalf("initial SoC changed units: %v", plan.InitialSoC)
		}
		final := plan.Actions[len(plan.Actions)-1].LoadpointSoC
		if math.Abs(final-.8) > 1e-9 || plan.LoadpointShortfallWh["easee"] > 1e-6 {
			t.Fatalf("target missed: %.12f, %+v", final, plan.LoadpointShortfallWh)
		}
		for _, a := range plan.Actions {
			if a.LoadpointSoC > .8+1e-9 {
				t.Fatal("overcharged")
			}
		}
		energy := 0.0
		for _, a := range plan.Actions {
			energy += a.LoadpointW * a.DurationHours()
		}
		want := (.8 - p.Loadpoint.InitialSoC) * 75000 / .9
		if math.Abs(energy-want) > 1e-6 {
			t.Fatalf("AC energy %.6f, want %.6f", energy, want)
		}
	}
}

func TestEVReserveReportsRealShortfallAndKeepsLimits(t *testing.T) {
	slots, p := nearTargetReserveFixture()
	p.Loadpoint.InitialSoC = .2
	p.Loadpoint.TargetSlotIdx = 0
	plan := coreReservePlan(context.Background(), slots, p)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Actions[0].LoadpointW != 11000 || plan.LoadpointShortfallWh["easee"] <= 0 {
		t.Fatalf("no best effort or missing shortfall: %+v", plan.Solver)
	}
	for _, a := range plan.Actions[1:] {
		if a.LoadpointW != 0 {
			t.Fatal("charged after departure")
		}
	}
}

func TestEVReservePreservesSolarOnlyAndNoBatteryToEV(t *testing.T) {
	slots, p := nearTargetReserveFixture()
	p.Loadpoint.SurplusOnly = true
	plan := coreReservePlan(context.Background(), slots, p)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Actions[0].LoadpointW != 0 || plan.LoadpointShortfallWh["easee"] <= 0 {
		t.Fatal("solar-only car charged from the grid")
	}
	slots[0].PVW = -5000
	p.Loadpoint.AllowedStepsW = []float64{0, 4140, 11000}
	plan = coreReservePlan(context.Background(), slots, p)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Actions[0].LoadpointW != 4140 {
		t.Fatalf("usable solar rejected: %.1f", plan.Actions[0].LoadpointW)
	}
}

func TestValidateEVPulseRejectsUnsafeOnPowerDespiteLegalAverage(t *testing.T) {
	for _, mutation := range []string{"fuse", "step", "unknown identity", "solar"} {
		t.Run(mutation, func(t *testing.T) {
			slots, p := nearTargetReserveFixture()
			plan := coreReservePlan(context.Background(), slots, p)
			switch mutation {
			case "fuse":
				slots[0].Limits.MaxImportW = 10000
			case "step":
				plan.Actions[0].LoadpointMaxPowerW["easee"] = 10500
			case "unknown identity":
				plan.Actions[0].LoadpointMaxPowerW = map[string]float64{"wrong": 11000}
			case "solar":
				p.Loadpoint.SurplusOnly = true
			}
			if err := ValidatePlan(slots, p, &plan); err == nil {
				t.Fatal("accepted unsafe pulse")
			}
		})
	}
}

func TestEVReserveUsesMinimumStepWhenHouseBlocksMaximum(t *testing.T) {
	slots, p := nearTargetReserveFixture()
	p.Loadpoint.MinChargeW = 4140
	p.InitialSoC = p.SoCMin
	plan := coreReservePlan(context.Background(), slots, p)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Actions[0].LoadpointMaxPowerW["easee"] != 4140 || plan.LoadpointShortfallWh["easee"] > 1 {
		t.Fatal("legal minimum charging step lost below the fuse")
	}
}
