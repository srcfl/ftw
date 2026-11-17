package config

import (
	"fmt"
	"math"
	"regexp"

	"github.com/srcfl/ftw/go/internal/heatplan"
)

// Heat is the owner's heat control (docs/adr/0012). Everything in it is the
// owner's own number: the band a zone may move in, what a cheap or dear day
// is worth, the least saving worth acting on, the peak worth sitting out.
// There is no default for any of them, and a section that is left out
// changes nothing. Plans are proposals; they are only written to a device
// when Write is true.
type Heat struct {
	// Write lets Core send a plan's values to the zones' controls. False, the
	// default, runs the planner in shadow: the plan and what it would save
	// are shown and nothing is written.
	Write bool `yaml:"write,omitempty" json:"write,omitempty"`
	// Savings is the least saving a day's plans must reach together, over
	// every zone, before they are acted on. Nil means no limit.
	Savings *heatplan.SavingsGate `yaml:"savings,omitempty" json:"savings,omitempty"`
	Zones   []HeatZone            `yaml:"zones,omitempty" json:"zones,omitempty"`
}

// HeatZone is one heated thing: a room's air, a floor.
type HeatZone struct {
	// Name is the zone's own name, used in plans and logs.
	Name string `yaml:"name" json:"name"`
	// Sensor is the thermal twin's key for this zone, "<driver>/<metric>".
	Sensor string `yaml:"sensor" json:"sensor"`
	// Control is the declared control the plan moves, "<driver>/<control>".
	// Empty means the zone is planned in shadow only.
	Control string `yaml:"control,omitempty" json:"control,omitempty"`
	// HeatInput is the "<driver>/<metric>" that measures or estimates the
	// heat warming this zone, for the thermal twin to learn against. Needed
	// when the sensor is on one device and the heat comes from another, as
	// room sensors on an underfloor gateway heated by a heat pump. Empty
	// lets the twin find a heat pump's power by itself.
	HeatInput string `yaml:"heat_input,omitempty" json:"heat_input,omitempty"`
	// Boundary is what the zone loses heat to: "outdoor" for the weather
	// forecast, or "<driver>/<metric>" for a measured temperature, such as
	// the room air around a floor. Required: a floor that loses heat to
	// outdoors is planned as a different thing.
	Boundary string `yaml:"boundary" json:"boundary"`
	// Source is what makes the heat: "direct_electric" or "heat_pump".
	Source string `yaml:"source" json:"source"`
	// HeatPump is the pump's efficiency, for source heat_pump. Nil, or a
	// field left at zero, takes the planner's starting point (half the
	// ideal COP, capped at 6), which is no claim about any pump.
	HeatPump *HeatPump `yaml:"heat_pump,omitempty" json:"heat_pump,omitempty"`
	// CurveEffect is what one unit of a curve_offset control does, the
	// owner's figures until a twin learns them. Required for a zone whose
	// control is a curve offset.
	CurveEffect *CurveEffect `yaml:"curve_effect,omitempty" json:"curve_effect,omitempty"`
	// HeatKW is the heat at full output, the owner's figure: a floor's area
	// times its rating, a radiator's plate.
	HeatKW float64 `yaml:"heat_kw" json:"heat_kw"`
	// TargetC is what the owner asks for; MinC and MaxC the hard band
	// around it. The band is never widened.
	TargetC float64 `yaml:"target_c" json:"target_c"`
	MinC    float64 `yaml:"min_c" json:"min_c"`
	MaxC    float64 `yaml:"max_c" json:"max_c"`
	// ComfortWeight prices distance from the target; see heatplan.Zone.
	ComfortWeight float64 `yaml:"comfort_weight,omitempty" json:"comfort_weight,omitempty"`
	// PriceComfort moves the target with the day's mean price.
	PriceComfort *heatplan.PriceComfort `yaml:"price_comfort,omitempty" json:"price_comfort,omitempty"`
	// PeakRelief lets the zone sit at the bottom of its band through a peak.
	PeakRelief *heatplan.PeakRelief `yaml:"peak_relief,omitempty" json:"peak_relief,omitempty"`
}

// The words a heat zone's boundary and source may take besides a sensor key.
const (
	HeatBoundaryOutdoor      = "outdoor"
	HeatSourceDirectElectric = "direct_electric"
	HeatSourceHeatPump       = "heat_pump"
)

// HeatPump is a heat pump's efficiency as the owner knows it.
type HeatPump struct {
	// CarnotFraction is the share of the ideal COP the pump reaches.
	CarnotFraction float64 `yaml:"carnot_fraction,omitempty" json:"carnot_fraction,omitempty"`
	// MaxCOP caps the COP.
	MaxCOP float64 `yaml:"max_cop,omitempty" json:"max_cop,omitempty"`
	// FrostLoss is the share of output an air-source pump loses to
	// defrosting at its worst, around +1 °C.
	FrostLoss float64 `yaml:"frost_loss,omitempty" json:"frost_loss,omitempty"`
	// Supply is the heat curve of a hydronic pump: the flow temperature at
	// a cold and a mild outdoor temperature. Nil for a pump that heats the
	// zone itself.
	Supply *HeatSupplyCurve `yaml:"supply,omitempty" json:"supply,omitempty"`
}

// HeatSupplyCurve is a heat curve by two points, flat outside them.
type HeatSupplyCurve struct {
	ColdOutdoorC float64 `yaml:"cold_outdoor_c" json:"cold_outdoor_c"`
	ColdSupplyC  float64 `yaml:"cold_supply_c" json:"cold_supply_c"`
	MildOutdoorC float64 `yaml:"mild_outdoor_c" json:"mild_outdoor_c"`
	MildSupplyC  float64 `yaml:"mild_supply_c" json:"mild_supply_c"`
}

