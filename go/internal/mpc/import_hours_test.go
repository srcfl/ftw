package mpc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func stockholm(t testing.TB) *time.Location {
	t.Helper()
	zone, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	return zone
}

// planImportWh is the plan's import in [fromMs, toMs), counting a charging
// pulse's import while it runs.
func planImportWh(plan Plan, fromMs, toMs int64) float64 {
	var wh float64
	for _, a := range plan.Actions {
		start := max(a.ExecutionStart(), fromMs)
		end := min(a.SlotStartMs+int64(a.SlotLenMin)*60_000, toMs)
		if end <= start {
			continue
		}
		pulseGridW(a, func(share, gridW float64) { wh += share * max(0, gridW) * float64(end-start) / 3.6e6 })
	}
	return wh
}

// importCeilingFieldService is the home box at 09:00 on 25 October 2026
// (v0.155.1-beta.1): an hourly import ceiling of 4.5 kW, about 0.8 kW of
// house load, a 9.6 kWh battery that may feed the car, and a car that wants
// 19 kWh more by 13:00 on a 3 × 20 A fuse. The plan ran the car at 4.83 kW
// from 09:00, imported 4.5-5.7 kWh an hour and called the goal met. Core held
// each hour under 4.5 kWh. A planned hour holds the ceiling less half of
// Core's 690 W rise step (control.PlanningImportCeiling).
func importCeilingFieldService(t *testing.T, optimizer PlanOptimizer, ceiling bool) (*Service, time.Time) {
	t.Helper()
	start := time.Date(2026, 10, 25, 9, 0, 0, 0, stockholm(t))
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	prices := make([]state.PricePoint, 32)
	for i := range prices {
		prices[i] = state.PricePoint{
			Zone: "SE3", SlotTsMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), SlotLenMin: 15,
			SpotOreKwh: 40 + float64(i%4), TotalOreKwh: 90 + float64(i%4), Source: "test", FetchedAtMs: start.UnixMilli(),
		}
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	svc := New(st, nil, "SE3", Params{
		Mode: ModeArbitrage, SoCLevels: 21, ActionLevels: 21,
		CapacityWh: 9600, InitialSoC: .5, SoCMin: .1, SoCMax: .95,
		MaxChargeW: 5000, MaxDischargeW: 5000, ChargeEfficiency: .95, DischargeEfficiency: .95,
	})
	svc.Optimizer = optimizer
	svc.Horizon = 8 * time.Hour
	svc.BaseLoad = 800
	svc.now = func() time.Time { return start.Add(30 * time.Second) }
	tel := telemetry.NewStore()
	soc := .5
	tel.Update("sungrow", telemetry.DerBattery, 0, &soc, nil)
	tel.DriverHealthMut("sungrow").RecordSuccess()
	svc.Tele = tel
	svc.UpdateBatteryFleet([]BatteryFleetMember{{Driver: "sungrow", CapacityWh: 9600, MaxChargeW: 5000, MaxDischargeW: 5000}}, 9600, 5000, 5000)
	svc.UpdateSiteEconomics(SiteEconomics{FuseMaxW: 13800, Timezone: "Europe/Stockholm"})
	svc.Loadpoint = func(int) *LoadpointSpec {
		return &LoadpointSpec{ID: "carport", CapacityWh: 75000, InitialSoC: .8 - 19000./75000, SoCMax: 1,
			Levels: 21, PluggedIn: true, TargetSoC: .8, TargetSlotIdx: 15, MinChargeW: 4140, MaxChargeW: 11040,
			AllowedStepsW:    []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040},
			ChargeEfficiency: .9}
	}
	if ceiling {
		svc.ImportCeiling = func() ImportCeiling {
			return ImportCeiling{W: 4500, MeanW: 4155, HourStart: start, ElapsedWh: 800 * 30 / 3600.}
		}
	}
	return svc, start
}

