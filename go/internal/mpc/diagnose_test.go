package mpc

import (
	"encoding/json"
	"fmt"
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
			PVW: -200, LoadW: 400, Confidence: 1.0,
			InputProvenanceSchema: inputProvenanceSchemaVersion,
			PriceInputSource:      "entsoe", PriceInputAvailableAtMs: 111,
			WeatherRowSource: "met.no", WeatherRowAvailableAtMs: 222},
		{StartMs: start + 15*60*1000, LenMin: 15, PriceOre: 150,
			SpotOre: 80, PVW: -100, LoadW: 500, Confidence: 0.6,
			InputProvenanceSchema: inputProvenanceSchemaVersion,
			PriceInputSource:      "forecast", PriceInputAvailableAtMs: 333,
			WeatherRowSource: "open-meteo", WeatherRowAvailableAtMs: 444},
	}
	p := Params{
		Mode:                ModeSelfConsumption,
		SoCLevels:           11,
		CapacityWh:          10000,
		SoCMin: 0.1,
		SoCMax: 0.95,
		InitialSoC: 0.5,
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
	if d.Params.InitialSoC != 0.50 {
		t.Errorf("Params.InitialSoC: got %.2f want 0.50", d.Params.InitialSoC)
	}
	if d.LastReason != "unit-test" {
		t.Errorf("LastReason: got %q want unit-test", d.LastReason)
	}
	if d.InputProvenanceSchema != inputProvenanceSchemaVersion {
		t.Errorf("InputProvenanceSchema: got %d want %d", d.InputProvenanceSchema, inputProvenanceSchemaVersion)
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
	if row.PriceInputSource != "entsoe" || row.PriceInputAvailableAtMs != 111 ||
		row.WeatherRowSource != "met.no" || row.WeatherRowAvailableAtMs != 222 {
		t.Errorf("row0 input provenance: %+v", row)
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
	if row := d.Slots[1]; row.PriceInputSource != "forecast" || row.PriceInputAvailableAtMs != 333 ||
		row.WeatherRowSource != "open-meteo" || row.WeatherRowAvailableAtMs != 444 {
		t.Errorf("row1 input provenance: %+v", row)
	}
	if d.Slots[1].SlotStartMs != start+15*60*1000 {
		t.Errorf("row1 SlotStartMs: got %d want %d",
			d.Slots[1].SlotStartMs, start+15*60*1000)
	}
}

func TestDiagnosticProvenanceSchemaMarksCurrentMissingRows(t *testing.T) {
	d := buildDiagnostic(
		&Plan{GeneratedAtMs: 1, HorizonSlots: 1, Actions: []Action{{}}},
		[]Slot{{
			StartMs:               1,
			LenMin:                15,
			InputProvenanceSchema: inputProvenanceSchemaVersion,
		}},
		Params{}, "SE3", 1, "test",
	)
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		InputProvenanceSchema int                          `json:"input_provenance_schema"`
		Slots                 []map[string]json.RawMessage `json:"slots"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.InputProvenanceSchema != inputProvenanceSchemaVersion {
		t.Fatalf("input provenance schema = %d, want %d", wire.InputProvenanceSchema, inputProvenanceSchemaVersion)
	}
	for _, key := range []string{
		"price_input_source", "price_input_available_at_ms",
		"weather_row_source", "weather_row_available_at_ms",
	} {
		if _, ok := wire.Slots[0][key]; ok {
			t.Errorf("current slot with unknown provenance emitted %q", key)
		}
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
		DecisionID:            testDecisionID1,
		ComputedAtMs:          now.Add(-1 * time.Minute).UnixMilli(),
		InputProvenanceSchema: inputProvenanceSchemaVersion,
		Zone:                  "SE4",
		Horizon:               2,
		TotalCostOre:          -12.3,
		LastReplanAtMs:        now.Add(-1 * time.Minute).UnixMilli(),
		LastReason:            "scheduled",
		Params: DiagnosticParams{
			Mode:                ModeSelfConsumption,
			InitialSoC: 0.42,
			SoCMin: 0.1,
			SoCMax: 0.9,
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
				Idx:              0,
				SlotStartMs:      start.UnixMilli(),
				SlotEndMs:        start.Add(15 * time.Minute).UnixMilli(),
				LenMin:           15,
				PriceOre:         120,
				SpotOre:          80,
				Confidence:       1,
				PVW:              -4500,
				LoadW:            900,
				BatteryW:         0,
				GridW:            -3600,
				SoC: 0.42,
				CostOre:          -72,
				Reason:           "export surplus",
				EMSMode:          "self_consumption",
				PVLimitW:         4100,
				PriceInputSource: "entsoe", PriceInputAvailableAtMs: 111,
				WeatherRowSource: "met.no", WeatherRowAvailableAtMs: 222,
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
				SoC: 0.44,
				CostOre:     9,
				Reason:      "avoid negative export",
				EMSMode:     "self_consumption",
			},
		},
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		DecisionID            string                       `json:"decision_id"`
		InputProvenanceSchema int                          `json:"input_provenance_schema"`
		Slots                 []map[string]json.RawMessage `json:"slots"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Slots) == 0 {
		t.Fatal("diagnostic JSON has no slots")
	}
	if wire.DecisionID != testDecisionID1 {
		t.Fatalf("diagnostic JSON decision ID = %q, want %q", wire.DecisionID, testDecisionID1)
	}
	if wire.InputProvenanceSchema != inputProvenanceSchemaVersion {
		t.Fatalf("diagnostic JSON provenance schema = %d, want %d", wire.InputProvenanceSchema, inputProvenanceSchemaVersion)
	}
	for _, key := range []string{
		"price_input_source", "price_input_available_at_ms",
		"weather_row_source", "weather_row_available_at_ms",
	} {
		if _, ok := wire.Slots[0][key]; !ok {
			t.Errorf("diagnostic JSON slot lacks %q", key)
		}
	}
	wantWireValues := map[string]any{
		"price_input_source":          "entsoe",
		"price_input_available_at_ms": float64(111),
		"weather_row_source":          "met.no",
		"weather_row_available_at_ms": float64(222),
	}
	for key, want := range wantWireValues {
		var got any
		if err := json.Unmarshal(wire.Slots[0][key], &got); err != nil {
			t.Fatalf("decode diagnostic JSON key %q: %v", key, err)
		}
		if got != want {
			t.Errorf("diagnostic JSON key %q = %#v, want %#v", key, got, want)
		}
	}
	var roundTripped Diagnostic
	if err := json.Unmarshal(raw, &roundTripped); err != nil {
		t.Fatal(err)
	}
	d = &roundTripped
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
	if latest.DecisionID != d.DecisionID {
		t.Fatalf("restored DecisionID = %q, want %q", latest.DecisionID, d.DecisionID)
	}
	dir, ok := svc.SlotDirectiveAt(now)
	if !ok {
		t.Fatal("SlotDirectiveAt returned ok=false after restore")
	}
	if dir.BatteryEnergyWh != 0 {
		t.Fatalf("BatteryEnergyWh = %v, want 0", dir.BatteryEnergyWh)
	}
	if dir.DecisionID != d.DecisionID || dir.SlotStart.UnixMilli() != d.Slots[0].SlotStartMs {
		t.Fatalf("directive identity = (%q, %d), want (%q, %d)",
			dir.DecisionID, dir.SlotStart.UnixMilli(), d.DecisionID, d.Slots[0].SlotStartMs)
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
	if diag.DecisionID != d.DecisionID {
		t.Fatalf("diagnostic decision ID after restore = %q, want %q", diag.DecisionID, d.DecisionID)
	}
	if diag.InputProvenanceSchema != inputProvenanceSchemaVersion {
		t.Fatalf("input provenance schema after JSON restore = %d, want %d", diag.InputProvenanceSchema, inputProvenanceSchemaVersion)
	}
	if row := diag.Slots[0]; row.PriceInputSource != "entsoe" || row.PriceInputAvailableAtMs != 111 ||
		row.WeatherRowSource != "met.no" || row.WeatherRowAvailableAtMs != 222 {
		t.Fatalf("input provenance after JSON restore = %+v", row)
	}
}

func TestRestoreDiagnosticRejectsInvalidDecisionIdentity(t *testing.T) {
	now := time.Now()
	start := now.Add(-5 * time.Minute).Truncate(time.Minute).UnixMilli()
	validSlot := DiagnosticSlot{SlotStartMs: start, LenMin: 15}
	canonicalID := "550e8400-e29b-41d4-a716-446655440000"
	tests := []struct {
		name       string
		decisionID string
		slots      []DiagnosticSlot
	}{
		{name: "invalid uuid", decisionID: "not-a-uuid", slots: []DiagnosticSlot{validSlot}},
		{name: "nil uuid", decisionID: "00000000-0000-0000-0000-000000000000", slots: []DiagnosticSlot{validSlot}},
		{name: "uppercase uuid", decisionID: "550E8400-E29B-41D4-A716-446655440000", slots: []DiagnosticSlot{validSlot}},
		{name: "compact uuid", decisionID: "550e8400e29b41d4a716446655440000", slots: []DiagnosticSlot{validSlot}},
		{name: "braced uuid", decisionID: "{" + canonicalID + "}", slots: []DiagnosticSlot{validSlot}},
		{name: "urn uuid", decisionID: "urn:uuid:" + canonicalID, slots: []DiagnosticSlot{validSlot}},
		{name: "duplicate slot start", decisionID: testDecisionID1, slots: []DiagnosticSlot{validSlot, validSlot}},
		{name: "overlapping slots", decisionID: testDecisionID1, slots: []DiagnosticSlot{
			validSlot,
			{SlotStartMs: start + 10*60*1000, LenMin: 15},
		}},
		{name: "out of order slots", decisionID: testDecisionID1, slots: []DiagnosticSlot{
			{SlotStartMs: start + 15*60*1000, LenMin: 15},
			validSlot,
		}},
		{name: "inconsistent slot end", decisionID: testDecisionID1, slots: []DiagnosticSlot{{
			SlotStartMs: start, SlotEndMs: start + 20*60*1000, LenMin: 15,
		}}},
		{name: "non-positive slot start", decisionID: testDecisionID1, slots: []DiagnosticSlot{{SlotStartMs: 0, LenMin: 15}}},
		{name: "invalid slot length", decisionID: testDecisionID1, slots: []DiagnosticSlot{{SlotStartMs: start}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &Diagnostic{
				DecisionID:     tc.decisionID,
				ComputedAtMs:   now.Add(-time.Minute).UnixMilli(),
				LastReplanAtMs: now.Add(-time.Minute).UnixMilli(),
				Params:         DiagnosticParams{Mode: ModeSelfConsumption},
				Slots:          tc.slots,
			}
			svc := &Service{Defaults: Params{Mode: ModeSelfConsumption}}
			if svc.RestoreDiagnostic(d, now, "test") {
				t.Fatalf("RestoreDiagnostic accepted invalid identity: %+v", d)
			}
			if svc.Latest() != nil {
				t.Fatal("invalid identified diagnostic published an active plan")
			}
		})
	}
}

func TestRestoreDiagnosticRejectsUnsupportedInputProvenanceSchema(t *testing.T) {
	for _, schema := range []int{-1, inputProvenanceSchemaVersion + 1} {
		t.Run(fmt.Sprintf("schema_%d", schema), func(t *testing.T) {
			now := time.Now()
			d := &Diagnostic{
				ComputedAtMs:          now.UnixMilli(),
				InputProvenanceSchema: schema,
				Slots: []DiagnosticSlot{{
					SlotStartMs: now.UnixMilli(),
					LenMin:      15,
				}},
			}
			svc := &Service{}
			if svc.RestoreDiagnostic(d, now, "test") {
				t.Fatalf("RestoreDiagnostic accepted unsupported provenance schema %d", schema)
			}
			if svc.Latest() != nil {
				t.Fatal("unsupported diagnostic published an active plan")
			}
		})
	}
}

// RestoreDiagnostic must merge fields that are zero-in-snapshot but
// non-zero-in-Defaults with the current Defaults value, so a deploy that adds
// a new Params field doesn't let an older snapshot's zero overwrite the
// operator's intended default until the next replan. Exercised here with
// PVChargeBonusOreKwh.
func TestRestoreDiagnosticMergesNewerDefaultsForMissingFields(t *testing.T) {
	now := time.Now()
	start := now.Add(-5 * time.Minute).Truncate(time.Minute)
	// Snapshot WITHOUT PVChargeBonusOreKwh (simulates an older binary).
	d := &Diagnostic{
		ComputedAtMs:   now.Add(-1 * time.Minute).UnixMilli(),
		Zone:           "SE4",
		Horizon:        1,
		LastReplanAtMs: now.Add(-1 * time.Minute).UnixMilli(),
		Params: DiagnosticParams{
			Mode:                ModeSelfConsumption,
			InitialSoC: 0.08,
			SoCMin: 0.1,
			SoCMax: 0.95,
			SoCLevels:           41,
			ActionLevels:        81,
			MaxChargeW:          5000,
			MaxDischargeW:       5000,
			ChargeEfficiency:    0.95,
			DischargeEfficiency: 0.95,
			CapacityWh:          16000,
			TerminalSoCPrice:    100,
			// PVChargeBonusOreKwh: 0 (field didn't exist when snapshot was written)
		},
		Slots: []DiagnosticSlot{{
			Idx: 0, SlotStartMs: start.UnixMilli(),
			SlotEndMs: start.Add(15 * time.Minute).UnixMilli(),
			LenMin:    15, PriceOre: 100, Confidence: 1, PVW: -3000, LoadW: 500,
			BatteryW: 0, GridW: -2500, SoC: 0.08,
			EMSMode: "self_consumption",
		}},
	}
	// Service has the newer defaults — operator picked a 30 öre PV-charge bonus.
	svc := &Service{
		Zone: "SE4",
		Defaults: Params{
			Mode:                ModeSelfConsumption,
			PVChargeBonusOreKwh: 30,
		},
	}
	if ok := svc.RestoreDiagnostic(d, now, "restored_diagnostic"); !ok {
		t.Fatal("RestoreDiagnostic returned false")
	}
	diag := svc.Diagnose()
	if diag == nil {
		t.Fatal("Diagnose returned nil after restore")
	}
	if diag.DecisionID != "" {
		t.Errorf("legacy snapshot gained decision ID %q", diag.DecisionID)
	}
	if diag.Params.PVChargeBonusOreKwh != 30 {
		t.Errorf("PVChargeBonusOreKwh after restore = %v, want 30 (merged from Defaults; snapshot had 0)", diag.Params.PVChargeBonusOreKwh)
	}
	if diag.InputProvenanceSchema != 0 {
		t.Errorf("legacy snapshot provenance schema = %d, want 0", diag.InputProvenanceSchema)
	}
	if row := diag.Slots[0]; row.PriceInputSource != "" || row.PriceInputAvailableAtMs != 0 ||
		row.WeatherRowSource != "" || row.WeatherRowAvailableAtMs != 0 {
		t.Errorf("legacy snapshot gained input provenance: %+v", row)
	}
	raw, err := json.Marshal(diag)
	if err != nil {
		t.Fatal(err)
	}
	var legacyWire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &legacyWire); err != nil {
		t.Fatal(err)
	}
	if _, ok := legacyWire["input_provenance_schema"]; ok {
		t.Error("legacy diagnostic JSON gained input_provenance_schema")
	}
	if _, ok := legacyWire["decision_id"]; ok {
		t.Error("legacy diagnostic JSON gained decision_id")
	}
	var legacySlots []map[string]json.RawMessage
	if err := json.Unmarshal(legacyWire["slots"], &legacySlots); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"price_input_source", "price_input_available_at_ms",
		"weather_row_source", "weather_row_available_at_ms",
	} {
		if _, ok := legacySlots[0][key]; ok {
			t.Errorf("legacy diagnostic JSON slot gained %q", key)
		}
	}
}

