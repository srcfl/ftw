package mpc

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// swedishWinterDay is a dark day priced as a Swedish household pays it: a
// 66 öre grid tariff (36 öre of it energy tax) and 25 % VAT on import, spot
// on export. The night is cheap and the evening dear, but not by enough to
// buy, store and sell without the refund.
func swedishWinterDay(loadW float64) []Slot {
	slots := make([]Slot, 24)
	for h := range slots {
		spot := 60.0
		switch {
		case h < 6:
			spot = 20
		case h >= 17 && h <= 20:
			spot = 110
		}
		slots[h] = Slot{StartMs: int64(h) * 3_600_000, LenMin: 60,
			SpotOre: spot, PriceOre: (spot + 66) * 1.25, LoadW: loadW}
	}
	return slots
}

func refundParams(mode Mode) Params {
	p := baseParams(mode)
	p.SoCLevels, p.ActionLevels = 86, 101
	p.InitialSoC = p.SoCMin
	p.BatteryReturnOreKwh = 36.0 - 9.6
	return p
}

func exportedWh(plan Plan) float64 {
	var wh float64
	for _, a := range plan.Actions {
		end := a.SlotStartMs + int64(a.SlotLenMin)*60_000
		wh += max(0, -a.GridW) * float64(end-a.ExecutionStart()) / 3.6e6
	}
	return wh
}

func returnedWh(plan Plan) float64 {
	var wh float64
	for _, a := range plan.Actions {
		wh += a.BatteryReturnWh
	}
	return wh
}

func TestRefundMakesGridArbitragePay(t *testing.T) {
	slots := swedishWinterDay(0)
	with := refundParams(ModeArbitrage)
	without := with
	without.BatteryReturnOreKwh = 0

	idle := Optimize(slots, without)
	if exportedWh(idle) > 1 || idle.BatteryReturnOre != 0 {
		t.Fatalf("without the refund the spread does not pay: exported %.0f Wh, refund %.1f", exportedWh(idle), idle.BatteryReturnOre)
	}
	plan := Optimize(slots, with)
	if exportedWh(plan) < 5000 {
		t.Fatalf("with the refund the battery should buy at night and sell at the peak; exported %.0f Wh", exportedWh(plan))
	}
	// The year-start battery holds 1 kWh of other energy, so not every
	// kWh sent back is grid energy, and every kWh returned left the site.
	ret := returnedWh(plan)
	if ret <= 0 || ret >= exportedWh(plan) {
		t.Fatalf("returned %.0f Wh of %.0f Wh exported", ret, exportedWh(plan))
	}
	if math.Abs(plan.BatteryReturnOre-with.BatteryReturnOreKwh*ret/1000) > 1e-6 {
		t.Fatalf("refund %.2f öre for %.0f Wh", plan.BatteryReturnOre, ret)
	}
	gain := idle.TotalCostOre - plan.TotalCostOre + plan.BatteryReturnOre
	t.Logf("exported %.0f Wh, returned %.0f Wh, cost %.1f öre, refund %.1f öre, gain %.1f öre",
		exportedWh(plan), ret, plan.TotalCostOre, plan.BatteryReturnOre, gain)
	if gain <= 0 {
		t.Fatalf("the plan should gain with the refund: cost %.1f → %.1f, refund %.1f", idle.TotalCostOre, plan.TotalCostOre, plan.BatteryReturnOre)
	}
}

func TestRefundNeverCountsStoredSolar(t *testing.T) {
	// A battery full of solar sells at the peak with or without the refund.
	// None of it is grid energy, and the night is too dear to refill.
	slots := swedishWinterDay(0)
	for h := range 6 {
		slots[h].SpotOre, slots[h].PriceOre = 100, (100+66)*1.25
	}
	with := refundParams(ModeArbitrage)
	with.InitialSoC = with.SoCMax
	without := with
	without.BatteryReturnOreKwh = 0
	a, b := Optimize(slots, with), Optimize(slots, without)
	if a.BatteryReturnOre != 0 || returnedWh(a) != 0 {
		t.Fatalf("stored solar counted as returned grid energy: %.0f Wh", returnedWh(a))
	}
	for i := range a.Actions {
		if a.Actions[i].BatteryW != b.Actions[i].BatteryW {
			t.Fatalf("slot %d: %0.f W with the refund, %.0f W without", i, a.Actions[i].BatteryW, b.Actions[i].BatteryW)
		}
	}
}

