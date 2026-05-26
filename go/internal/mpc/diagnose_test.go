package mpc

import (
	"testing"
	"time"
)

// TestDiagnoseNilBeforeReplan asserts we return nil (not a garbage
// struct or panic) when Diagnose is called before any replan has
// completed. The UI handles nil as "no plan yet".
func TestDiagnoseNilBeforeReplan(t *testing.T) {
	s := &Service{Zone: "SE3"}
	if d := s.Diagnose(); d != nil {
		t.Errorf("Diagnose before first replan must be nil, got %+v", d)
	}
}

// TestDiagnoseJoinsSlotsAndActions is the core contract: the per-slot
// output row must carry BOTH the input context the DP saw (price, PV,
// load, confidence) and the decision it made (battery, grid, SoC,
// reason). Without the join, operators can't audit decisions.
func TestDiagnoseJoinsSlotsAndActions(t *testing.T) {
	start := time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC).UnixMilli()
	// Two 15-min slots, both have positive price so the DP will opt to
	// idle (self-consumption mode). Exact decision doesn't matter —
	// we're testing the join shape.
	slots := []Slot{
		{StartMs: start, LenMin: 15, PriceOre: 100, SpotOre: 50,
			PVW: -200, LoadW: 400, Confidence: 1.0},
		{StartMs: start + 15*60*1000, LenMin: 15, PriceOre: 150,
			SpotOre: 80, PVW: -100, LoadW: 500, Confidence: 0.6},
	}
	p := Params{
		Mode:                ModeSelfConsumption,
		SoCLevels:           11,
		CapacityWh:          10000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        7,
		MaxChargeW:          5000,
		MaxDischargeW:       5000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    100,
	}
	plan := Optimize(slots, p)

	svc := &Service{
		Zone:         "SE3",
		last:         &plan,
		lastSlots:    slots,
		lastParams:   p,
		lastReplanAt: time.UnixMilli(plan.GeneratedAtMs),
		lastReason:   "unit-test",
	}
	d := svc.Diagnose()
	if d == nil {
		t.Fatal("Diagnose returned nil after a successful optimize")
	}
	if d.Zone != "SE3" {
		t.Errorf("Zone: got %q want SE3", d.Zone)
	}
	if d.Params.Mode != ModeSelfConsumption {
		t.Errorf("Params.Mode: got %q want self_consumption", d.Params.Mode)
	}
	if d.Params.InitialSoCPct != 50 {
		t.Errorf("Params.InitialSoCPct: got %.2f want 50", d.Params.InitialSoCPct)
	}
	if d.LastReason != "unit-test" {
		t.Errorf("LastReason: got %q want unit-test", d.LastReason)
	}
	if got := len(d.Slots); got != len(slots) {
		t.Fatalf("Slots length: got %d want %d", got, len(slots))
	}
	// Verify row 0 joined correctly: inputs match slots[0], outputs
	// match plan.Actions[0].
	row := d.Slots[0]
	if row.PriceOre != 100 {
		t.Errorf("row0 PriceOre: got %.1f want 100", row.PriceOre)
	}
	if row.SpotOre != 50 {
		t.Errorf("row0 SpotOre: got %.1f want 50", row.SpotOre)
	}
	if row.Confidence != 1.0 {
		t.Errorf("row0 Confidence: got %.2f want 1.0", row.Confidence)
	}
	if row.PVW != -200 {
		t.Errorf("row0 PVW: got %.1f want -200", row.PVW)
	}
	if row.LoadW != 400 {
		t.Errorf("row0 LoadW: got %.1f want 400", row.LoadW)
	}
	// Outputs come from the plan's action — we don't assert exact
	// values (that's what the mpc_test suite covers), just that they
	// were populated.
	if row.Reason == "" {
		t.Error("row0 Reason should be populated by the DP")
	}
	// Row 1 should carry the forecast confidence.
	if d.Slots[1].Confidence != 0.6 {
		t.Errorf("row1 Confidence: got %.2f want 0.6", d.Slots[1].Confidence)
	}
	if d.Slots[1].SlotStartMs != start+15*60*1000 {
		t.Errorf("row1 SlotStartMs: got %d want %d",
			d.Slots[1].SlotStartMs, start+15*60*1000)
	}
}