// CurveEffect is one unit of a curve offset: how far it moves the zone and
// the flow.
type CurveEffect struct {
	ZoneKPerUnit   float64 `yaml:"zone_k_per_unit" json:"zone_k_per_unit"`
	SupplyKPerUnit float64 `yaml:"supply_k_per_unit" json:"supply_k_per_unit"`
}

// PlanSource is the zone's source as the planner reads it.
func (z HeatZone) PlanSource() heatplan.Source {
	if z.Source != HeatSourceHeatPump {
		return heatplan.Source{Kind: heatplan.SourceDirectElectric}
	}
	src := heatplan.Source{Kind: heatplan.SourceHeatPump}
	if hp := z.HeatPump; hp != nil {
		src.CarnotFraction, src.MaxCOP, src.FrostLoss = hp.CarnotFraction, hp.MaxCOP, hp.FrostLoss
		if c := hp.Supply; c != nil {
			src.Supply = &heatplan.SupplyCurve{ColdOutdoorC: c.ColdOutdoorC, ColdSupplyC: c.ColdSupplyC, MildOutdoorC: c.MildOutdoorC, MildSupplyC: c.MildSupplyC}
		}
	}
	return src
}

var heatKey = regexp.MustCompile(`^[^/\s]+/[^/\s]+$`)

// Validate refuses a heat section that cannot be read one way.
func (h *Heat) Validate() error {
	if h == nil {
		return nil
	}
	if h.Savings != nil {
		if err := h.Savings.Validate(); err != nil {
			return fmt.Errorf("heat.savings: %w", err)
		}
	}
	seen := map[string]bool{}
	for i, z := range h.Zones {
		at := fmt.Sprintf("heat.zones[%d]", i)
		if z.Name == "" {
			return fmt.Errorf("%s: name is required", at)
		}
		if seen[z.Name] {
			return fmt.Errorf("%s: zone %q is listed twice", at, z.Name)
		}
		seen[z.Name] = true
		if !heatKey.MatchString(z.Sensor) {
			return fmt.Errorf("%s (%s): sensor must be \"<driver>/<metric>\"", at, z.Name)
		}
		if z.Control != "" && !heatKey.MatchString(z.Control) {
			return fmt.Errorf("%s (%s): control must be \"<driver>/<control>\"", at, z.Name)
		}
		if z.HeatInput != "" && !heatKey.MatchString(z.HeatInput) {
			return fmt.Errorf("%s (%s): heat_input must be \"<driver>/<metric>\"", at, z.Name)
		}
		if z.Boundary != HeatBoundaryOutdoor && !heatKey.MatchString(z.Boundary) {
			return fmt.Errorf("%s (%s): boundary must be \"outdoor\" or \"<driver>/<metric>\"", at, z.Name)
		}
		if z.Source != HeatSourceDirectElectric && z.Source != HeatSourceHeatPump {
			return fmt.Errorf("%s (%s): source must be %q or %q", at, z.Name, HeatSourceDirectElectric, HeatSourceHeatPump)
		}
		if z.HeatPump != nil && z.Source != HeatSourceHeatPump {
			return fmt.Errorf("%s (%s): heat_pump is set but source is %q", at, z.Name, z.Source)
		}
		if err := z.PlanSource().Validate(); err != nil {
			return fmt.Errorf("%s (%s): heat_pump: %w", at, z.Name, err)
		}
		if e := z.CurveEffect; e != nil {
			if math.IsNaN(e.ZoneKPerUnit+e.SupplyKPerUnit) || math.IsInf(e.ZoneKPerUnit+e.SupplyKPerUnit, 0) ||
				e.ZoneKPerUnit <= 0 || e.ZoneKPerUnit > 5 || e.SupplyKPerUnit < 0 || e.SupplyKPerUnit > 10 {
				return fmt.Errorf("%s (%s): curve_effect needs zone_k_per_unit above 0 and at most 5, supply_k_per_unit 0 to 10", at, z.Name)
			}
		}
		if !(z.HeatKW > 0) || z.HeatKW > 500 {
			return fmt.Errorf("%s (%s): heat_kw must be above 0 and at most 500", at, z.Name)
		}
		for _, v := range []float64{z.TargetC, z.MinC, z.MaxC, z.ComfortWeight} {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("%s (%s): temperatures must be finite", at, z.Name)
			}
		}
		if !(z.MinC <= z.TargetC && z.TargetC <= z.MaxC) || z.MinC == z.MaxC {
			return fmt.Errorf("%s (%s): need min_c <= target_c <= max_c with a band wider than zero", at, z.Name)
		}
		if z.ComfortWeight < 0 {
			return fmt.Errorf("%s (%s): comfort_weight must not be negative", at, z.Name)
		}
		if z.PriceComfort != nil {
			if err := z.PriceComfort.Validate(); err != nil {
				return fmt.Errorf("%s (%s): price_comfort: %w", at, z.Name, err)
			}
		}
		if z.PeakRelief != nil {
			if err := z.PeakRelief.Validate(); err != nil {
				return fmt.Errorf("%s (%s): peak_relief: %w", at, z.Name, err)
			}
		}
	}
	if h.Write {
		for _, z := range h.Zones {
			if z.Control != "" {
				return nil
			}
		}
		return fmt.Errorf("heat.write is on but no zone has a control to write to")
	}
	return nil
}