func checkImportCeilingFieldPlan(t *testing.T, svc *Service, start time.Time) Plan {
	t.Helper()
	if svc.Replan(context.Background()) == nil {
		t.Fatalf("no plan published: %+v", svc.PlanSnapshot())
	}
	plan := *svc.PlanSnapshot().Plan
	for h := 0; h < 8; h++ {
		from := start.Add(time.Duration(h) * time.Hour)
		budget := 4155.0
		if h == 0 {
			budget -= 800 * 30 / 3600.
		}
		if got := planImportWh(plan, from.UnixMilli(), from.Add(time.Hour).UnixMilli()); got > budget+1 {
			t.Errorf("%s: plan imports %.0f Wh, over the hour's %.0f Wh", from.Format("15:04"), got, budget)
		}
	}
	short := plan.LoadpointShortfallWh["carport"]
	// The battery covers the house, so the car gets 4 h × 4.155 kW at 90 %:
	// about 15 kWh of 19.
	if delivered := 19000 - short; short < 1000 || delivered < 14500 {
		t.Fatalf("goal by 13:00: %.0f Wh short, %.0f Wh delivered; want it flagged short with the hours' room used", short, delivered)
	}
	return plan
}

func TestImportCeilingFieldCase(t *testing.T) {
	t.Run("the hours stay under the ceiling and the goal is short", func(t *testing.T) {
		svc, start := importCeilingFieldService(t, nil, true)
		plan := checkImportCeilingFieldPlan(t, svc, start)
		// The battery covers the house so the car gets the hour's room.
		for _, a := range plan.Actions[:4] {
			if a.LoadpointW > 0 && a.BatteryW > -700 {
				t.Errorf("%s: battery %.0f W beside the car; want it covering the house", time.UnixMilli(a.SlotStartMs).In(start.Location()).Format("15:04"), a.BatteryW)
			}
		}
	})
	t.Run("without the ceiling the plan overruns the hours", func(t *testing.T) {
		// The scenario exercises the ceiling: master's plan for it.
		svc, start := importCeilingFieldService(t, nil, false)
		if svc.Replan(context.Background()) == nil {
			t.Fatal("no plan published")
		}
		plan := *svc.PlanSnapshot().Plan
		if got := planImportWh(plan, start.UnixMilli(), start.Add(time.Hour).UnixMilli()); got < 5000 {
			t.Fatalf("09:00 imports %.0f Wh; the scenario needs a plan over the ceiling without it", got)
		}
		if short := plan.LoadpointShortfallWh["carport"]; short > 1 {
			t.Fatalf("without the ceiling the goal is %.0f Wh short; the scenario needs a goal the fuse alone allows", short)
		}
	})
}

// The bundled worker does not announce hourly_import_caps: Core rejects its
// plan when it overruns an hour and publishes its own.
func TestNativeImportCeilingFieldCase(t *testing.T) {
	worker := nativeWorker(t, 1500*time.Millisecond)
	t.Cleanup(func() { _ = worker.Close() })
	svc, start := importCeilingFieldService(t, worker, true)
	plan := checkImportCeilingFieldPlan(t, svc, start)
	if plan.Solver == nil || !plan.Solver.Fallback || !strings.Contains(plan.Solver.FallbackReason, "clock hour") {
		t.Fatalf("solver %+v; want Core's plan after the worker's overran an hour", plan.Solver)
	}
}

// A local clock hour lasts an hour on the 25-hour day: 02:00 comes twice.
func TestImportHoursOnTheLongDay(t *testing.T) {
	zone := stockholm(t)
	hourStart := time.Date(2026, 10, 25, 1, 0, 0, 0, zone) // CEST
	now := hourStart.Add(40 * time.Minute)
	slots := make([]Slot, 0, 14)
	for at := now.Truncate(15 * time.Minute); at.Before(hourStart.Add(4 * time.Hour)); at = at.Add(15 * time.Minute) {
		slots = append(slots, Slot{StartMs: at.UnixMilli(), LenMin: 15, LoadW: 500, PriceOre: 100})
	}
	slots[0].ExecutionStartMs = now.UnixMilli()
	hours := importHours(slots, ImportCeiling{W: 4500, MeanW: 4500, HourStart: hourStart, ElapsedWh: 3500})
	var got []string
	for _, h := range hours {
		got = append(got, time.UnixMilli(h.StartMs).In(zone).Format("15:04 MST"))
		if h.EndMs-h.StartMs != hourMs {
			t.Errorf("hour from %s lasts %d ms", got[len(got)-1], h.EndMs-h.StartMs)
		}
	}
	if want := "01:00 CEST,02:00 CEST,02:00 CET,03:00 CET"; strings.Join(got, ",") != want {
		t.Fatalf("hours %s, want %s", strings.Join(got, ","), want)
	}
	if hours[0].ElapsedImportWh != 3500 || hours[1].ElapsedImportWh != 0 || hours[2].ElapsedImportWh != 0 {
		t.Fatalf("elapsed %+v", hours)
	}
	// Each of the two 02:00 hours holds its own 4.5 kWh.
	caps, _ := importHourCapsW(slots, Params{ImportMeanW: 4500, ImportHours: hours})
	for i, s := range slots {
		want := 4500.0
		if s.StartMs < hourStart.Add(time.Hour).UnixMilli() {
			want = 1000 / (20. / 60) // what 01:00 has left over its last 20 minutes
		}
		if math.Abs(caps[i]-want) > 1e-6 {
			t.Errorf("slot %s: cap %.1f W, want %.1f W", time.UnixMilli(s.StartMs).In(zone).Format("15:04 MST"), caps[i], want)
		}
	}
}