// TestDiagnoseHandlesLengthMismatch guards against a panic if slots
// and actions ever get out of sync (shouldn't happen in practice —
// Optimize returns len(actions) == len(slots) — but we round-trip
// into lastSlots in service code paths that could diverge).
func TestDiagnoseHandlesLengthMismatch(t *testing.T) {
	slots := []Slot{
		{StartMs: 1000, LenMin: 15, PriceOre: 100, Confidence: 1.0},
		{StartMs: 2000, LenMin: 15, PriceOre: 110, Confidence: 1.0},
	}
	plan := Plan{
		GeneratedAtMs: 123,
		Actions:       []Action{{SlotStartMs: 1000, SlotLenMin: 15}},
	}
	svc := &Service{
		Zone:      "SE3",
		last:      &plan,
		lastSlots: slots,
	}
	d := svc.Diagnose()
	if d == nil {
		t.Fatal("Diagnose should not be nil on mismatch — should truncate")
	}
	if len(d.Slots) != 1 {
		t.Errorf("should truncate to shorter side; got %d rows", len(d.Slots))
	}
}

func TestRestoreDiagnosticRehydratesActivePlan(t *testing.T) {
	now := time.Now()
	start := now.Add(-5 * time.Minute).Truncate(time.Minute)
	d := &Diagnostic{
		ComputedAtMs:   now.Add(-1 * time.Minute).UnixMilli(),
		Zone:           "SE4",
		Horizon:        2,
		TotalCostOre:   -12.3,
		LastReplanAtMs: now.Add(-1 * time.Minute).UnixMilli(),
		LastReason:     "scheduled",
		Params: DiagnosticParams{
			Mode:                ModeSelfConsumption,
			InitialSoCPct:       42,
			SoCMinPct:           10,
			SoCMaxPct:           90,
			SoCLevels:           41,
			ActionLevels:        81,
			MaxChargeW:          5000,
			MaxDischargeW:       5000,
			ChargeEfficiency:    0.95,
			DischargeEfficiency: 0.95,
			CapacityWh:          16000,
			TerminalSoCPrice:    25,
		},
		Slots: []DiagnosticSlot{
			{
				Idx:         0,
				SlotStartMs: start.UnixMilli(),
				SlotEndMs:   start.Add(15 * time.Minute).UnixMilli(),
				LenMin:      15,
				PriceOre:    120,
				SpotOre:     80,
				Confidence:  1,
				PVW:         -4500,
				LoadW:       900,
				BatteryW:    0,
				GridW:       -3600,
				SoCPct:      42,
				CostOre:     -72,
				Reason:      "export surplus",
				EMSMode:     "self_consumption",
				PVLimitW:    4100,
			},
			{
				Idx:         1,
				SlotStartMs: start.Add(15 * time.Minute).UnixMilli(),
				SlotEndMs:   start.Add(30 * time.Minute).UnixMilli(),
				LenMin:      15,
				PriceOre:    -10,
				SpotOre:     -20,
				Confidence:  1,
				PVW:         -5000,
				LoadW:       900,
				BatteryW:    1200,
				GridW:       -2900,
				SoCPct:      44,
				CostOre:     9,
				Reason:      "avoid negative export",
				EMSMode:     "self_consumption",
			},
		},
	}
	svc := &Service{
		Zone:     "SE4",
		Defaults: Params{Mode: ModeSelfConsumption},
	}
	if ok := svc.RestoreDiagnostic(d, now, "restored_diagnostic"); !ok {
		t.Fatal("RestoreDiagnostic returned false")
	}
	latest := svc.Latest()
	if latest == nil {
		t.Fatal("Latest returned nil after restore")
	}
	if latest.GeneratedAtMs != d.ComputedAtMs {
		t.Fatalf("GeneratedAtMs = %d, want %d", latest.GeneratedAtMs, d.ComputedAtMs)
	}
	dir, ok := svc.SlotDirectiveAt(now)
	if !ok {
		t.Fatal("SlotDirectiveAt returned ok=false after restore")
	}
	if dir.BatteryEnergyWh != 0 {
		t.Fatalf("BatteryEnergyWh = %v, want 0", dir.BatteryEnergyWh)
	}
	if dir.GridW != -3600 {
		t.Fatalf("GridW = %v, want -3600", dir.GridW)
	}
	if dir.PVLimitW != 4100 {
		t.Fatalf("PVLimitW = %v, want 4100", dir.PVLimitW)
	}
	at, reason := svc.LastReplanInfo()
	if reason != "restored_diagnostic" {
		t.Fatalf("reason = %q, want restored_diagnostic", reason)
	}
	if at.UnixMilli() != d.LastReplanAtMs {
		t.Fatalf("lastReplanAt = %d, want %d", at.UnixMilli(), d.LastReplanAtMs)
	}
	diag := svc.Diagnose()
	if diag == nil || len(diag.Slots) != 2 {
		t.Fatalf("Diagnose after restore = %+v, want 2 slots", diag)
	}
}

