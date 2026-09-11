package mpc

import (
	"math"
	"testing"
)

// TestLoadpointDerateCapsResolution pins the EV derate: the battery-only
// default is cut to the capped grid, and anything already at or below the
// caps is left alone (a small grid must not be silently enlarged).
func TestLoadpointDerateCapsResolution(t *testing.T) {
	cases := []struct {
		name                string
		soc, action         int
		wantSoC, wantAction int
	}{
		{"battery-only default derates", 201, 401, 101, 201},
		{"below caps unchanged", 61, 81, 61, 81},
		{"at caps unchanged", 101, 201, 101, 201},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			soc, action := derateResolutionForLoadpoint(tc.soc, tc.action)
			if soc != tc.wantSoC || action != tc.wantAction {
				t.Fatalf("derate(%d,%d) = (%d,%d), want (%d,%d)",
					tc.soc, tc.action, soc, action, tc.wantSoC, tc.wantAction)
			}
		})
	}
}

// TestFineGridStillAbsorbsBorderlinePV carries the 2026-05-23 guarantee of
// TestSelfConsumptionAbsorbsCheapPVOver48hHorizon to the raised default
// grid. That test pins 41×81 as the floor; this one proves the finer
// 201×401 production grid did not reintroduce the bug from the other side
// — a 273 W net surplus must still land on a charge action rather than
// idle and export.
func TestFineGridStillAbsorbsBorderlinePV(t *testing.T) {
	slots := make([]Slot, 192)
	for i := range slots {
		var price, spot, pv, load float64
		d := i % 96
		switch {
		case d < 6:
			price, spot, pv, load = 152, 2, -2300, 693
		case d < 13:
			price, spot, pv, load = 170, 15, -1500, 800
		case d < 22:
			price, spot, pv, load = 260, 90, -200, 1800
		case d < 30:
			price, spot, pv, load = 170, 12, 0, 500
		case d < 40:
			price, spot, pv, load = 150, 5, 0, 500
		case d < 55:
			price, spot, pv, load = 130, 3, 0, 500
		case d < 70:
			price, spot, pv, load = 200, 50, -800, 1100
		case d < 85:
			price, spot, pv, load = 145, 1, -3500, 693
		default:
			price, spot, pv, load = 150, 2, -2900, 693
		}
		if i >= 96 { // Day 2: cloudy — no PV.
			pv = 0
		}
		slots[i] = Slot{
			LenMin:     15,
			PriceOre:   price,
			SpotOre:    spot,
			PVW:        pv,
			LoadW:      load,
			Confidence: 1.0,
		}
	}
	// Slot 0 nets 2300 − 693 = 1607 W of PV surplus; the regression is
	// about the smallest legal charge action, so shave load until the
	// surplus is the 273 W the original incident carried.
	slots[0].LoadW = 2300 - 273

	p := Params{
		Mode:                ModeSelfConsumption,
		CapacityWh:          20000,
		InitialSoC:          0.65,
		SoCMin:              0.1,
		SoCMax:              0.9,
		SoCLevels:           201, // main.go's buildMPC default
		ActionLevels:        401,
		MaxChargeW:          9000,
		MaxDischargeW:       9000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    163.99,
	}
	plan := Optimize(slots, p)
	if len(plan.Actions) == 0 {
		t.Fatal("plan empty")
	}
	a := plan.Actions[0]
	if a.BatteryW <= 0 {
		t.Fatalf("DP must charge battery in slot 0 on a 273 W surplus, got batt=%v reason=%q",
			a.BatteryW, a.Reason)
	}
	// modeAllows grants a 50 W boundary tolerance; anything beyond that
	// would mean the fine grid started importing to charge.
	if a.BatteryW > 273+50+1e-6 {
		t.Fatalf("charge must stay inside the surplus (no-battery-export rule), got batt=%v", a.BatteryW)
	}
}

// resolutionBenchSlots builds the 193-slot (48 h + 1) replan horizon the
// production service sees: 15-minute slots, a day-shaped PV curve and a
// two-peak price profile.
func resolutionBenchSlots() []Slot {
	slots := make([]Slot, 193)
	for i := range slots {
		h := float64(i%96) / 4.0 // hour of day

		pv := 0.0 // half-sine 05:00–21:00, ~7 kW peak, negative = production
		if h > 5 && h < 21 {
			pv = -7000 * math.Sin(math.Pi*(h-5)/16)
		}
		if i >= 96 {
			pv *= 0.4 // day 2 is overcast
		}

		spot := 12.0 // night trough
		if h >= 5 {
			spot = 35 + 20*math.Sin(math.Pi*(h-5)/14) + 75*math.Exp(-(h-18)*(h-18)/3)
		}
		load := 450 + 900*math.Exp(-(h-7)*(h-7)/1.5) + 1400*math.Exp(-(h-19)*(h-19)/2)

		slots[i] = Slot{
			StartMs:    int64(i) * 15 * 60 * 1000,
			LenMin:     15,
			PriceOre:   spot*1.25 + 95, // spot + grid fee + VAT
			SpotOre:    spot,
			PVW:        pv,
			LoadW:      load,
			Confidence: 1.0,
		}
	}
	return slots
}

func resolutionBenchParams() Params {
	return Params{
		Mode:                ModeSelfConsumption,
		CapacityWh:          20000,
		InitialSoC:          0.45,
		SoCMin:              0.10,
		SoCMax:              0.95,
		MaxChargeW:          9000,
		MaxDischargeW:       9000,
		ChargeEfficiency:    0.95,
		DischargeEfficiency: 0.95,
		TerminalSoCPrice:    160,
	}
}

// BenchmarkOptimize193BatteryOnly measures the raised default grid on a
// full replan horizon — the number the seconds-scale budget is checked
// against.
func BenchmarkOptimize193BatteryOnly(b *testing.B) {
	slots := resolutionBenchSlots()
	p := resolutionBenchParams()
	p.SoCLevels, p.ActionLevels = 201, 401

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if plan := Optimize(slots, p); len(plan.Actions) == 0 {
			b.Fatal("plan empty")
		}
	}
}

// BenchmarkOptimize193WithLoadpoint measures the same horizon once an EV
// loadpoint extends the state space, at the resolution the derate hands
// the DP.
func BenchmarkOptimize193WithLoadpoint(b *testing.B) {
	slots := resolutionBenchSlots()
	p := resolutionBenchParams()
	p.SoCLevels, p.ActionLevels = derateResolutionForLoadpoint(201, 401)
	lp := &LoadpointSpec{
		ID:               "garage",
		CapacityWh:       60000,
		Levels:           11,
		SoCMin:           0,
		SoCMax:           1,
		InitialSoC:       0.35,
		PluggedIn:        true,
		TargetSoC:        0.80,
		TargetSlotIdx:    64, // 16 h out
		MaxChargeW:       11000,
		AllowedStepsW:    []float64{0, 4140, 6900, 11000},
		ChargeEfficiency: 0.90,
	}
	p.Loadpoint = lp
	p.Loadpoints = []*LoadpointSpec{lp}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if plan := Optimize(slots, p); len(plan.Actions) == 0 {
			b.Fatal("plan empty")
		}
	}
}