// The hour in progress starts before the in-flight slot's execution and the
// last runs past the horizon: the plan's part of each may use what the hour
// has left, as Energyplan's hourly_import_caps count it.
func TestImportHoursAroundThePlan(t *testing.T) {
	start := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	slots := []Slot{
		{StartMs: start.Add(15 * time.Minute).UnixMilli(), ExecutionStartMs: start.Add(20 * time.Minute).UnixMilli(), LenMin: 15},
		{StartMs: start.Add(30 * time.Minute).UnixMilli(), LenMin: 15},
	}
	c := ImportCeiling{W: 4690, MeanW: 4000, HourStart: start, ElapsedWh: 1000}
	hours := importHours(slots, c)
	if len(hours) != 1 || hours[0].StartMs != start.UnixMilli() || hours[0].EndMs != start.Add(time.Hour).UnixMilli() || hours[0].ElapsedImportWh != 1000 {
		t.Fatalf("hours %+v", hours)
	}
	// 3 kWh left over the plan's 25 minutes of the hour.
	if caps, _ := importHourCapsW(slots, Params{ImportMeanW: c.MeanW, ImportHours: hours}); math.Abs(caps[0]-3000/(25./60)) > 1e-6 {
		t.Fatalf("caps %v", caps)
	}
	c.ElapsedWh = 5000 // overrun: only the house may import
	slots[1].LoadW = 600
	if caps, carCaps := importHourCapsW(slots, Params{ImportMeanW: c.MeanW, ImportHours: importHours(slots, c)}); caps[0] != 0 || caps[1] != 600 || !math.IsInf(carCaps[0], -1) || !math.IsInf(carCaps[1], -1) {
		t.Fatalf("overrun caps %v, with a car %v", caps, carCaps)
	}
	if importHours(slots, ImportCeiling{MeanW: 4000, HourStart: start}) != nil {
		t.Fatal("no ceiling gave hours")
	}
	if got := importHours(slots, ImportCeiling{W: 500, HourStart: start}); len(got) != 1 {
		t.Fatalf("a ceiling below the rise step gave %+v; want the hour with no room", got)
	}
}

// Each slot keeps its house import and shares the room left, never above
// the hour's mean, so the plan imports evenly as Core's live limit allows.
func TestImportHourCapsSpreadTheRoom(t *testing.T) {
	start := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	slots := make([]Slot, 4)
	for i, w := range []float64{4000, 500, 500, -2000} {
		slots[i] = Slot{StartMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), LenMin: 15, LoadW: max(0, w), PVW: min(0, w)}
	}
	p := Params{ImportMeanW: 3000, ImportHours: []ImportHour{{StartMs: start.UnixMilli(), EndMs: start.Add(time.Hour).UnixMilli()}}}
	caps, carCaps := importHourCapsW(slots, p)
	// The house takes 1.25 kWh and leaves 1.75 kWh: 1.75 kW on top of each
	// slot's house, never above the 3 kW mean. The 4 kW slot keeps its house,
	// but a car there gets only the mean.
	for i, want := range []float64{4000, 2250, 2250, 1750} {
		if math.Abs(caps[i]-want) > 1e-9 || math.Abs(carCaps[i]-min(want, 3000)) > 1e-9 {
			t.Errorf("slot %d: cap %.1f W, with a car %.1f W, want %.1f W", i, caps[i], carCaps[i], want)
		}
	}
	p.ImportMeanW = 500 // less than the house: no room for a car
	caps, carCaps = importHourCapsW(slots, p)
	for i, want := range []float64{4000, 500, 500, 0} {
		if math.Abs(caps[i]-want) > 1e-9 || !math.IsInf(carCaps[i], -1) {
			t.Errorf("overrun hour, slot %d: cap %.1f W, with a car %.1f W, want %.1f W and none", i, caps[i], carCaps[i], want)
		}
	}
}