// 2026-05-25 live regression: deploy of v0.82 added SoCSafetyFloorPct
// to Params but persisted snapshots from v0.81 had no field for it.
// RestoreDiagnostic re-hydrated lastParams from the old snapshot
// (SoCSafetyFloorPct=0), so the safety-floor gate sat inactive until
// the next scheduled replan (15 min later). Fix: when restoring,
// merge fields that are zero-in-snapshot but non-zero-in-Defaults
// with the current Defaults value.
func TestRestoreDiagnosticMergesNewerDefaultsForMissingFields(t *testing.T) {
	now := time.Now()
	start := now.Add(-5 * time.Minute).Truncate(time.Minute)
	// Snapshot WITHOUT SoCSafetyFloorPct (simulates an older binary).
	d := &Diagnostic{
		ComputedAtMs:   now.Add(-1 * time.Minute).UnixMilli(),
		Zone:           "SE4",
		Horizon:        1,
		LastReplanAtMs: now.Add(-1 * time.Minute).UnixMilli(),
		Params: DiagnosticParams{
			Mode:                ModeSelfConsumption,
			InitialSoCPct:       8,
			SoCMinPct:           10,
			SoCMaxPct:           95,
			SoCLevels:           41,
			ActionLevels:        81,
			MaxChargeW:          5000,
			MaxDischargeW:       5000,
			ChargeEfficiency:    0.95,
			DischargeEfficiency: 0.95,
			CapacityWh:          16000,
			TerminalSoCPrice:    100,
			// SoCSafetyFloorPct: 0 (field didn't exist when snapshot was written)
			// SafetyFloorPenaltyOreKwhHour: 0
		},
		Slots: []DiagnosticSlot{{
			Idx: 0, SlotStartMs: start.UnixMilli(),
			SlotEndMs: start.Add(15 * time.Minute).UnixMilli(),
			LenMin:    15, PriceOre: 100, Confidence: 1, PVW: -3000, LoadW: 500,
			BatteryW: 0, GridW: -2500, SoCPct: 8,
			EMSMode: "self_consumption",
		}},
	}
	// Service has the newer defaults — operator picked 25% safety floor.
	svc := &Service{
		Zone: "SE4",
		Defaults: Params{
			Mode:                         ModeSelfConsumption,
			SoCSafetyFloorPct:            25,
			SafetyFloorPenaltyOreKwhHour: 100,
		},
	}
	if ok := svc.RestoreDiagnostic(d, now, "restored_diagnostic"); !ok {
		t.Fatal("RestoreDiagnostic returned false")
	}
	diag := svc.Diagnose()
	if diag == nil {
		t.Fatal("Diagnose returned nil after restore")
	}
	if diag.Params.SoCSafetyFloorPct != 25 {
		t.Errorf("SoCSafetyFloorPct after restore = %v, want 25 (merged from Defaults; snapshot had 0)", diag.Params.SoCSafetyFloorPct)
	}
	if diag.Params.SafetyFloorPenaltyOreKwhHour != 100 {
		t.Errorf("SafetyFloorPenaltyOreKwhHour after restore = %v, want 100 (merged from Defaults)", diag.Params.SafetyFloorPenaltyOreKwhHour)
	}
}

// If the snapshot DOES have explicit values (operator persisted a
// non-zero choice), restore must preserve them — not overwrite with
// Defaults. Only zero-in-snapshot fields get the merge.
func TestRestoreDiagnosticPreservesExplicitSnapshotValues(t *testing.T) {
	now := time.Now()
	start := now.Add(-5 * time.Minute).Truncate(time.Minute)
	d := &Diagnostic{
		ComputedAtMs:   now.Add(-1 * time.Minute).UnixMilli(),
		Zone:           "SE4",
		Horizon:        1,
		LastReplanAtMs: now.Add(-1 * time.Minute).UnixMilli(),
		Params: DiagnosticParams{
			Mode:                         ModeSelfConsumption,
			InitialSoCPct:                30,
			SoCMinPct:                    10,
			SoCMaxPct:                    95,
			SoCLevels:                    41,
			ActionLevels:                 81,
			MaxChargeW:                   5000,
			MaxDischargeW:                5000,
			ChargeEfficiency:             0.95,
			DischargeEfficiency:          0.95,
			CapacityWh:                   16000,
			TerminalSoCPrice:             100,
			SoCSafetyFloorPct:            15, // operator picked a different value
			SafetyFloorPenaltyOreKwhHour: 50,
		},
		Slots: []DiagnosticSlot{{
			Idx: 0, SlotStartMs: start.UnixMilli(),
			SlotEndMs: start.Add(15 * time.Minute).UnixMilli(),
			LenMin:    15, PriceOre: 100, Confidence: 1, PVW: -3000, LoadW: 500,
			BatteryW: 0, GridW: -2500, SoCPct: 30,
			EMSMode: "self_consumption",
		}},
	}
	svc := &Service{
		Zone: "SE4",
		Defaults: Params{
			Mode:                         ModeSelfConsumption,
			SoCSafetyFloorPct:            25, // Defaults would say 25 but snapshot has 15
			SafetyFloorPenaltyOreKwhHour: 100,
		},
	}
	if ok := svc.RestoreDiagnostic(d, now, "restored_diagnostic"); !ok {
		t.Fatal("RestoreDiagnostic returned false")
	}
	diag := svc.Diagnose()
	if diag.Params.SoCSafetyFloorPct != 15 {
		t.Errorf("SoCSafetyFloorPct = %v, want 15 (snapshot value must win over Defaults)", diag.Params.SoCSafetyFloorPct)
	}
	if diag.Params.SafetyFloorPenaltyOreKwhHour != 50 {
		t.Errorf("SafetyFloorPenaltyOreKwhHour = %v, want 50 (snapshot value must win)", diag.Params.SafetyFloorPenaltyOreKwhHour)
	}
}

