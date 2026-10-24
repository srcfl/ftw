package loadpoint

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestTwoCarsShareTotalFuseCap(t *testing.T) {
	now := time.Now()
	configs := []Config{
		{ID: "a", DriverName: "charger-a", MinChargeW: 1380, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 2760, 4140, 5520, 6900, 8280, 9660, 11000}},
		{ID: "b", DriverName: "charger-b", MinChargeW: 1380, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 2760, 4140, 5520, 6900, 8280, 9660, 11000}},
	}
	d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{"a": 2750, "b": 2750}, LoadpointMaxPowerW: map[string]float64{"a": 11000, "b": 11000}}
	sender := &fakeSender{}
	c := newTestController(t, configs, d, map[string]EVSample{"charger-a": {Connected: true}, "charger-b": {Connected: true}}, sender)
	c.SetFuseEVMax(func() (float64, bool) { return 5000, true })
	c.Tick(context.Background(), now)
	total := 0.0
	commands := map[string]float64{}
	for _, cmd := range sender.calls {
		if cmd.action == "ev_set_current" {
			commands[cmd.driver] = cmd.power
			total += cmd.power
		}
	}
	if len(commands) != 2 {
		t.Fatalf("commands=%v", commands)
	}
	if total > 5000.0001 {
		t.Fatalf("shared cap=5000 W, sum=%.2f W, per charger=%v", total, commands)
	}
	if total <= 0 {
		t.Fatalf("usable shared cap abandoned: %v", commands)
	}
}

func TestMixedChargingIntentSharesTotalFuseCap(t *testing.T) {
	for _, intent := range []string{"manual-a", "manual-both", "pause-a", "planned-zero-a", "nan-cap", "infinite-cap"} {
		t.Run(intent, func(t *testing.T) {
			now := time.Now()
			configs := []Config{
				{ID: "a", DriverName: "charger-a", MinChargeW: 1380, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 2760, 4140, 5520, 6900, 8280, 9660, 11000}},
				{ID: "b", DriverName: "charger-b", MinChargeW: 1380, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 2760, 4140, 5520, 6900, 8280, 9660, 11000}},
			}
			d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{"a": 2750, "b": 2750}, LoadpointMaxPowerW: map[string]float64{"a": 11000, "b": 11000}}
			sender := &fakeSender{}
			c := newTestController(t, configs, d, map[string]EVSample{"charger-a": {Connected: true, RequestActive: true}, "charger-b": {Connected: true, RequestActive: true}}, sender)
			capW := 5000.0
			switch intent {
			case "manual-a":
				c.SetManualHold("a", ManualHold{PowerW: 11000, Persistent: true})
			case "manual-both":
				c.SetManualHold("a", ManualHold{PowerW: 11000, Persistent: true})
				c.SetManualHold("b", ManualHold{PowerW: 11000, Persistent: true})
			case "pause-a":
				c.SetManualHold("a", ManualHold{PowerW: 0, Persistent: true})
			case "planned-zero-a":
				d.LoadpointEnergyWh["a"] = 0
			case "nan-cap":
				capW = math.NaN()
			case "infinite-cap":
				capW = math.Inf(1)
			}
			c.SetFuseEVMax(func() (float64, bool) { return capW, true })
			c.Tick(context.Background(), now)
			total := 0.0
			commands := map[string]float64{}
			for _, cmd := range sender.calls {
				if cmd.action == "ev_set_current" {
					commands[cmd.driver] = cmd.power
					total += cmd.power
				}
			}
			if len(commands) != 2 {
				t.Fatalf("commands=%v", commands)
			}
			maxTotal := 5000.0
			if math.IsNaN(capW) || math.IsInf(capW, 0) {
				maxTotal = 0
			}
			if math.IsNaN(total) || math.IsInf(total, 0) || total > maxTotal+0.0001 {
				t.Fatalf("intent=%s cap=%v sum=%v per charger=%v", intent, capW, total, commands)
			}
			if (intent == "pause-a" || intent == "planned-zero-a") && commands["charger-a"] != 0 {
				t.Fatalf("zero intent replaced: %v", commands)
			}
			if maxTotal > 0 && total <= 0 {
				t.Fatalf("usable cap abandoned: %v", commands)
			}
		})
	}
}