func TestValidatePlanHoldsEachClockHourToItsBudget(t *testing.T) {
	start := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	slots := make([]Slot, 8)
	for i := range slots {
		slots[i] = Slot{StartMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), LenMin: 15, LoadW: 800, PriceOre: 100, SpotOre: 50}
	}
	p := Params{Mode: ModeArbitrage, CapacityWh: 10000, InitialSoC: .5, SoCMin: .1, SoCMax: .9, MaxChargeW: 5000, MaxDischargeW: 5000,
		ChargeEfficiency: 1, DischargeEfficiency: 1, ImportMeanW: 4500,
		ImportHours: []ImportHour{
			{StartMs: start.UnixMilli(), EndMs: start.Add(time.Hour).UnixMilli()},
			{StartMs: start.Add(time.Hour).UnixMilli(), EndMs: start.Add(2 * time.Hour).UnixMilli()},
		}}
	planWith := func(batteryW map[int]float64) Plan {
		var plan Plan
		soc := p.InitialSoC
		for i, s := range slots {
			b := batteryW[i]
			soc += b * .25 / p.CapacityWh
			a := Action{SlotStartMs: s.StartMs, SlotLenMin: 15, LoadW: s.LoadW, BatteryW: b, GridW: s.LoadW + b, SoC: soc}
			a.CostOre = reserveSlotCost(s, p, a)
			plan.TotalCostOre += a.CostOre
			plan.Actions = append(plan.Actions, a)
		}
		return plan
	}
	// 0.8 kW + 4.9 kW in one quarter is 5.7 kW, under the fuse, but the
	// hour's mean is 2.0 kW: inside the budget.
	if plan := planWith(map[int]float64{2: 4900}); ValidatePlan(slots, p, &plan) != nil {
		t.Fatal(ValidatePlan(slots, p, &plan))
	}
	// 3.9 kW of battery charge all through 10:00 makes a 4.7 kW mean.
	plan := planWith(map[int]float64{4: 3900, 5: 3900, 6: 3900, 7: 3900})
	if err := ValidatePlan(slots, p, &plan); err == nil || !strings.Contains(err.Error(), "clock hour") {
		t.Fatalf("hour of 4.7 kW mean passed: %v", err)
	}
	// The house alone may overrun an hour that has used its budget.
	p.ImportHours[0].ElapsedImportWh = 4400
	if plan := planWith(nil); ValidatePlan(slots, p, &plan) != nil {
		t.Fatal(ValidatePlan(slots, p, &plan))
	}
	if plan := planWith(map[int]float64{0: 1000}); ValidatePlan(slots, p, &plan) == nil {
		t.Fatal("battery charge into an overrun hour passed")
	}
}

// A car pulse imports while it runs even when the slot exports between.
func TestValidatePlanCountsPulseImport(t *testing.T) {
	start := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	slots := make([]Slot, 4)
	for i := range slots {
		slots[i] = Slot{StartMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), LenMin: 15, LoadW: 500, PVW: -4500, PriceOre: 100, SpotOre: 50}
	}
	lp := &LoadpointSpec{ID: "car", CapacityWh: 60000, Levels: 11, SoCMax: 1, InitialSoC: .5, PluggedIn: true,
		MaxChargeW: 11000, AllowedStepsW: []float64{0, 11000}, ChargeEfficiency: 1}
	p := Params{Mode: ModeArbitrage, ChargeEfficiency: 1, DischargeEfficiency: 1, Loadpoint: lp, ImportMeanW: 1000,
		ImportHours: []ImportHour{{StartMs: start.UnixMilli(), EndMs: start.Add(time.Hour).UnixMilli()}}}
	var plan Plan
	soc := lp.InitialSoC
	for _, s := range slots {
		// 4 kW mean: 11 kW for 4/11 of the slot imports 7 kW, else 4 kW export.
		soc += 4000 * .25 / lp.CapacityWh
		a := Action{SlotStartMs: s.StartMs, SlotLenMin: 15, LoadW: s.LoadW, PVW: s.PVW, GridW: 0, LoadpointW: 4000, LoadpointSoC: soc,
			LoadpointPowerW: map[string]float64{"car": 4000}, LoadpointSoCByID: map[string]float64{"car": soc}, LoadpointMaxPowerW: map[string]float64{"car": 11000}}
		a.CostOre = reserveSlotCost(s, p, a)
		plan.TotalCostOre += a.CostOre
		plan.Actions = append(plan.Actions, a)
	}
	// Mean grid flow is 0 W, but the pulses import 7 kW × 4/11 ≈ 2.5 kWh.
	if err := ValidatePlan(slots, p, &plan); err == nil || !strings.Contains(err.Error(), "clock hour") {
		t.Fatalf("pulses importing 2.5 kWh passed a 1 kWh hour: %v", err)
	}
}

