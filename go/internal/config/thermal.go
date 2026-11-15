package config

import (
	"fmt"
	"math"
	"strings"
)

// Thermal is the owner's say over what heat is worth (docs/adr/0012,
// "Comfort follows the day's price"). Every number is the owner's: nothing
// here has a default, and a site without the section runs exactly as
// before. The zone's target, band and controls of the ADR are not here yet.
//
// The section is plain numbers. What turns them into the planner's rules
// lives beside the planner's caller, so the configuration does not pull the
// planner onto the control path.
//
// Prices are the day's mean of the total consumer price, öre/kWh, the same
// figure the price view shows.
type Thermal struct {
	// Day price levels. On a day at or below cheap_day_ore_kwh a zone gets
	// all of its cheap_day_raise_k; at normal_day_ore_kwh its target is the owner's
	// own; at or above dear_day_ore_kwh it loses all of its dear_day_lower_k. The
	// change between them is linear.
	CheapDayOreKWh  float64 `yaml:"cheap_day_ore_kwh,omitempty" json:"cheap_day_ore_kwh,omitempty"`
	NormalDayOreKWh float64 `yaml:"normal_day_ore_kwh,omitempty" json:"normal_day_ore_kwh,omitempty"`
	DearDayOreKWh   float64 `yaml:"dear_day_ore_kwh,omitempty" json:"dear_day_ore_kwh,omitempty"`
	// MinSavingOrePerDay is what moving heat in time must save across all
	// zones in a day before any of it is acted on. Below it every zone is
	// left on its target for the day. 0 means no limit.
	MinSavingOrePerDay float64 `yaml:"min_saving_ore_per_day,omitempty" json:"min_saving_ore_per_day,omitempty"`
	// Zones lists the comfort each zone gives and takes. A zone not listed
	// keeps its target whatever the day costs.
	Zones []ThermalZone `yaml:"zones,omitempty" json:"zones,omitempty"`
}

// ThermalSensor names one driver's metric.
type ThermalSensor struct {
	Driver string `yaml:"driver" json:"driver"`
	Metric string `yaml:"metric" json:"metric"`
}

// Key is the name the thermal twin lists the sensor under.
func (s ThermalSensor) Key() string { return s.Driver + "/" + s.Metric }

// ThermalZone is one zone's comfort rule.
type ThermalZone struct {
	// ComfortSensor is the zone: the sensor whose temperature the owner
	// cares about, as in the ADR.
	ComfortSensor ThermalSensor `yaml:"comfort_sensor" json:"comfort_sensor"`
	// CheapDayRaiseK is how much warmer the zone is wanted on a cheap day,
	// DearDayLowerK how much cooler it may be on a dear one, in kelvin.
	// Zero leaves that direction off.
	CheapDayRaiseK float64 `yaml:"cheap_day_raise_k,omitempty" json:"cheap_day_raise_k,omitempty"`
	DearDayLowerK  float64 `yaml:"dear_day_lower_k,omitempty" json:"dear_day_lower_k,omitempty"`
	// StepK rounds the change to the thermostat's step. 0 leaves it
	// unrounded.
	StepK float64 `yaml:"step_k,omitempty" json:"step_k,omitempty"`
}

// Zone is the rule for one comfort sensor, by its key, and whether the
// owner listed it.
func (h *Thermal) Zone(sensor string) (ThermalZone, bool) {
	if h == nil {
		return ThermalZone{}, false
	}
	for _, z := range h.Zones {
		if z.ComfortSensor.Key() == sensor {
			return z, true
		}
	}
	return ThermalZone{}, false
}

// Validate refuses a section that cannot be read one way, so a typo is told
// to the owner instead of silently moving nothing.
func (h *Thermal) Validate() error {
	if h == nil {
		return nil
	}
	if h.CheapDayOreKWh < 0 || h.NormalDayOreKWh < 0 || h.DearDayOreKWh < 0 {
		return fmt.Errorf("thermal: day price levels must be >= 0")
	}
	if h.MinSavingOrePerDay < 0 {
		return fmt.Errorf("thermal.min_saving_ore_per_day must be >= 0")
	}
	if !thermalFinite(h.CheapDayOreKWh, h.NormalDayOreKWh, h.DearDayOreKWh, h.MinSavingOrePerDay) {
		return fmt.Errorf("thermal: prices and the least saving must be finite")
	}
	seen := map[string]bool{}
	for i, z := range h.Zones {
		at := fmt.Sprintf("thermal.zones[%d]", i)
		key := z.ComfortSensor.Key()
		if z.ComfortSensor.Driver == "" || z.ComfortSensor.Metric == "" || strings.Contains(z.ComfortSensor.Driver, "/") {
			return fmt.Errorf("%s.comfort_sensor needs a driver name and a metric", at)
		}
		if seen[key] {
			return fmt.Errorf("%s.comfort_sensor %s is listed twice", at, key)
		}
		seen[key] = true
		if !thermalFinite(z.CheapDayRaiseK, z.DearDayLowerK, z.StepK) ||
			z.CheapDayRaiseK < 0 || z.DearDayLowerK < 0 || z.CheapDayRaiseK > 10 || z.DearDayLowerK > 10 ||
			z.StepK < 0 || z.StepK > 5 {
			return fmt.Errorf("%s (%s): a target moves 0 to 10 K, in steps of at most 5 K", at, key)
		}
		if z.CheapDayRaiseK > 0 && !(h.CheapDayOreKWh < h.NormalDayOreKWh) {
			return fmt.Errorf("%s (%s): cheap_day_raise_k needs thermal.cheap_day_ore_kwh below normal_day_ore_kwh", at, key)
		}
		if z.DearDayLowerK > 0 && !(h.NormalDayOreKWh < h.DearDayOreKWh) {
			return fmt.Errorf("%s (%s): dear_day_lower_k needs thermal.dear_day_ore_kwh above normal_day_ore_kwh", at, key)
		}
	}
	return nil
}

func thermalFinite(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
