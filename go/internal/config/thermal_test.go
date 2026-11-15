package config

import (
	"math"
	"strings"
	"testing"
)

const thermalYAML = `
site:
  name: test
fuse:
  max_amps: 16
thermal:
  cheap_day_ore_kwh: 150
  normal_day_ore_kwh: 260
  dear_day_ore_kwh: 400
  min_saving_ore_per_day: 500
  zones:
    - comfort_sensor: { driver: wiser, metric: bathroom_floor_c }
      cheap_day_raise_k: 2.5
    - comfort_sensor: { driver: wiser, metric: bathroom_air_c }
      dear_day_lower_k: 1
      step_k: 0.5
`

func TestThermalSectionCarriesTheOwnersNumbers(t *testing.T) {
	c, err := Parse([]byte(thermalYAML), ".")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := c.Thermal.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	h := c.Thermal
	if h.CheapDayOreKWh != 150 || h.NormalDayOreKWh != 260 || h.DearDayOreKWh != 400 || h.MinSavingOrePerDay != 500 {
		t.Errorf("levels = %+v", h)
	}
	floor, ok := h.Zone("wiser/bathroom_floor_c")
	if !ok || floor.CheapDayRaiseK != 2.5 || floor.DearDayLowerK != 0 {
		t.Errorf("floor = %+v, %v", floor, ok)
	}
	air, ok := h.Zone("wiser/bathroom_air_c")
	if !ok || air.DearDayLowerK != 1 || air.StepK != 0.5 || air.CheapDayRaiseK != 0 {
		t.Errorf("air = %+v, %v", air, ok)
	}
	if _, ok := h.Zone("wiser/hall_c"); ok {
		t.Error("a zone with no rule got one")
	}
}

func TestThermalSectionIsOffUntilTheOwnerSetsIt(t *testing.T) {
	var none *Thermal
	if _, ok := none.Zone("a/b"); ok || none.Validate() != nil {
		t.Error("a missing thermal section must change nothing")
	}
	if (&Thermal{}).Validate() != nil {
		t.Error("an empty thermal section must be valid")
	}
}

func TestThermalSectionRefusesWhatCannotBeReadOneWay(t *testing.T) {
	ok := func() *Thermal {
		return &Thermal{CheapDayOreKWh: 150, NormalDayOreKWh: 260, DearDayOreKWh: 400,
			Zones: []ThermalZone{{ComfortSensor: ThermalSensor{Driver: "wiser", Metric: "floor_c"}, CheapDayRaiseK: 2, DearDayLowerK: 1}}}
	}
	for name, tc := range map[string]struct {
		change func(*Thermal)
		want   string
	}{
		"cheap above normal": {func(h *Thermal) { h.CheapDayOreKWh = 300 }, "cheap_day_ore_kwh below"},
		"dear below normal":  {func(h *Thermal) { h.DearDayOreKWh = 200 }, "dear_day_ore_kwh above"},
		"no price levels":    {func(h *Thermal) { h.CheapDayOreKWh, h.NormalDayOreKWh, h.DearDayOreKWh = 0, 0, 0 }, "cheap_day_ore_kwh below"},
		"negative level":     {func(h *Thermal) { h.CheapDayOreKWh = -5 }, "day price levels"},
		"level not a number": {func(h *Thermal) { h.DearDayOreKWh = math.Inf(1) }, "finite"},
		"negative saving":    {func(h *Thermal) { h.MinSavingOrePerDay = -1 }, "min_saving_ore_per_day"},
		"negative raise":     {func(h *Thermal) { h.Zones[0].CheapDayRaiseK = -1 }, "0 to 10 K"},
		"a raise of 20 K":    {func(h *Thermal) { h.Zones[0].CheapDayRaiseK = 20 }, "0 to 10 K"},
		"sensor not named":   {func(h *Thermal) { h.Zones[0].ComfortSensor.Metric = "" }, "needs a driver name and a metric"},
		"sensor twice":       {func(h *Thermal) { h.Zones = append(h.Zones, h.Zones[0]) }, "listed twice"},
	} {
		h := ok()
		tc.change(h)
		err := h.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, tc.want)
		}
	}
	if err := ok().Validate(); err != nil {
		t.Fatalf("the valid section: %v", err)
	}
	// A direction that is off needs no price level.
	only := &Thermal{NormalDayOreKWh: 260, DearDayOreKWh: 400,
		Zones: []ThermalZone{{ComfortSensor: ThermalSensor{Driver: "a", Metric: "b"}, DearDayLowerK: 1}}}
	if err := only.Validate(); err != nil {
		t.Errorf("lower only, no cheap level: %v", err)
	}
	c := &Config{Thermal: ok()}
	c.Thermal.DearDayOreKWh = 100
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "thermal.zones[0]") {
		t.Errorf("Config.Validate let a bad thermal section through: %v", err)
	}
}
