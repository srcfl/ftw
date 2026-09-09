package mpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWeekdayPeakHoursSkipWeekendAndHonorTimezone(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	// Friday 2026-09-04 20:00 through Monday 2026-09-07 08:00 local.
	from := time.Date(2026, 9, 4, 20, 0, 0, 0, loc)
	to := time.Date(2026, 9, 7, 8, 0, 0, 0, loc)
	hours := weekdayPeakClockHours(loc, from, to)
	if len(hours) != 2 {
		t.Fatalf("hours=%d, want Friday none + Monday 06–08 = 2, got %+v", len(hours), hours)
	}
	if hours[0].start != time.Date(2026, 9, 7, 6, 0, 0, 0, loc) || hours[1].start != time.Date(2026, 9, 7, 7, 0, 0, 0, loc) {
		t.Fatalf("unexpected hours: %+v", hours)
	}
}

func TestBindDemandChargesExpandsWeekdayHoursAndElapsed(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 9, 10, 7, 0, 0, loc) // Wednesday
	start := time.Date(2026, 9, 9, 10, 0, 0, 0, loc)
	slots := []Slot{
		{StartMs: start.UnixMilli(), ExecutionStartMs: now.UnixMilli(), LenMin: 15, PriceOre: 100, Confidence: 1},
		{StartMs: start.Add(15 * time.Minute).UnixMilli(), LenMin: 15, PriceOre: 100, Confidence: 1},
		{StartMs: start.Add(30 * time.Minute).UnixMilli(), LenMin: 15, PriceOre: 100, Confidence: 1},
		{StartMs: start.Add(45 * time.Minute).UnixMilli(), LenMin: 15, PriceOre: 100, Confidence: 1},
		{StartMs: start.Add(time.Hour).UnixMilli(), LenMin: 60, PriceOre: 80, Confidence: 1},
	}
	importWh := func(intervals [][2]int64) ([]float64, []int64) {
		wh := make([]float64, len(intervals))
		cov := make([]int64, len(intervals))
		for i, window := range intervals {
			cov[i] = window[1] - window[0]
			if window[1]-window[0] == 7*time.Minute.Milliseconds() {
				wh[i] = 500 // 0.5 kWh in the elapsed 10:00–10:07
			} else {
				wh[i] = 3000 // 3 kWh → 3 kW over a full hour
			}
		}
		return wh, cov
	}
	got := bindDemandCharges(slots, 7000, 3, 25, loc, now, importWh)
	if len(got) != 1 || got[0].ID != "weekday-high-2026-09" || got[0].TopN != 3 {
		t.Fatalf("charges=%+v", got)
	}
	if abs := got[0].PricePerKW - 8750; abs > 1e-9 {
		t.Fatalf("price_per_kw=%g, want 8750 after 25%% VAT", got[0].PricePerKW)
	}
	if len(got[0].Hours) < 2 {
		t.Fatalf("hours=%+v", got[0].Hours)
	}
	if got[0].Hours[0].StartMs != start.UnixMilli() || got[0].Hours[0].EndMs != start.Add(time.Hour).UnixMilli() {
		t.Fatalf("first hour=%+v", got[0].Hours[0])
	}
	if got[0].Hours[0].ElapsedImportKWh != 0.5 {
		t.Fatalf("elapsed=%g, want 0.5 kWh", got[0].Hours[0].ElapsedImportKWh)
	}
	if len(got[0].AlreadyKW) == 0 {
		t.Fatal("expected already_kw from earlier weekday hours this month")
	}
	for _, kw := range got[0].AlreadyKW {
		if kw < 2.9 || kw > 3.1 {
			t.Fatalf("already_kw=%v", got[0].AlreadyKW)
		}
	}
}

