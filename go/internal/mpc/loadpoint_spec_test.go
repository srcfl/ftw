package mpc

import "testing"

func TestNormalizedStepsDefaults(t *testing.T) {
	cases := []struct {
		name string
		lp   *LoadpointSpec
		want []float64
	}{
		{"nil spec", nil, nil},
		{"empty steps, no max", &LoadpointSpec{}, []float64{0}},
		{"empty steps, max only", &LoadpointSpec{MaxChargeW: 11000}, []float64{0, 11000}},
		{"explicit steps preserved", &LoadpointSpec{
			MaxChargeW:    11000,
			AllowedStepsW: []float64{1400, 4100, 7400},
		}, []float64{0, 1400, 4100, 7400}},
		{"dedup + zero always included", &LoadpointSpec{
			AllowedStepsW: []float64{0, 0, 1400, 1400, 4100},
		}, []float64{0, 1400, 4100}},
		{"above max clamped out", &LoadpointSpec{
			MaxChargeW:    5000,
			AllowedStepsW: []float64{1400, 4100, 7400, 11000},
		}, []float64{0, 1400, 4100}},
		{"negative filtered", &LoadpointSpec{
			AllowedStepsW: []float64{-100, 1400},
		}, []float64{0, 1400}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.lp.normalizedSteps()
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d]=%f, want %f", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestActiveGate(t *testing.T) {
	cases := []struct {
		name string
		lp   *LoadpointSpec
		want bool
	}{
		{"nil", nil, false},
		{"unplugged", &LoadpointSpec{CapacityWh: 60000, Levels: 11}, false},
		{"zero capacity", &LoadpointSpec{PluggedIn: true, Levels: 11}, false},
		{"too-coarse levels", &LoadpointSpec{PluggedIn: true, CapacityWh: 60000, Levels: 1}, false},
		{"active", &LoadpointSpec{PluggedIn: true, CapacityWh: 60000, Levels: 11}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lp.active(); got != tc.want {
				t.Errorf("active() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOptimizePrefersCheapSlotsForEV is the core EV-planning contract:
// MPC should schedule EV charging in cheap slots and skip expensive
// ones, given the EV can wait until cheaper prices arrive before its
// deadline.
func TestOptimizePrefersCheapSlotsForEV(t *testing.T) {
	// 4 hourly slots. Slot 0 expensive (150), slot 1 cheap (20),
	// slot 2 expensive (180), slot 3 cheap (30). Target: charge EV
	// by end of slot 3 (50 %). No PV, flat 500 W load. Battery
	// inactive-ish (small capacity).
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 150, SpotOre: 100,
			LoadW: 500, Confidence: 1.0},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 20, SpotOre: 10,
			LoadW: 500, Confidence: 1.0},
		{StartMs: 7200_000, LenMin: 60, PriceOre: 180, SpotOre: 140,
			LoadW: 500, Confidence: 1.0},
		{StartMs: 10800_000, LenMin: 60, PriceOre: 30, SpotOre: 20,
			LoadW: 500, Confidence: 1.0},
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
		TerminalSoCPrice:    70,
		Loadpoint: &LoadpointSpec{
			ID:              "garage",
			CapacityWh:      60000, // 60 kWh
			Levels:          11,
			InitialSoCPct:   20,
			PluggedIn:       true,
			TargetSoCPct:    30,   // need 10 % → 6 kWh
			TargetSlotIdx:   3,    // deadline at end of horizon
			MaxChargeW:      11000,
			AllowedStepsW:   []float64{0, 11000},
			ChargeEfficiency: 0.9,
		},
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) != 4 {
		t.Fatalf("got %d actions, want 4", len(plan.Actions))
	}
	// Charging in slot 1 (cheap) or slot 3 (cheap) is acceptable.
	// Charging in slot 0 (150) or slot 2 (180) when cheaper
	// alternatives exist should not happen.
	if plan.Actions[0].LoadpointW > 0 {
		t.Errorf("charged EV in slot 0 (expensive) — DP should have waited")
	}
	if plan.Actions[2].LoadpointW > 0 {
		t.Errorf("charged EV in slot 2 (expensive) — DP should have waited")
	}
	// At least one cheap slot should have EV charging.
	cheapCharged := plan.Actions[1].LoadpointW > 0 || plan.Actions[3].LoadpointW > 0
	if !cheapCharged {
		t.Errorf("DP did not charge EV in any cheap slot; actions: %+v", plan.Actions)
	}
	// Final EV SoC should be at or above target (hard deadline).
	finalEV := plan.Actions[3].LoadpointSoCPct
	if finalEV < p.Loadpoint.TargetSoCPct-1 {
		t.Errorf("final EV SoC %.1f below target %.1f — deadline missed",
			finalEV, p.Loadpoint.TargetSoCPct)
	}
}

// TestOptimizeNilLoadpointUnchanged asserts the legacy battery-only
// path produces identical decisions when Loadpoint is nil vs. a
// non-plugged spec. Guards the refactor from regressing.
func TestOptimizeNilLoadpointUnchanged(t *testing.T) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PriceOre: 150, SpotOre: 100,
			LoadW: 500, Confidence: 1.0},
		{StartMs: 3600_000, LenMin: 60, PriceOre: 20, SpotOre: 10,
			LoadW: 500, Confidence: 1.0},
	}
	p := Params{
		Mode:                ModeCheapCharge,
		SoCLevels:           21,
		CapacityWh:          10000,
		SoCMinPct:           10,
		SoCMaxPct:           95,
		InitialSoCPct:       50,
		ActionLevels:        11,
		MaxChargeW:          3000,
		MaxDischargeW:       3000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    70,
	}
	planNil := Optimize(slots, p)
	p.Loadpoint = &LoadpointSpec{ID: "none", PluggedIn: false}
	planUnplugged := Optimize(slots, p)
	if len(planNil.Actions) != len(planUnplugged.Actions) {
		t.Fatalf("length mismatch: %d vs %d",
			len(planNil.Actions), len(planUnplugged.Actions))
	}
	for i := range planNil.Actions {
		if planNil.Actions[i].BatteryW != planUnplugged.Actions[i].BatteryW {
			t.Errorf("slot %d: battery action differs: nil=%.1f unplugged=%.1f",
				i, planNil.Actions[i].BatteryW, planUnplugged.Actions[i].BatteryW)
		}
		if planUnplugged.Actions[i].LoadpointW != 0 {
			t.Errorf("slot %d: unplugged LP should produce 0 W, got %.1f",
				i, planUnplugged.Actions[i].LoadpointW)
		}
	}
}