// TestDiagnoseCarriesLoadpointFields — without this the plan table
// hides EV columns because its `lpActive` gate is
// `slots.some(x => x.loadpoint_w || x.loadpoint_soc_pct)`. Plumbing
// these fields through is the whole point of issue #174: when the
// battery covers `LOAD + EV`, an operator looking at `BATTERY −5.6
// kW` next to `LOAD 1.6 kW` would reasonably think the battery is
// exporting 4 kW to grid — the reality is the EV eats the
// difference. This test is the contract that the diagnostic carries
// enough information to explain that arithmetic.
func TestDiagnoseCarriesLoadpointFields(t *testing.T) {
	// Four cheap-priced slots with a deadline-bound EV. The DP
	// must charge at least once, so at least one action carries a
	// non-zero LoadpointW we can observe through the diagnostic.
	start := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC).UnixMilli()
	slots := []Slot{
		{StartMs: start, LenMin: 60, PriceOre: 30, SpotOre: 15,
			LoadW: 400, Confidence: 1.0},
		{StartMs: start + 3600_000, LenMin: 60, PriceOre: 20, SpotOre: 10,
			LoadW: 400, Confidence: 1.0},
		{StartMs: start + 7200_000, LenMin: 60, PriceOre: 25, SpotOre: 12,
			LoadW: 400, Confidence: 1.0},
		{StartMs: start + 10800_000, LenMin: 60, PriceOre: 40, SpotOre: 20,
			LoadW: 400, Confidence: 1.0},
	}
	p := Params{
		Mode:                ModeCheapCharge,
		SoCLevels:           21,
		CapacityWh:          10000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        5,
		MaxChargeW:          2000,
		MaxDischargeW:       2000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    70,
		Loadpoint: &LoadpointSpec{
			ID:               "garage",
			CapacityWh:       60000,
			Levels:           11,
			InitialSoCPct:    20,
			PluggedIn:        true,
			TargetSoCPct:     30,
			TargetSlotIdx:    3,
			MaxChargeW:       11000,
			AllowedStepsW:    []float64{0, 11000},
			ChargeEfficiency: 0.9,
		},
	}
	plan := Optimize(slots, p)
	svc := &Service{
		Zone:         "SE3",
		last:         &plan,
		lastSlots:    slots,
		lastParams:   p,
		lastReplanAt: time.UnixMilli(plan.GeneratedAtMs),
		lastReason:   "ev-test",
	}
	d := svc.Diagnose()
	if d == nil {
		t.Fatal("Diagnose returned nil with a loadpoint-enabled plan")
	}
	if d.LoadpointID != "garage" {
		t.Fatalf("LoadpointID = %q, want garage", d.LoadpointID)
	}

	// At least one slot must report the EV charging that the DP
	// decided — otherwise the UI's lpActive gate stays false and
	// the columns never appear.
	sawCharge := false
	sawSoC := false
	for i, row := range d.Slots {
		if row.LoadpointW != plan.Actions[i].LoadpointW {
			t.Errorf("slot %d LoadpointW: diagnostic=%.1f plan=%.1f — "+
				"the plumb from Action → DiagnosticSlot is broken",
				i, row.LoadpointW, plan.Actions[i].LoadpointW)
		}
		if row.LoadpointSoCPct != plan.Actions[i].LoadpointSoCPct {
			t.Errorf("slot %d LoadpointSoCPct: diagnostic=%.1f plan=%.1f",
				i, row.LoadpointSoCPct, plan.Actions[i].LoadpointSoCPct)
		}
		if row.LoadpointW > 0 {
			sawCharge = true
		}
		if row.LoadpointSoCPct > 0 {
			sawSoC = true
		}
	}
	if !sawCharge {
		t.Error("no slot carries LoadpointW — deadline-bound DP should " +
			"charge at least once; check Action.LoadpointW plumbing")
	}
	if !sawSoC {
		t.Error("no slot carries LoadpointSoCPct — EV SoC trajectory lost")
	}
}