// The DP charges the battery in the cheap hour only as far as the hour's
// budget leaves room, and the plan passes the same check.
func TestOptimizeKeepsEachHourUnderItsBudget(t *testing.T) {
	start := time.Date(2026, 10, 26, 1, 0, 0, 0, time.UTC)
	slots := make([]Slot, 8)
	for i := range slots {
		price := 20.0
		if i >= 4 {
			price = 300
		}
		slots[i] = Slot{StartMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), LenMin: 15, LoadW: 800, PriceOre: price, SpotOre: price / 2}
	}
	p := Params{Mode: ModeArbitrage, SoCLevels: 41, ActionLevels: 41, CapacityWh: 20000, InitialSoC: .2, SoCMin: .1, SoCMax: .95,
		MaxChargeW: 8000, MaxDischargeW: 8000, ChargeEfficiency: .95, DischargeEfficiency: .95}
	free := Optimize(slots, p)
	if got := planImportWh(free, start.UnixMilli(), start.Add(time.Hour).UnixMilli()); got < 6000 {
		t.Fatalf("without a budget the cheap hour imports %.0f Wh; the test needs it over 3 kWh", got)
	}
	p.ImportMeanW, p.ImportHours = 3000, []ImportHour{{StartMs: start.UnixMilli(), EndMs: start.Add(time.Hour).UnixMilli()}}
	plan := Optimize(slots, p)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	// The 400 W action grid reaches 2.8 of the 3 kWh.
	if got := planImportWh(plan, start.UnixMilli(), start.Add(time.Hour).UnixMilli()); got > 3000+1e-6 || got < 2700 {
		t.Fatalf("cheap hour imports %.0f Wh; want the room of its 3000 Wh used", got)
	}
}

type importCapTransport struct{ caps bool }

func (c *importCapTransport) RoundTrip(context.Context, []byte) ([]byte, error) {
	features := []string{"champion", "ev_duty", "published_prices"}
	if c.caps {
		features = append(features, "hourly_import_caps")
	}
	return json.Marshal(map[string]any{"name": "ftw-solver", "version": "test", "protocol_version": 1, "features": features})
}
func (*importCapTransport) Close() error { return nil }

// A worker that announces hourly_import_caps gets each hour, the mean and
// what the hour in progress had used. Any other worker, and every worker
// while the ceiling is off, gets the request it got before.
func TestEnergyplanRequestCarriesHourlyImportCaps(t *testing.T) {
	slots, p := externalTestFixture()
	transport := &importCapTransport{}
	external := &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: 7 * time.Second}, transport: transport, timeBudget: energyplanTimeBudget}
	o := &EnergyplanOptimizer{ExternalOptimizer: external}
	request := func(p Params) string {
		t.Helper()
		r := external.buildRequest(slots, p)
		r.RequestID = "fixed"
		if err := o.prepareEnergyplanRequest(context.Background(), &r, slots, p); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	off := p
	on := p
	on.ImportMeanW = 3810
	// The third hour lies past the slots: it is not sent.
	on.ImportHours = []ImportHour{{StartMs: 0, EndMs: hourMs, ElapsedImportWh: 125}, {StartMs: hourMs, EndMs: 2 * hourMs}, {StartMs: 3 * hourMs, EndMs: 4 * hourMs}}
	for _, caps := range []bool{true, false} {
		transport.caps = caps
		if request(on) == request(off) != !caps {
			t.Fatalf("caps=%v: with the ceiling on the request changes %v", caps, request(on) != request(off))
		}
	}
	transport.caps = true
	if got, want := request(on), `"hourly_import_caps":[{"start_ms":0,"end_ms":3600000,"max_mean_w":3810,"elapsed_import_wh":125},{"start_ms":3600000,"end_ms":7200000,"max_mean_w":3810,"elapsed_import_wh":0}]`; !strings.Contains(got, want) {
		t.Fatalf("request %s\nwant %s", got, want)
	}
	// With the hours the worker gets the fleet budget; without them its own.
	if got := request(on); !strings.Contains(got, `"time_limit_s":5,`) || !strings.Contains(request(off), `"time_limit_s":0.5,`) {
		t.Fatalf("time limits: with hours %s; without %s", got, request(off))
	}
	if strings.Contains(request(off), "hourly_import_caps") {
		t.Fatal("the ceiling off sent caps")
	}
}

