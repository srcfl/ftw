package control

import (
	"math"
	"testing"
	"time"
)

func TestPartialChargeKeepsIntentAndHasNoCatchupSpike(t *testing.T) {
	start := time.Date(2026, 9, 8, 4, 45, 0, 0, time.UTC)
	for _, remaining := range []time.Duration{151061 * time.Millisecond, 5 * time.Second} {
		dir := SlotDirective{PriceSlotStart: start, SlotStart: start.Add(15*time.Minute - remaining), SlotEnd: start.Add(15 * time.Minute), BatteryEnergyWh: 2000 * remaining.Hours(), PlannedGridW: 2500, HasPlannedGridW: true}
		st := newStateWithEnergyDispatch(dir, "battery")
		now := dir.SlotStart.Add(time.Second)
		st.clock = func() time.Time { return now }
		st.MinDispatchIntervalS = 0
		st.SlewEnabled = false
		store := seedStore(500, []struct {
			name          string
			currentW, soc float64
		}{{"battery", 0, .5}})
		targets := ComputeDispatch(store, st, caps(map[string]float64{"battery": 9600}), 11040)
		if len(targets) != 1 || math.Abs(targets[0].TargetW-2000) > .001 {
			t.Fatalf("remaining=%v targets=%+v", remaining, targets)
		}
		snapshot := st.SlotEnergy()
		if math.Abs(snapshot.EnergyPathWh-2000./3600) > 1e-8 || snapshot.ActualWh != 0 || snapshot.PlannedSoFarWh != 0 {
			t.Fatalf("elapsed-time credit entered observed energy: %+v", snapshot)
		}
	}
}

func TestPublicationDelayCreditStaysOutOfMeasuredEnergy(t *testing.T) {
	start := time.Date(2026, 9, 8, 4, 45, 0, 0, time.UTC)
	dir := SlotDirective{PriceSlotStart: start, SlotStart: start.Add(12 * time.Minute), SlotEnd: start.Add(15 * time.Minute), BatteryEnergyWh: 100, PlannedGridW: 2500, HasPlannedGridW: true}
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	now := dir.SlotStart.Add(30 * time.Second)
	st.clock = func() time.Time { return now }
	st.SlewEnabled = false
	store := seedStore(500, []struct {
		name          string
		currentW, soc float64
	}{{"battery", 900, .5}})
	ComputeDispatch(store, st, caps(map[string]float64{"battery": 9600}), 11040)
	first := st.SlotEnergy()
	if math.Abs(first.EnergyPathWh-2000*30./3600) > 1e-8 || first.ActualWh != 0 || first.PlannedSoFarWh != 0 {
		t.Fatalf("publication delay was counted as measured energy: %+v", first)
	}
	now = now.Add(6 * time.Second)
	ComputeDispatch(store, st, caps(map[string]float64{"battery": 9600}), 11040)
	next := st.SlotEnergy()
	if math.Abs(next.ActualWh-900*6./3600) > 1e-8 || math.Abs(next.PlannedSoFarWh-2000*6./3600) > 1e-8 {
		t.Fatalf("observed energy includes unobserved time: %+v", next)
	}
}

func TestSlotMetricsWeightsAcceptedDecisionsWithinPriceSlot(t *testing.T) {
	start := time.Date(2026, 9, 8, 3, 15, 0, 0, time.UTC)
	active := SlotDirective{PriceSlotStart: start, SlotStart: start, SlotEnd: start.Add(15 * time.Minute), BatteryEnergyWh: -400 * .25}
	st := makeSlotMetricsState("battery", func(time.Time) (SlotDirective, bool) { return active, true })
	// Bounded one-minute ticks avoid hiding a long measurement gap.
	updateSlotDeliveryMetrics(st, -400, start)
	for i := 1; i <= 12; i++ {
		updateSlotDeliveryMetrics(st, -400, start.Add(time.Duration(i)*time.Minute))
	}
	at := start.Add(750 * time.Second)
	active = SlotDirective{PriceSlotStart: start, SlotStart: at, SlotEnd: start.Add(15 * time.Minute), BatteryEnergyWh: 600 * 150. / 3600}
	updateSlotDeliveryMetrics(st, -400, at)
	for _, sec := range []int{780, 810, 840, 870} {
		updateSlotDeliveryMetrics(st, 600, start.Add(time.Duration(sec)*time.Second))
	}
	if math.Abs(st.slotActualPlannedWh-(-400*750./3600+600*150./3600)) > 1e-8 {
		t.Fatal("latest decision overwrote prior intervals", st.slotActualPlannedWh)
	}
	snapshot := st.SlotEnergy()
	wantPast := -400*750./3600 + 600*120./3600
	if math.Abs(snapshot.PlannedSoFarWh-wantPast) > 1e-8 || math.Abs(snapshot.ActualWh-wantPast) > 1e-8 {
		t.Fatalf("snapshot did not preserve observed decisions: %+v", snapshot)
	}
	active = SlotDirective{SlotStart: start.Add(15 * time.Minute), SlotEnd: start.Add(30 * time.Minute), BatteryEnergyWh: -100}
	updateSlotDeliveryMetrics(st, 600, start.Add(15*time.Minute))
	if st.SlotDeliveryStats.SignMismatchCount != 0 || st.SlotDeliveryStats.UnderDeliveryCount != 0 || st.SlotDeliveryStats.OverDeliveryCount != 0 {
		t.Fatalf("followed decisions reported as mismatch: %+v", st.SlotDeliveryStats)
	}
}

func TestChargeToCoverLoadSignGuardMatches0500(t *testing.T) {
	start := time.Now()
	dir := SlotDirective{SlotStart: start, SlotEnd: start.Add(15 * time.Minute), BatteryEnergyWh: -838.6695152799834 * .25, PlannedGridW: 0, HasPlannedGridW: true}
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.PI = NewPI(.5, .1, 3000, 10000)
	st.clock = func() time.Time { return start }
	st.MinDispatchIntervalS = 0
	st.SlewEnabled = false
	store := seedStore(2031.009439662536, []struct {
		name          string
		currentW, soc float64
	}{{"sungrow", 1513.1961523958023, .875}})
	targets := ComputeDispatch(store, st, caps(map[string]float64{"sungrow": 9600}), 11040)
	if len(targets) != 1 || targets[0].TargetW != 0 {
		t.Fatalf("opposite command escaped: %+v", targets)
	}
	start = start.Add(6 * time.Second)
	store = seedStore(618.7841045439957, []struct {
		name          string
		currentW, soc float64
	}{{"sungrow", 108.64259570154694, .875}})
	targets = ComputeDispatch(store, st, caps(map[string]float64{"sungrow": 9600}), 11040)
	if len(targets) != 1 || targets[0].TargetW >= 0 {
		t.Fatalf("cover-load failed to resume: %+v", targets)
	}
}
