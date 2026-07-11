package mpc

import (
	"testing"
	"time"
)

// TestSlotDirectiveCarriesLoadpointEnergyWh asserts that when the DP
// decided an EV should charge in a slot, SlotDirectiveAt surfaces the
// planned Wh under the correct loadpoint ID. This is the contract the
// dispatch layer consumes to drive the charger.
func TestSlotDirectiveCarriesLoadpointEnergyWh(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Truncate(15 * time.Minute)
	// 4 hourly slots with cheap nighttime + expensive daytime. A
	// target of 40 % on a 20-% start forces the DP to schedule EV
	// charging across multiple slots — we don't assert WHICH ones;
	// only that at least one gets a loadpoint entry.
	slots := make([]Slot, 4)
	for i := range slots {
		slots[i] = Slot{
			StartMs:    start.Add(time.Duration(i) * time.Hour).UnixMilli(),
			LenMin:     60,
			PriceOre:   40,
			SpotOre:    20,
			LoadW:      400,
			Confidence: 1.0,
		}
	}
	p := Params{
		Mode:                ModeCheapCharge,
		SoCLevels:           11,
		CapacityWh:          5000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        5,
		MaxChargeW:          2000,
		MaxDischargeW:       2000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    40,
		Loadpoint: &LoadpointSpec{
			ID:               "garage",
			CapacityWh:       60000,
			Levels:           11,
			InitialSoCPct:    20,
			PluggedIn:        true,
			TargetSoCPct:     40,
			TargetSlotIdx:    3,
			MaxChargeW:       11000,
			AllowedStepsW:    []float64{0, 11000},
			ChargeEfficiency: 0.9,
		},
	}
	plan := Optimize(slots, p)

	// Find a slot where DP scheduled charging — assert the Service
	// routes its Wh under the loadpoint ID.
	var chargedSlotIdx int = -1
	for i, a := range plan.Actions {
		if a.LoadpointW > 0 {
			chargedSlotIdx = i
			break
		}
	}
	if chargedSlotIdx < 0 {
		t.Fatalf("DP never scheduled EV charging; actions: %+v", plan.Actions)
	}

	svc := &Service{
		Zone:            "SE3",
		Defaults:        Params{Mode: ModeCheapCharge},
		last:            &plan,
		lastLoadpointID: "garage",
	}
	// Query inside the charged slot.
	queryAt := time.UnixMilli(plan.Actions[chargedSlotIdx].SlotStartMs).Add(1 * time.Minute)
	d, ok := svc.SlotDirectiveAt(queryAt)
	if !ok {
		t.Fatal("SlotDirectiveAt returned ok=false")
	}
	if d.LoadpointEnergyWh == nil {
		t.Fatalf("LoadpointEnergyWh nil on slot %d where DP set LoadpointW=%f",
			chargedSlotIdx, plan.Actions[chargedSlotIdx].LoadpointW)
	}
	wh, exists := d.LoadpointEnergyWh["garage"]
	if !exists {
		t.Fatalf("garage missing: %+v", d.LoadpointEnergyWh)
	}
	if wh <= 0 {
		t.Errorf("LoadpointEnergyWh[garage] = %.1f, want > 0", wh)
	}
	if _, ok := d.LoadpointSoCTargetPct["garage"]; !ok {
		t.Errorf("LoadpointSoCTargetPct missing garage entry")
	}
}

// TestSlotDirectiveEmptyWhenNoLoadpoint asserts the legacy path:
// when no loadpoint was active, SlotDirective's LP fields stay nil
// so older dispatch code paths see no change.
func TestSlotDirectiveEmptyWhenNoLoadpoint(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Truncate(15 * time.Minute)
	slots := []Slot{
		{StartMs: start.UnixMilli(), LenMin: 15, PriceOre: 50,
			LoadW: 500, Confidence: 1.0},
	}
	plan := Optimize(slots, Params{
		Mode: ModeSelfConsumption, SoCLevels: 11, CapacityWh: 10000,
		SoCMinPct: 10, SoCMaxPct: 95, InitialSoCPct: 50,
		ActionLevels: 5, MaxChargeW: 2000, MaxDischargeW: 2000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
	})
	svc := &Service{last: &plan, lastLoadpointID: ""}
	d, ok := svc.SlotDirectiveAt(start.Add(1 * time.Minute))
	if !ok {
		t.Fatal("SlotDirectiveAt ok=false")
	}
	if d.LoadpointEnergyWh != nil {
		t.Errorf("expected nil LoadpointEnergyWh, got %+v", d.LoadpointEnergyWh)
	}
}

