package main

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/heatplan"
)

func TestThermalConfigBecomesThePlannersRules(t *testing.T) {
	cfg := &config.Thermal{
		CheapDayOreKWh: 150, NormalDayOreKWh: 260, DearDayOreKWh: 400, MinSavingOrePerDay: 500,
		Zones: []config.ThermalZone{
			{ComfortSensor: config.ThermalSensor{Driver: "wiser", Metric: "floor_c"}, CheapDayRaiseK: 2.5},
			{ComfortSensor: config.ThermalSensor{Driver: "wiser", Metric: "air_c"}, DearDayLowerK: 1, StepK: 0.5},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	floor := thermalComfort(cfg, "wiser/floor_c")
	if want := (heatplan.PriceComfort{CheapPrice: 150, NormalPrice: 260, DearPrice: 400, RaiseK: 2.5}); floor != want {
		t.Errorf("floor = %+v, want %+v", floor, want)
	}
	// What the configuration accepts, the planner's own check accepts.
	for _, key := range []string{"wiser/floor_c", "wiser/air_c"} {
		if err := thermalComfort(cfg, key).Validate(); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	if got := floor.ShiftK(205); got != 1.25 {
		t.Errorf("floor on a half-cheap day = %v K, want 1.25", got)
	}
	if got := thermalComfort(cfg, "wiser/air_c").ShiftK(500); got != -1 {
		t.Errorf("air on a dear day = %v K, want -1", got)
	}
	if thermalComfort(cfg, "wiser/hall_c").Enabled() {
		t.Error("a zone with no rule got one")
	}
	if gate := thermalSavingsGate(cfg); gate.MinPerDay != 500 || gate.KeepShare != thermalGateKeepShare {
		t.Errorf("gate = %+v", gate)
	}
}

func TestNoThermalConfigIsNoRuleAndNoGate(t *testing.T) {
	if thermalComfort(nil, "a/b").Enabled() || thermalSavingsGate(nil) != (heatplan.SavingsGate{}) {
		t.Error("a missing thermal section must change nothing")
	}
	if thermalSavingsGate(&config.Thermal{}) != (heatplan.SavingsGate{}) {
		t.Error("no limit must be no gate")
	}
	if open, _ := thermalSavingsGate(nil).Open(nil, 24, false); !open {
		t.Error("no limit closed the gate")
	}
}
