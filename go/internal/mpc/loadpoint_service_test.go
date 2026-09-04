package mpc

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
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
		SoCMin:              0.1,
		SoCMax:              0.95,
		InitialSoC:          0.5,
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
			InitialSoC:       0.2,
			PluggedIn:        true,
			TargetSoC:        0.4,
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

	svc := &Service{Zone: "SE3", Defaults: Params{Mode: ModeCheapCharge}}
	svc.InstallPlan(plan, p, "garage")
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
	if _, ok := d.LoadpointSoCTarget["garage"]; !ok {
		t.Errorf("LoadpointSoCTarget missing garage entry")
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
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
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
			SoCMin:              0.1,
			SoCMax:              0.95,
			InitialSoC:          0.9,
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
				InitialSoC:       0.2,
				PluggedIn:        true,
				TargetSoC:        0.3,
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

	mkParams := func(surplusOnly bool) Params {
		return Params{
			Mode:                ModeArbitrage,
			SoCLevels:           11,
			CapacityWh:          20000,
			SoCMin:              0.1,
			SoCMax:              0.95,
			InitialSoC:          0.9,
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
				InitialSoC:       0.2,
				PluggedIn:        true,
				TargetSoC:        0.7,
				TargetSlotIdx:    1,
				MaxChargeW:       5000,
				AllowedStepsW:    []float64{0, 5000},
				ChargeEfficiency: 1.0,
				SurplusOnly:      surplusOnly,
				NoBatteryToEV:    false, // mirrors BatteryCoversEV=true.
			},
		}
	}

	// Prove the scenario exercises the prohibited path: without the
	// surplus-only contract, the cost-optimal plan feeds the EV from the
	// home battery rather than importing at the high retail price.
	unprotected := Optimize(slots, mkParams(false))
	var batteryFedEV bool
	for _, a := range unprotected.Actions {
		if a.LoadpointW > 100 && a.BatteryW < -100 {
			batteryFedEV = true
			break
		}
	}
	if !batteryFedEV {
		t.Fatalf("unprotected baseline did not feed EV from battery: %+v", unprotected.Actions)
	}

	plan := Optimize(slots, mkParams(true))
	for i, a := range plan.Actions {
		if loadpoint.BatteryDischargeFeedsEV(a.BatteryW, a.LoadpointW, slots[i].LoadW, slots[i].PVW) {
			t.Errorf("slot %d: surplus_only used battery as EV surplus — battW=%.0f loadpointW=%.0f gridW=%.0f",
				i, a.BatteryW, a.LoadpointW, a.GridW)
		}
	}
}

// TestArbitrageGridChargesWhileSurplusOnlyEVIsConnected is the active-
// arbitrage + surplus-only EV contract: the car may not import, but the
// home battery must still buy from the grid in a cheap slot. The old
// feasibility rule forbade (battW > 0 AND gridW > 50) whenever the car
// was plugged in, which silenced grid-charge for the whole connection.
func TestArbitrageGridChargesWhileSurplusOnlyEVIsConnected(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 30, SpotOre: 10, LoadW: 500, PVW: 0, Confidence: 1},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 300, SpotOre: 250, LoadW: 500, PVW: 0, Confidence: 1},
	}
	plan := Optimize(slots, Params{
		Mode:                ModeArbitrage,
		SoCLevels:           11,
		CapacityWh:          20000,
		SoCMin:              0.1,
		SoCMax:              0.95,
		InitialSoC:          0.2,
		ActionLevels:        11,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    150,
		Loadpoint: &LoadpointSpec{
			ID:               "garage",
			CapacityWh:       60000,
			Levels:           11,
			InitialSoC:       0.8,
			PluggedIn:        true,
			TargetSoC:        0.8, // already at target — EV should stay off
			TargetSlotIdx:    1,
			MaxChargeW:       3000,
			AllowedStepsW:    []float64{0, 3000},
			ChargeEfficiency: 1.0,
			SurplusOnly:      true,
			NoBatteryToEV:    true,
		},
	})
	if len(plan.Actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(plan.Actions))
	}
	if plan.Actions[0].LoadpointW > 50 {
		t.Errorf("surplus-only EV imported without PV: evW=%.0f gridW=%.0f",
			plan.Actions[0].LoadpointW, plan.Actions[0].GridW)
	}
	if plan.Actions[0].BatteryW < 500 {
		t.Errorf("cheap slot should grid-charge the home battery, got battW=%.0f gridW=%.0f evW=%.0f",
			plan.Actions[0].BatteryW, plan.Actions[0].GridW, plan.Actions[0].LoadpointW)
	}
	if plan.Actions[0].GridW < 500 {
		t.Errorf("cheap slot should import for the battery, got gridW=%.0f", plan.Actions[0].GridW)
	}
}

func TestPassiveArbitrageGridChargesWhileSurplusOnlyEVIsConnected(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 30, SpotOre: 10, LoadW: 500, Confidence: 1},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 300, SpotOre: 250, LoadW: 5000, Confidence: 1},
	}
	plan := Optimize(slots, Params{
		Mode:                ModePassiveArbitrage,
		SoCLevels:           18,
		CapacityWh:          20000,
		SoCMin:              0.1,
		SoCMax:              0.95,
		InitialSoC:          0.1,
		ActionLevels:        11,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		Loadpoint: &LoadpointSpec{
			ID:               "garage",
			CapacityWh:       60000,
			Levels:           11,
			InitialSoC:       0.8,
			PluggedIn:        true,
			TargetSoC:        0.8,
			TargetSlotIdx:    1,
			MaxChargeW:       3000,
			AllowedStepsW:    []float64{0, 3000},
			ChargeEfficiency: 1,
			SurplusOnly:      true,
			NoBatteryToEV:    true,
		},
	})
	if len(plan.Actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(plan.Actions))
	}
	if got := plan.Actions[0].LoadpointW; got > 50 {
		t.Errorf("surplus-only EV imported during cheap slot: %+v", plan.Actions[0])
	}
	if got := plan.Actions[0].BatteryW; got < 500 {
		t.Errorf("defer-grid mode should charge home battery at cheap night price: %+v", plan.Actions[0])
	}
	if got := plan.Actions[0].GridW; got < 500 {
		t.Errorf("cheap slot should import for home battery: %+v", plan.Actions[0])
	}
}