func TestRefundCountsTheGridShareTheBatteryHolds(t *testing.T) {
	slots := swedishWinterDay(0)
	p := refundParams(ModeArbitrage)
	p.InitialSoC = p.SoCMax
	p.BatteryReturnGridShare = 0.5
	plan := Optimize(slots, p)
	// Half of what the battery can give back is other energy, and it goes
	// to the grid first.
	other := 0.5 * p.SoCMax * p.CapacityWh * p.DischargeEfficiency
	if got, want := returnedWh(plan), exportedWh(plan)-other; exportedWh(plan) < 5000 || math.Abs(got-want) > 1 {
		t.Fatalf("returned %.0f Wh of %.0f Wh exported, want %.0f", got, exportedWh(plan), want)
	}
}

func TestOnlyActiveArbitrageCountsTheRefund(t *testing.T) {
	slots := swedishWinterDay(1500)
	for _, mode := range []Mode{ModePassiveArbitrage, ModeSelfConsumption, ModeCheapCharge} {
		with := refundParams(mode)
		with.BatteryReturnGridShare = 1
		without := with
		without.BatteryReturnOreKwh = 0
		a, b := Optimize(slots, with), Optimize(slots, without)
		if a.BatteryReturnOre != 0 || with.batteryReturnShareLevels() != 1 {
			t.Fatalf("%s counted a refund of %.1f öre", mode, a.BatteryReturnOre)
		}
		for i := range a.Actions {
			if a.Actions[i].BatteryW != b.Actions[i].BatteryW {
				t.Fatalf("%s slot %d: the refund changed the plan", mode, i)
			}
		}
	}
}

func TestReturnFlowsCountSolarFirst(t *testing.T) {
	for _, c := range []struct {
		name               string
		pvW, battW, gridW  float64
		toBattery, toGridW float64
	}{
		{"grid charge in the dark", 0, 3000, 4000, 3000, 0},
		{"solar covers part of the charge", -1000, 3000, 2500, 2000, 0},
		{"solar could have covered all of it", -4000, 3000, 500, 0, 0},
		{"import caps the grid's part", 0, 3000, 1000, 1000, 0},
		{"export beyond the house", 0, -3000, -2000, 0, 2000},
		{"discharge to the house only", 0, -3000, 500, 0, 0},
		{"solar is exported first", -1500, -3000, -2500, 0, 1000},
		{"idle", -1500, 0, -1000, 0, 0},
	} {
		in, out := returnFlowsW(Slot{PVW: c.pvW}, c.battW, c.gridW)
		if in != c.toBattery || out != c.toGridW {
			t.Errorf("%s: %.0f W in, %.0f W out; want %.0f, %.0f", c.name, in, out, c.toBattery, c.toGridW)
		}
	}
}

func TestPlanRefundIsTheSameForEveryEngine(t *testing.T) {
	slots := swedishWinterDay(0)
	p := refundParams(ModeArbitrage)
	plan := Optimize(slots, p)
	want := plan.BatteryReturnOre
	copied := plan
	copied.Actions = append([]Action(nil), plan.Actions...)
	for i := range copied.Actions {
		copied.Actions[i].BatteryReturnWh = 0
	}
	annotateBatteryReturn(&copied, slots, p)
	if want <= 0 || math.Abs(copied.BatteryReturnOre-want) > 1e-9 {
		t.Fatalf("annotated %.3f öre, DP %.3f", copied.BatteryReturnOre, want)
	}
}

// BenchmarkCoreDPBatteryReturn is the release grid over 48 h of quarters,
// without and with the refund's grid-share state.
func BenchmarkCoreDPBatteryReturn(b *testing.B) {
	day := swedishWinterDay(800)
	slots := make([]Slot, 193)
	for i := range slots {
		s := day[(i/4)%24]
		s.StartMs, s.LenMin = int64(i)*900_000, 15
		slots[i] = s
	}
	for _, c := range []struct {
		name   string
		credit float64
		soc    int
		act    int
	}{{"off", 0, 201, 401}, {"on", 26.4, 201, 401}, {"on-derated", 26.4, 101, 201}} {
		b.Run(c.name, func(b *testing.B) {
			p := refundParams(ModeArbitrage)
			p.BatteryReturnOreKwh, p.SoCLevels, p.ActionLevels = c.credit, c.soc, c.act
			for b.Loop() {
				Optimize(slots, p)
			}
		})
	}
}