func TestBindDemandChargesSkipsUnalignedCurrentHour(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 9, 9, 10, 17, 0, 0, loc)
	start := time.Date(2026, 9, 9, 10, 15, 0, 0, loc)
	slots := []Slot{
		{StartMs: start.UnixMilli(), ExecutionStartMs: now.UnixMilli(), LenMin: 15, Confidence: 1},
		{StartMs: start.Add(15 * time.Minute).UnixMilli(), LenMin: 45, Confidence: 1},
		{StartMs: start.Add(time.Hour).UnixMilli(), LenMin: 60, Confidence: 1},
	}
	got := bindDemandCharges(slots, 7000, 1, 0, loc, now, nil)
	if len(got) != 1 || len(got[0].Hours) == 0 {
		t.Fatalf("charges=%+v", got)
	}
	if got[0].Hours[0].StartMs != start.Add(45*time.Minute).UnixMilli() {
		t.Fatalf("current 10:00 hour is not inside the slot timeline, first hour=%+v", got[0].Hours[0])
	}
}

func TestOptimizerRequestDropsOreKeys(t *testing.T) {
	o := &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: time.Second}}
	slots, p := externalTestFixture()
	p.DemandCharges = []DemandCharge{{
		ID: "weekday-high-2026-09", PricePerKW: 8750, TopN: 3,
		Hours: []DemandHour{{StartMs: slots[0].StartMs, EndMs: slots[0].StartMs + 3600_000}},
	}}
	raw, err := json.Marshal(o.buildRequest(slots, p))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, key := range []string{"price_ore", "spot_ore", "export_ore_per_kwh", "terminal_price_ore_kwh"} {
		if strings.Contains(body, `"`+key+`"`) {
			t.Fatalf("planner wire still has %s: %s", key, body)
		}
	}
	for _, key := range []string{"price_per_kwh", "spot_per_kwh", "demand_charges", "price_per_kw"} {
		if !strings.Contains(body, `"`+key+`"`) {
			t.Fatalf("planner wire missing %s", key)
		}
	}
}

func TestNativeDemandChargesOnWeekdayHour(t *testing.T) {
	o := nativeWorker(t, 500*time.Millisecond)
	defer o.Close()
	start := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	slots := []Slot{
		{StartMs: start.UnixMilli(), LenMin: 60, PriceOre: 100, SpotOre: 50, Confidence: 1, LoadW: 500,
			Limits: PowerLimits{MaxImportW: 8000, MaxExportW: 8000}},
		{StartMs: start.Add(time.Hour).UnixMilli(), LenMin: 60, PriceOre: 300, SpotOre: 240, Confidence: 1, LoadW: 500,
			Limits: PowerLimits{MaxImportW: 8000, MaxExportW: 8000}},
	}
	p := Params{
		Mode: ModeArbitrage, CapacityWh: 10000,
		SoCMin: 0.1, SoCMax: 0.95, InitialSoC: 0.5,
		MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		TerminalSoCPrice: 100,
		DemandCharges: []DemandCharge{{
			ID: "weekday-high-2026-09", PricePerKW: 7000, TopN: 1,
			Hours: []DemandHour{{StartMs: start.UnixMilli(), EndMs: start.Add(time.Hour).UnixMilli()}},
		}},
	}
	plan, err := o.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	var req externalRequest
	if err := json.Unmarshal(plan.OptimizerInput, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.DemandCharges) != 1 || req.DemandCharges[0].PricePerKW != 7000 || len(req.DemandCharges[0].Hours) != 1 {
		t.Fatalf("demand_charges=%+v", req.DemandCharges)
	}
	if plan.Solver.Backend != "fleet_milp_rust" {
		t.Fatalf("backend=%s, want fleet_milp_rust", plan.Solver.Backend)
	}
}

func TestEnergyplanTimeBudgetUsesFleetWhenDemandCharges(t *testing.T) {
	slots := []Slot{{StartMs: time.Now().UnixMilli(), LenMin: 15}}
	p := Params{CapacityWh: 10000, DemandCharges: []DemandCharge{{ID: "weekday-high-2026-09"}}}
	if got := energyplanTimeBudget(slots, p); got != energyplanFleetBudget {
		t.Fatalf("budget=%s, want %s", got, energyplanFleetBudget)
	}
}