// If the snapshot DOES have an explicit value (operator persisted a
// non-zero choice), restore must preserve it — not overwrite with
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
			Mode:                ModeSelfConsumption,
			InitialSoC: 0.3,
			SoCMin: 0.1,
			SoCMax: 0.95,
			SoCLevels:           41,
			ActionLevels:        81,
			MaxChargeW:          5000,
			MaxDischargeW:       5000,
			ChargeEfficiency:    0.95,
			DischargeEfficiency: 0.95,
			CapacityWh:          16000,
			TerminalSoCPrice:    100,
			PVChargeBonusOreKwh: 15, // operator picked a different value
		},
		Slots: []DiagnosticSlot{{
			Idx: 0, SlotStartMs: start.UnixMilli(),
			SlotEndMs: start.Add(15 * time.Minute).UnixMilli(),
			LenMin:    15, PriceOre: 100, Confidence: 1, PVW: -3000, LoadW: 500,
			BatteryW: 0, GridW: -2500, SoC: 0.3,
			EMSMode: "self_consumption",
		}},
	}
	svc := &Service{
		Zone: "SE4",
		Defaults: Params{
			Mode:                ModeSelfConsumption,
			PVChargeBonusOreKwh: 30, // Defaults would say 30 but snapshot has 15
		},
	}
	if ok := svc.RestoreDiagnostic(d, now, "restored_diagnostic"); !ok {
		t.Fatal("RestoreDiagnostic returned false")
	}
	diag := svc.Diagnose()
	if diag.Params.PVChargeBonusOreKwh != 15 {
		t.Errorf("PVChargeBonusOreKwh = %v, want 15 (snapshot value must win over Defaults)", diag.Params.PVChargeBonusOreKwh)
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
		SoCMin: 0.1,
		SoCMax: 0.95,
		InitialSoC: 0.5,
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
			InitialSoC: 0.2,
			PluggedIn:        true,
			TargetSoC: 0.3,
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
		if row.LoadpointSoC != plan.Actions[i].LoadpointSoC {
			t.Errorf("slot %d LoadpointSoC: diagnostic=%.1f plan=%.1f",
				i, row.LoadpointSoC, plan.Actions[i].LoadpointSoC)
		}
		if row.LoadpointW > 0 {
			sawCharge = true
		}
		if row.LoadpointSoC > 0 {
			sawSoC = true
		}
	}
	if !sawCharge {
		t.Error("no slot carries LoadpointW — deadline-bound DP should " +
			"charge at least once; check Action.LoadpointW plumbing")
	}
	if !sawSoC {
		t.Error("no slot carries LoadpointSoC — EV SoC trajectory lost")
	}
}