// ceilinglessOptimizer plans as a worker without the hours does: its plan
// keeps every rule but the hours, and Core's check names the hour.
type ceilinglessOptimizer struct{}

func (ceilinglessOptimizer) Optimize(ctx context.Context, slots []Slot, p Params) (Plan, error) {
	free := p
	free.ImportHours, free.ImportMeanW = nil, 0
	plan, err := coreReservePlan(ctx, slots, free)
	if err != nil {
		return Plan{}, err
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		return Plan{}, fmt.Errorf("optimizer plan rejected: %w", err)
	}
	return plan, nil
}

func (ceilinglessOptimizer) Close() error { return nil }

// A worker plan that overruns an hour falls back to Core's own plan. When
// Core cannot plan the site itself, here a battery below its floor, the
// worker's plan is published as before the hours, with a warning, rather
// than no plan at all.
func TestImportCeilingWorkerPlanOverrunsAnHour(t *testing.T) {
	for _, soc := range []float64{.5, .05} {
		t.Run(fmt.Sprintf("battery %.0f %%", soc*100), func(t *testing.T) {
			svc, start := importCeilingFieldService(t, ceilinglessOptimizer{}, true)
			svc.Tele.Update("sungrow", telemetry.DerBattery, 0, &soc, nil)
			if svc.Replan(context.Background()) == nil {
				t.Fatalf("no plan published: %+v", svc.PlanSnapshot())
			}
			snap := svc.PlanSnapshot()
			got := planImportWh(*snap.Plan, start.UnixMilli(), start.Add(time.Hour).UnixMilli())
			warned := strings.Contains(strings.Join(snap.Warnings, " "), "Core cannot plan this site itself")
			if soc == .5 {
				if !snap.Plan.Solver.Fallback || got > 4155 || warned {
					t.Fatalf("solver %+v, 09:00 imports %.0f Wh, warned %v; want Core's plan inside the hour", snap.Plan.Solver, got, warned)
				}
				return
			}
			if snap.Plan.Solver.Fallback || got <= 4155 || !warned {
				t.Fatalf("solver %+v, 09:00 imports %.0f Wh, warnings %q; want the worker's plan with a warning", snap.Plan.Solver, got, snap.Warnings)
			}
		})
	}
}

func TestNativeImportCeilingWhenCoreCannotPlan(t *testing.T) {
	worker := nativeWorker(t, 1500*time.Millisecond)
	t.Cleanup(func() { _ = worker.Close() })
	svc, _ := importCeilingFieldService(t, worker, true)
	soc := .05
	svc.Tele.Update("sungrow", telemetry.DerBattery, 0, &soc, nil)
	if svc.Replan(context.Background()) == nil {
		t.Fatalf("no plan published: %+v", svc.PlanSnapshot())
	}
	if w := strings.Join(svc.PlanSnapshot().Warnings, " "); !strings.Contains(w, "Core cannot plan this site itself") {
		t.Fatalf("warnings %q", w)
	}
}