func TestSurplusOnlyEVCannotImportEvenWithDeadline(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 40, SpotOre: 10, LoadW: 500, PVW: 0, Confidence: 1},
	}
	plan := Optimize(slots, Params{
		Mode:                ModeArbitrage,
		SoCLevels:           11,
		CapacityWh:          10000,
		SoCMin:              0.1,
		SoCMax:              0.95,
		InitialSoC:          0.5,
		ActionLevels:        11,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    40,
		Loadpoint: &LoadpointSpec{
			ID:               "garage",
			CapacityWh:       40000,
			Levels:           11,
			InitialSoC:       0.2,
			PluggedIn:        true,
			TargetSoC:        0.8,
			TargetSlotIdx:    0,
			MaxChargeW:       7000,
			AllowedStepsW:    []float64{0, 7000},
			ChargeEfficiency: 1.0,
			SurplusOnly:      true,
		},
	})
	if len(plan.Actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(plan.Actions))
	}
	a := plan.Actions[0]
	if surplusOnlyExceedsHousePV(a.LoadpointW, slots[0].LoadW, slots[0].PVW) {
		t.Errorf("surplus-only EV exceeded leftover PV: evW=%.0f leftover=%.0f gridW=%.0f",
			a.LoadpointW, loadpoint.PVLeftoverAfterHouseW(slots[0].LoadW, slots[0].PVW), a.GridW)
	}
}

func TestArbitrageChargesSurplusOnlyEVFromPVWhileBatteryGridCharges(t *testing.T) {
	// Cheap sun + empty battery + expensive evening. Surplus-only may
	// take leftover PV after the house; the battery may still buy from
	// the grid in the same slot. The old feasibility rule rejected any
	// (evW>0 AND gridW>50) pair and forced "car sits / Pixii never buys".
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 20, SpotOre: 10, LoadW: 500, PVW: -6500, Confidence: 1},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 300, SpotOre: 240, LoadW: 2500, PVW: 0, Confidence: 1},
	}
	plan := Optimize(slots, Params{
		Mode:                ModeArbitrage,
		SoCLevels:           11,
		CapacityWh:          20000,
		SoCMin:              0.10,
		SoCMax:              0.95,
		InitialSoC:          0.20,
		ActionLevels:        11,
		MaxChargeW:          10000,
		MaxDischargeW:       10000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    150,
		Loadpoint: &LoadpointSpec{
			ID:               "garage",
			CapacityWh:       40000,
			Levels:           11,
			InitialSoC:       0.20,
			PluggedIn:        true,
			TargetSoC:        0.40,
			TargetSlotIdx:    1,
			MaxChargeW:       4140,
			AllowedStepsW:    []float64{0, 4140},
			ChargeEfficiency: 1.0,
			SurplusOnly:      true,
			NoBatteryToEV:    true,
		},
	})
	if len(plan.Actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(plan.Actions))
	}
	a := plan.Actions[0]
	if a.LoadpointW < 1000 {
		t.Errorf("cheap PV slot should charge the surplus-only EV from leftover PV, got %+v", a)
	}
	if a.BatteryW < 500 {
		t.Errorf("cheap slot should still grid-charge the home battery, got %+v", a)
	}
	if a.GridW < 100 {
		t.Errorf("battery charge past leftover PV must import: %+v", a)
	}
	if surplusOnlyExceedsHousePV(a.LoadpointW, slots[0].LoadW, slots[0].PVW) {
		t.Errorf("EV %.0f W exceeded leftover %.0f W", a.LoadpointW, loadpoint.PVLeftoverAfterHouseW(slots[0].LoadW, slots[0].PVW))
	}
}

func TestInstallPlanPublishesLoadpointEnergyToSlotDirectiveAt(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	plan := Plan{
		GeneratedAtMs: now.UnixMilli(),
		Mode:          ModeArbitrage,
		Actions: []Action{{
			SlotStartMs: now.UnixMilli(),
			SlotLenMin:  15,
			BatteryW:    10000,
			LoadpointW:  4140,
			LoadW:       500,
			PVW:         -8000,
			GridW:       loadpoint.GridW(500, -8000, 10000, 4140),
		}},
	}
	svc := &Service{}
	svc.InstallPlan(plan, Params{Mode: ModeArbitrage}, "garage")
	d, ok := svc.SlotDirectiveAt(now.Add(time.Second))
	if !ok {
		t.Fatal("fresh InstallPlan must be visible to SlotDirectiveAt")
	}
	wantEV := 4140.0 * 15 / 60
	if d.LoadpointEnergyWh["garage"] != wantEV {
		t.Fatalf("EV budget = %+v, want garage=%.0f Wh", d.LoadpointEnergyWh, wantEV)
	}
	if d.Strategy != ModeArbitrage {
		t.Fatalf("Strategy = %q, want %q", d.Strategy, ModeArbitrage)
	}
	lp := d.LoadpointDirective()
	if lp.LoadpointEnergyWh["garage"] != wantEV {
		t.Fatalf("LoadpointDirective dropped EV budget: %+v", lp.LoadpointEnergyWh)
	}
}