// TestServiceCountsTheRefundFromItsHook: Core asks for the refund over the
// plan's window and starts from the grid share History last counted.
func TestServiceCountsTheRefundFromItsHook(t *testing.T) {
	start := time.Date(2026, 11, 2, 17, 0, 0, 0, stockholm(t))
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	prices := make([]state.PricePoint, 16)
	for i := range prices {
		prices[i] = state.PricePoint{Zone: "SE3", SlotTsMs: start.Add(time.Duration(i) * 15 * time.Minute).UnixMilli(),
			SlotLenMin: 15, SpotOreKwh: 110, TotalOreKwh: (110 + 61) * 1.25, Source: "test", FetchedAtMs: start.UnixMilli()}
	}
	if err := st.SavePrices(prices); err != nil {
		t.Fatal(err)
	}
	run := func(hook bool) (Plan, [2]time.Time) {
		svc := New(st, nil, "SE3", Params{
			Mode: ModeArbitrage, SoCLevels: 21, ActionLevels: 21, TerminalSoCPrice: 1,
			CapacityWh: 10000, InitialSoC: .9, SoCMin: .1, SoCMax: .95,
			MaxChargeW: 5000, MaxDischargeW: 5000, ChargeEfficiency: .95, DischargeEfficiency: .95,
		})
		svc.Horizon = 4 * time.Hour
		svc.BaseLoad = 500
		svc.now = func() time.Time { return start.Add(30 * time.Second) }
		tel := telemetry.NewStore()
		soc := .9
		tel.Update("sungrow", telemetry.DerBattery, 0, &soc, nil)
		tel.DriverHealthMut("sungrow").RecordSuccess()
		svc.Tele = tel
		svc.UpdateBatteryFleet([]BatteryFleetMember{{Driver: "sungrow", CapacityWh: 10000, MaxChargeW: 5000, MaxDischargeW: 5000}}, 10000, 5000, 5000)
		svc.UpdateSiteEconomics(SiteEconomics{FuseMaxW: 13800, Timezone: "Europe/Stockholm"})
		var window [2]time.Time
		if hook {
			svc.BatteryReturn = func(from, to time.Time) BatteryReturnState {
				window = [2]time.Time{from, to}
				return BatteryReturnState{OreKwh: 26.4, GridWh: 7000}
			}
		}
		if svc.Replan(context.Background()) == nil {
			t.Fatalf("no plan published: %+v", svc.PlanSnapshot())
		}
		return *svc.PlanSnapshot().Plan, window
	}
	without, _ := run(false)
	plan, window := run(true)
	if without.BatteryReturnOre != 0 {
		t.Fatalf("no hook, yet a refund of %.1f öre", without.BatteryReturnOre)
	}
	if !window[0].Equal(start.Add(30*time.Second)) || !window[1].Equal(start.Add(4*time.Hour)) {
		t.Fatalf("asked for %v..%v", window[0], window[1])
	}
	// 9 kWh in the cells give back 8.55 kWh, 7 of it grid energy. Replay
	// the plan from that share: it must be what the service started from.
	p := Params{Mode: ModeArbitrage, CapacityWh: 10000, InitialSoC: .9, ChargeEfficiency: .95, DischargeEfficiency: .95,
		BatteryReturnOreKwh: 26.4, BatteryReturnGridShare: 7000 / 8550.}
	slots := make([]Slot, len(plan.Actions))
	for i, a := range plan.Actions {
		slots[i] = Slot{StartMs: a.SlotStartMs, LenMin: a.SlotLenMin, ExecutionStartMs: a.ExecutionStartMs, PVW: a.PVW, LoadW: a.LoadW}
	}
	if exportedWh(plan) < 3000 || returnedWh(plan) <= 0 || returnedWh(plan) > exportedWh(plan) {
		t.Fatalf("returned %.0f Wh of %.0f Wh exported", returnedWh(plan), exportedWh(plan))
	}
	if want := walkBatteryReturn(plan.Actions, slots, p, nil); math.Abs(plan.BatteryReturnOre-want) > 1e-6 {
		t.Fatalf("refund %.2f öre, replayed from the hook's start %.2f", plan.BatteryReturnOre, want)
	}
	if math.Abs(plan.BatteryReturnOre-26.4*returnedWh(plan)/1000) > 1e-6 {
		t.Fatalf("refund %.2f öre", plan.BatteryReturnOre)
	}
}