// TestNoBatteryToEVForbidsBatteryFeedingEV asserts the DP refuses to
// schedule battery discharge that would, by energy conservation, flow
// into the EV when LoadpointSpec.NoBatteryToEV is true. The scenario
// is constructed so the cost-optimal allocation WITHOUT the constraint
// is "battery discharges to cover EV" (expensive grid + free battery
// energy + EV demand). With the constraint, the DP must keep the
// battery at most at house-residual-after-PV.
func TestNoBatteryToEVForbidsBatteryFeedingEV(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Truncate(15 * time.Minute)
	// Two slots. First slot: very expensive grid, low PV (1 kW), modest
	// house (1 kW), high battery SoC. Without the constraint the DP
	// would happily discharge battery 5+ kW to cover house + EV; with
	// it, battery must stay ≤ house_residual = max(0, 1000 - 1000) = 0.
	slots := []Slot{
		{
			StartMs:    start.UnixMilli(),
			LenMin:     60,
			PriceOre:   500,
			SpotOre:    500,
			LoadW:      1000,
			PVW:        -1000,
			Confidence: 1.0,
		},
		{
			StartMs:    start.Add(time.Hour).UnixMilli(),
			LenMin:     60,
			PriceOre:   500,
			SpotOre:    500,
			LoadW:      1000,
			PVW:        -1000,
			Confidence: 1.0,
		},
	}
	mkParams := func(noBatToEV bool) Params {
		return Params{
			Mode:                ModeArbitrage,
			SoCLevels:           11,
			CapacityWh:          20000,
			SoCMinPct:           10,
			SoCMaxPct:           95,
			InitialSoCPct:       90,
			ActionLevels:        11,
			MaxChargeW:          5000,
			MaxDischargeW:       5000,
			ChargeEfficiency:    0.95,
			DischargeEfficiency: 0.95,
			TerminalSoCPrice:    400,
			Loadpoint: &LoadpointSpec{
				ID:               "garage",
				CapacityWh:       60000,
				Levels:           11,
				InitialSoCPct:    20,
				PluggedIn:        true,
				TargetSoCPct:     30,
				TargetSlotIdx:    1,
				MaxChargeW:       11000,
				AllowedStepsW:    []float64{0, 11000},
				ChargeEfficiency: 0.9,
				NoBatteryToEV:    noBatToEV,
			},
		}
	}

	// Baseline (constraint off): DP is allowed to over-discharge.
	planOff := Optimize(slots, mkParams(false))
	// Find a slot where both EV charges AND battery discharges past
	// house-residual. With house=1000, PV=-1000, residual is 0, so
	// any battW < -50 simultaneous with evW > 0 is "feeding EV".
	violationOff := false
	for _, a := range planOff.Actions {
		if a.LoadpointW > 100 && a.BatteryW < -50 {
			violationOff = true
			break
		}
	}
	if !violationOff {
		t.Skip("baseline never picked battery-to-EV — scenario didn't exercise the rule (price model / SoC grid changed?)")
	}

	// Constraint on: same scenario, DP must NOT pick that allocation.
	planOn := Optimize(slots, mkParams(true))
	for i, a := range planOn.Actions {
		if a.LoadpointW > 100 && a.BatteryW < -50 {
			t.Errorf("slot %d: NoBatteryToEV violated — battW=%.0f loadpointW=%.0f (PV=%.0f load=%.0f)",
				i, a.BatteryW, a.LoadpointW, slots[i].PVW, slots[i].LoadW)
		}
	}
}

func TestSurplusOnlyForbidsBatteryFeedingEVEvenWhenCoverEVEnabled(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	start := now.Truncate(15 * time.Minute)

	slots := []Slot{
		{
			StartMs:    start.UnixMilli(),
			LenMin:     60,
			PriceOre:   500,
			SpotOre:    500,
			LoadW:      1000,
			PVW:        -1000,
			Confidence: 1.0,
		},
		{
			StartMs:    start.Add(time.Hour).UnixMilli(),
			LenMin:     60,
			PriceOre:   500,
			SpotOre:    500,
			LoadW:      1000,
			PVW:        -1000,
			Confidence: 1.0,
		},
	}

	plan := Optimize(slots, Params{
		Mode:                ModeArbitrage,
		SoCLevels:           11,
		CapacityWh:          20000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       90,
		ActionLevels:        11,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    400,
		Loadpoint: &LoadpointSpec{
			ID:               "garage",
			CapacityWh:       10000,
			Levels:           11,
			InitialSoCPct:    20,
			PluggedIn:        true,
			TargetSoCPct:     70,
			TargetSlotIdx:    1,
			MaxChargeW:       5000,
			AllowedStepsW:    []float64{0, 5000},
			ChargeEfficiency: 1.0,
			SurplusOnly:      true,
			NoBatteryToEV:    false, // mirrors BatteryCoversEV=true.
		},
	})

	for i, a := range plan.Actions {
		houseResidualW := slots[i].LoadW + slots[i].PVW
		if houseResidualW < 0 {
			houseResidualW = 0
		}
		if a.LoadpointW > 100 && a.BatteryW < -(houseResidualW+50) {
			t.Errorf("slot %d: surplus_only used battery as EV surplus — battW=%.0f loadpointW=%.0f gridW=%.0f",
				i, a.BatteryW, a.LoadpointW, a.GridW)
		}
	}
}