func TestOnePhaseCarsShareWorstPhaseBudget(t *testing.T) {
	now := time.Now()
	configs := []Config{
		{ID: "a", DriverName: "charger-a", MinChargeW: 1380, MaxChargeW: 5750, PhaseMode: "1p", AllowedStepsW: []float64{0, 1380, 2300, 3680, 4600, 5750}},
		{ID: "b", DriverName: "charger-b", MinChargeW: 1380, MaxChargeW: 5750, PhaseMode: "1p", AllowedStepsW: []float64{0, 1380, 2300, 3680, 4600, 5750}},
	}
	d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{"a": 1437.5, "b": 1437.5}, LoadpointMaxPowerW: map[string]float64{"a": 5750, "b": 5750}}
	sender := &fakeSender{}
	c := newTestController(t, configs, d, map[string]EVSample{"charger-a": {Connected: true, RequestActive: true}, "charger-b": {Connected: true, RequestActive: true}}, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	c.SetFuseEVMax(meteredEVCap(c, func() float64 { return 1000.0 / 230 }))
	c.Tick(context.Background(), now)
	total := 0.0
	commands := map[string]float64{}
	for _, cmd := range sender.calls {
		if cmd.action == "ev_set_current" {
			commands[cmd.driver] = cmd.power
			total += cmd.power
		}
	}
	// Both unknown/same single-phase connections may be on L1. The house
	// takes1000W there; the offered EV sum must fit the remaining4750W.
	if total > 4750.0001 {
		t.Fatalf("single-phase sum=%v implies L1=%.2fA above25A, commands=%v", total, (1000+total)/230, commands)
	}
	if total <= 0 {
		t.Fatalf("usable phase headroom abandoned: %v", commands)
	}
}

func TestReachedGoalDoesNotReserveIdleFuseBudget(t *testing.T) {
	now := time.Now()
	configs := []Config{
		{ID: "a", DriverName: "charger-a", VehicleCapacityWh: 60000, PluginSoC: .8, MinChargeW: 1380, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 2760, 4140, 5520, 6900, 8280, 9660, 11000}},
		{ID: "b", DriverName: "charger-b", VehicleCapacityWh: 60000, PluginSoC: .3, MinChargeW: 1380, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 2760, 4140, 5520, 6900, 8280, 9660, 11000}},
	}
	d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{"b": 2750}, LoadpointMaxPowerW: map[string]float64{"b": 11000}}
	sender := &fakeSender{}
	c := newTestController(t, configs, d, map[string]EVSample{"charger-a": {Connected: true, RequestActive: true}, "charger-b": {Connected: true, RequestActive: true}}, sender)
	for _, id := range []string{"a", "b"} {
		c.manager.Observe(id, true, 0, 0, true)
	}
	c.manager.SetTarget("a", .8, now.Add(time.Hour))
	c.manager.SetTarget("b", .8, now.Add(2*time.Hour))
	c.manager.SetCurrentSoC("a", .8)
	c.manager.SetCurrentSoC("b", .3)
	c.SetFuseEVMax(func() (float64, bool) { return 5000, true })
	c.Tick(context.Background(), now)
	commands := map[string]float64{}
	for _, cmd := range sender.calls {
		if cmd.action == "ev_set_current" {
			commands[cmd.driver] = cmd.power
		}
	}
	if commands["charger-a"] != 0 {
		t.Fatalf("reached goal resumed: %v", commands)
	}
	if commands["charger-b"] <= 0 {
		t.Fatalf("reached early goal consumed phantom shared budget: %v", commands)
	}
	if commands["charger-b"] > 5000.0001 {
		t.Fatalf("remaining goal exceeded shared cap: %v", commands)
	}
}

func TestZeroSurplusDoesNotReservePhantomFuseBudget(t *testing.T) {
	now := time.Now()
	configs := []Config{
		{ID: "a", DriverName: "charger-a", VehicleCapacityWh: 60000, PluginSoC: .3, SurplusOnly: true, MinChargeW: 1380, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 2760, 4140, 5520, 6900, 8280, 9660, 11000}},
		{ID: "b", DriverName: "charger-b", VehicleCapacityWh: 60000, PluginSoC: .3, MinChargeW: 1380, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 2760, 4140, 5520, 6900, 8280, 9660, 11000}},
	}
	d := &Directive{SlotStart: now, SlotEnd: now.Add(15 * time.Minute), LoadpointEnergyWh: map[string]float64{"a": 0, "b": 2750}, LoadpointMaxPowerW: map[string]float64{"a": 11000, "b": 11000}}
	sender := &fakeSender{}
	c := newTestController(t, configs, d, map[string]EVSample{"charger-a": {Connected: true, RequestActive: true}, "charger-b": {Connected: true, RequestActive: true}}, sender)
	for _, id := range []string{"a", "b"} {
		c.manager.Observe(id, true, 0, 0, true)
		c.manager.SetCurrentSoC(id, .3)
	}
	c.manager.SetTarget("a", .8, now.Add(time.Hour))
	c.manager.SetTarget("b", .8, now.Add(2*time.Hour))
	c.SetSiteSurplusForEV(func() (float64, bool) { return 0, true })
	c.SetFuseEVMax(func() (float64, bool) { return 5000, true })
	c.Tick(context.Background(), now)
	commands := map[string]float64{}
	for _, cmd := range sender.calls {
		if cmd.action == "ev_set_current" {
			commands[cmd.driver] = cmd.power
		}
	}
	if commands["charger-a"] != 0 {
		t.Fatalf("surplus-only car imported without PV: %v", commands)
	}
	if commands["charger-b"] <= 0 {
		t.Fatalf("zero-PV opportunism consumed real goal's shared budget: %v", commands)
	}
	if commands["charger-b"] > 5000.0001 {
		t.Fatalf("remaining goal exceeded shared cap: %v", commands)
	}
}
