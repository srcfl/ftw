package mpc

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// A field night, 15 Nov 2026: a 20 A fuse with 10 A of house load on one
// phase left the site 9,250 W of import. The house drew 3,800 W, the car had
// a goal it could not reach by morning, and the plan discharged the battery
// to cover the house. The grid room that freed went to the car. Every slot
// met the conservation rule, yet the battery charged the car. A battery
// that may not cover the car leaves it only the room the grid gives with
// the battery idle.
//
// The field plan came from the worker. Here the price makes the car's
// missed goal (four times the mean price) worth more than the battery's
// stored energy, so the Core DP would take the same room.
const fuseNightImportW = 9250

func fuseNightSlots() []Slot {
	slots := make([]Slot, 8)
	for i := range slots {
		slots[i] = Slot{StartMs: 1_794_700_800_000 + int64(i)*60*60*1000, LenMin: 60, PriceOre: 100, SpotOre: 80,
			LoadW: 3800, Limits: PowerLimits{MaxImportW: fuseNightImportW}}
	}
	return slots
}

func fuseNightParams(mode Mode, noBatteryToEV bool) Params {
	return Params{
		Mode: mode, SoCLevels: 41, ActionLevels: 41,
		CapacityWh: 20000, SoCMin: .1, SoCMax: .95, InitialSoC: .6,
		MaxChargeW: 10000, MaxDischargeW: 10000,
		ChargeEfficiency: .95, DischargeEfficiency: .95, TerminalSoCPrice: 125,
		Loadpoint: &LoadpointSpec{
			ID: "car", CapacityWh: 84000, Levels: 85, SoCMin: 0, SoCMax: 1,
			InitialSoC: .3, PluggedIn: true, TargetSoC: 1, TargetSlotIdx: 7,
			MinChargeW: 4140, MaxChargeW: 11040, ChargeEfficiency: .9,
			AllowedStepsW: []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040},
			NoBatteryToEV: noBatteryToEV,
		},
	}
}

func TestBatteryThatMayNotCoverTheCarLeavesItTheGridsRoomAlone(t *testing.T) {
	for _, mode := range []Mode{ModeArbitrage, ModePassiveArbitrage} {
		t.Run(string(mode), func(t *testing.T) {
			slots := fuseNightSlots()
			plan := Optimize(slots, fuseNightParams(mode, true))
			for i, a := range plan.Actions {
				t.Logf("slot %d: battery %.0f W car %.0f W grid %.0f W", i, a.BatteryW, a.LoadpointW, a.GridW)
				if a.LoadpointW > 0 && a.BatteryW < -50 && slots[i].LoadW+a.LoadpointW > fuseNightImportW+loadpoint.SitePowerEpsW {
					t.Errorf("slot %d: battery %.0f W makes the room for a %.0f W car beside a %.0f W house under %d W",
						i, a.BatteryW, a.LoadpointW, slots[i].LoadW, fuseNightImportW)
				}
			}
		})
	}
}

// With the battery allowed to cover the car, the same night may use it.
func TestBatteryThatMayCoverTheCarStillHelpsItUnderTheLimit(t *testing.T) {
	slots := fuseNightSlots()
	plan := Optimize(slots, fuseNightParams(ModeArbitrage, false))
	helped := false
	for i, a := range plan.Actions {
		if a.LoadpointW > 0 && a.BatteryW < -50 && slots[i].LoadW+a.LoadpointW > fuseNightImportW {
			helped = true
		}
	}
	if !helped {
		t.Fatal("a battery that may cover the car never helped it past the grid's room")
	}
}

func TestValidatePlanRejectsBatteryMakingRoomForTheCar(t *testing.T) {
	slots := []Slot{{StartMs: 1, LenMin: 60, PriceOre: 15, SpotOre: 15, LoadW: 3800,
		Limits: PowerLimits{MaxImportW: fuseNightImportW}}}
	p := fuseNightParams(ModeArbitrage, true)
	p.InitialSoC = .5
	p.Loadpoint.TargetSlotIdx = 0
	// Battery 3,000 W to the house, the car 8,280 W: grid 9,080 W fits the
	// limit, and the house uses all the battery gives. With the battery idle
	// the car could have had only 5,450 W.
	evSoC := .3 + 8280*.9/84000
	plan := Plan{Mode: p.Mode, HorizonSlots: 1, CapacityWh: p.CapacityWh, InitialSoC: p.InitialSoC, Actions: []Action{{
		SlotStartMs: 1, SlotLenMin: 60, LoadW: 3800, PriceOre: 15, SpotOre: 15,
		BatteryW: -3000, GridW: 9080, SoC: .5 - 3000/.95/20000,
		LoadpointW: 8280, LoadpointSoC: evSoC,
	}}}
	plan.Actions[0].CostOre = reserveSlotCost(slots[0], p, plan.Actions[0])
	plan.TotalCostOre = plan.Actions[0].CostOre
	if err := ValidatePlan(slots, p, &plan); err == nil {
		t.Fatal("ValidatePlan accepted a battery making grid room for the car")
	}
	// The same plan with the battery allowed to cover the car stands.
	p.Loadpoint.NoBatteryToEV = false
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("ValidatePlan rejected a battery that may cover the car: %v", err)
	}
}

// A battery that may cover the car still covers the 500 W house so the
// 11 kW car fits an 11,040 W limit.
func TestReservePlanLetsACoveredCarUseTheBatteryRoom(t *testing.T) {
	slots, p := nearTargetReserveFixture()
	for i := range slots {
		slots[i].Limits.MaxImportW = 11040
	}
	p.Loadpoint.NoBatteryToEV = false
	plan := mustCoreReservePlan(t, slots, p)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	if a := plan.Actions[0]; a.LoadpointMaxPowerW["easee"] != 11000 || a.BatteryW >= 0 {
		t.Fatalf("covered car lost the battery's room: pulse %.0f W, battery %.0f W", a.LoadpointMaxPowerW["easee"], a.BatteryW)
	}
}

func TestReservePlanKeepsTheBatteryOutOfTheCarsRoom(t *testing.T) {
	slots := fuseNightSlots()
	plan, err := coreReservePlan(t.Context(), slots, fuseNightParams(ModeArbitrage, true))
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range plan.Actions {
		peak := a.LoadpointMaxPowerW["car"]
		if peak > 0 && a.BatteryW < -50 && slots[i].LoadW+peak > fuseNightImportW+loadpoint.SitePowerEpsW {
			t.Errorf("slot %d: battery %.0f W makes the room for a %.0f W car pulse", i, a.BatteryW, peak)
		}
	}
}