// A car step that needs the battery to discharge past the house fits the
// fuse only while it runs. When the hour makes it part-time, the battery
// would drive export beside the car: the reserve plan takes a smaller step.
func TestReservePlanUnderAnHourNeverChargesTheCarFromExport(t *testing.T) {
	first := time.Date(2026, 10, 26, 10, 40, 0, 0, time.UTC)
	for _, fuse := range []float64{6900, 8280} {
		slots := make([]Slot, 12)
		for i := range slots {
			slots[i] = Slot{StartMs: first.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), LenMin: 15, LoadW: 500, PriceOre: 100, SpotOre: 50,
				Limits: PowerLimits{MaxImportW: fuse, MaxExportW: fuse}}
		}
		p := Params{Mode: ModeArbitrage, SoCLevels: 21, ActionLevels: 21, CapacityWh: 10000, InitialSoC: .8, SoCMin: .1, SoCMax: .95,
			MaxChargeW: 5000, MaxDischargeW: 5000, ChargeEfficiency: .95, DischargeEfficiency: .95,
			Loadpoint: &LoadpointSpec{ID: "car", CapacityWh: 60000, InitialSoC: .3, SoCMax: 1, Levels: 21, PluggedIn: true, MinChargeW: 4140, MaxChargeW: 11040,
				AllowedStepsW: []float64{0, 4140, 6900, 11040}, ChargeEfficiency: .9, TargetSoC: .5, TargetSlotIdx: 10}}
		c := ImportCeiling{W: 4500, MeanW: 4155, HourStart: first.Add(-40 * time.Minute), ElapsedWh: 3800}
		p.ImportHours, p.ImportMeanW = importHours(slots, c), c.MeanW
		if _, used, err := corePlanWithRecovery(context.Background(), slots, p); err != nil || len(used) != len(slots) {
			t.Errorf("fuse %.0f W: %d of %d slots, err %v", fuse, len(used), len(slots), err)
		}
	}
}

// The house may overrun an hour that has used its budget, but a car may not
// charge there on the import the battery frees by covering the house: Core's
// live limit stops the car. The plan holds the car to the next hour.
func TestNoCarOnTheHouseFloor(t *testing.T) {
	start := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	slots := make([]Slot, 8)
	for i := range slots {
		slots[i] = Slot{StartMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(), LenMin: 15, LoadW: 800, PriceOre: 100, SpotOre: 50,
			Limits: PowerLimits{MaxImportW: 13800, MaxExportW: 13800}}
	}
	lp := &LoadpointSpec{ID: "car", CapacityWh: 60000, InitialSoC: .3, SoCMax: 1, Levels: 21, PluggedIn: true, MinChargeW: 4140, MaxChargeW: 11040,
		AllowedStepsW: []float64{0, 4140, 11040}, ChargeEfficiency: .9, TargetSoC: .5, TargetSlotIdx: 7}
	p := Params{Mode: ModeArbitrage, SoCLevels: 21, ActionLevels: 21, CapacityWh: 10000, InitialSoC: .8, SoCMin: .1, SoCMax: .95,
		MaxChargeW: 5000, MaxDischargeW: 5000, ChargeEfficiency: 1, DischargeEfficiency: 1, Loadpoint: lp, ImportMeanW: 4155,
		ImportHours: []ImportHour{
			{StartMs: start.UnixMilli(), EndMs: start.Add(time.Hour).UnixMilli(), ElapsedImportWh: 4000},
			{StartMs: start.Add(time.Hour).UnixMilli(), EndMs: start.Add(2 * time.Hour).UnixMilli()},
		}}
	plan, used, err := corePlanWithRecovery(context.Background(), slots, p)
	if err != nil || len(used) != len(slots) {
		t.Fatalf("%d of %d slots, err %v", len(used), len(slots), err)
	}
	for i, a := range plan.Actions {
		if i < 4 && a.LoadpointW > 0 {
			t.Errorf("slot %d: car at %.0f W in the hour that has 155 Wh left for 800 W of house", i, a.LoadpointW)
		}
	}
	if plan.Actions[4].LoadpointW <= 0 {
		t.Errorf("the car waits past 10:00: %+v", plan.Actions[4])
	}
	// The same car beside a battery covering the house fails the check.
	a := &plan.Actions[0]
	a.BatteryW, a.LoadpointW = -800, 800
	a.LoadpointPowerW, a.LoadpointMaxPowerW = map[string]float64{"car": 800}, map[string]float64{"car": 4140}
	err = validateImportHours(slots, p, plan.Actions)
	if err == nil || !strings.Contains(err.Error(), "clock hour") {
		t.Fatalf("car on the house floor passed: %v", err)
	}
}

// The DP plans on a SoC grid. Replayed from the real SoC, the battery can run
// short of the discharge that kept an hour under its budget, here at 12:45
// in the field case: the car gives way rather than the hour overrunning.
func TestOptimizeCarGivesWayWhenTheBatteryRunsShort(t *testing.T) {
	svc, _ := importCeilingFieldService(t, nil, true)
	if svc.Replan(context.Background()) == nil {
		t.Fatal("no plan published")
	}
	svc.mu.RLock()
	slots, p := append([]Slot(nil), svc.lastSlots...), svc.lastParams
	svc.mu.RUnlock()
	plan := Optimize(slots, p)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
}