func TestAPluggedInCarLeavesTheRefundOutOfTheDP(t *testing.T) {
	slots := swedishWinterDay(0)
	with := refundParams(ModeArbitrage)
	with.Loadpoint = &LoadpointSpec{ID: "car", CapacityWh: 60000, InitialSoC: .5, SoCMax: 1, Levels: 11,
		PluggedIn: true, MaxChargeW: 11000, AllowedStepsW: []float64{0, 4140, 11000}, ChargeEfficiency: .9}
	if with.batteryReturnShareLevels() != 1 {
		t.Fatal("a plugged-in car keeps the DP to one grid-share level")
	}
	without := with
	without.BatteryReturnOreKwh = 0
	a, b := Optimize(slots, with), Optimize(slots, without)
	for i := range a.Actions {
		if a.Actions[i].BatteryW != b.Actions[i].BatteryW || a.Actions[i].LoadpointW != b.Actions[i].LoadpointW {
			t.Fatalf("slot %d: the refund changed the plan", i)
		}
	}
}

func TestTheHouseUsesGridEnergyFirstAndTheGridGetsOtherFirst(t *testing.T) {
	step := func(share, storedWh, pvW, battW, gridW float64) (float64, float64) {
		return returnMixStep(Slot{PVW: pvW}, battW, gridW, storedWh, share, 1, 1, 1)
	}
	// 5 kWh, 40 % grid. 3 kW out, 1 kW of it for the house: the house takes
	// grid energy, the grid other energy, so nothing is returned.
	if share, ret := step(0.4, 5000, 0, -3000, -2000); math.Abs(share-0.5) > 1e-12 || ret != 0 {
		t.Fatalf("share %.3f returned %.0f Wh", share, ret)
	}
	// 80 % grid: once the 1 kWh of other energy is gone the grid gets grid
	// energy.
	if share, ret := step(0.8, 5000, 0, -3000, -2000); math.Abs(share-1) > 1e-12 || math.Abs(ret-1000) > 1e-9 {
		t.Fatalf("share %.3f returned %.0f Wh", share, ret)
	}
	// Charging 2 kW with 1 kW of sun: 1 kW of grid energy enters after the
	// round-trip loss, the sun's whole, and what does not fit in the battery
	// sheds grid energy.
	share, ret := returnMixStep(Slot{PVW: -1000}, 2000, 1000, 5000, 0.4, 1, 0.95, 0.95)
	held, after := 5000*0.95, 5000*0.95+2000*0.95*0.95
	grid := 0.4*held + 1000*0.95*0.95
	other := 0.6*held + 1000
	if want := (grid - (grid + other - after)) / after; ret != 0 || math.Abs(share-want) > 1e-12 {
		t.Fatalf("share %.4f, want %.4f", share, want)
	}
	// History's case: 5 kWh from the grid at night, 5 kWh to the house in
	// the morning, 5 kWh of sun at midday, 5 kWh to the grid in the evening
	// returns nothing.
	f, stored, total := 0.0, 0.0, 0.0
	for _, h := range []struct{ pvW, battW, gridW float64 }{
		{0, 5000, 5000}, {0, -5000, 0}, {-5000, 5000, 0}, {0, -5000, -5000},
	} {
		var r float64
		f, r = step(f, stored, h.pvW, h.battW, h.gridW)
		stored += h.battW
		total += r
	}
	if total != 0 {
		t.Fatalf("returned %.0f Wh of grid energy the house had used", total)
	}
}

func TestStartShareCountsLow(t *testing.T) {
	for _, c := range []struct {
		name     string
		b        BatteryReturnState
		storedWh float64
		want     float64
	}{
		{"as History counted", BatteryReturnState{GridWh: 4000, OtherWh: 2000}, 8000, 0.5},
		{"given out since: grid energy first", BatteryReturnState{GridWh: 4000, OtherWh: 2000, DischargedSinceWh: 1000}, 8000, 3000. / 8000},
		{"taken in since: other energy", BatteryReturnState{GridWh: 4000, OtherWh: 2000, ChargedSinceWh: 3000}, 8000, 3000. / 8000},
		{"less than History holds: all its other energy is there", BatteryReturnState{GridWh: 4000, OtherWh: 2000}, 5000, 0.6},
		{"empty", BatteryReturnState{GridWh: 4000}, 0, 0},
		{"nothing counted", BatteryReturnState{}, 8000, 0},
	} {
		if got := c.b.startShare(c.storedWh, 1); math.Abs(got-c.want) > 1e-12 {
			t.Errorf("%s: share %.4f, want %.4f", c.name, got, c.want)
		}
	}
}
