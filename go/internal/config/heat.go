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
	// Boundary is what the zone loses heat to: "outdoor" for the weather
	// forecast, or "<driver>/<metric>" for a measured temperature, such as
	// the room air around a floor. Required: a floor that loses heat to
	// outdoors is planned as a different thing.
	Boundary string `yaml:"boundary" json:"boundary"`
	// Source is what makes the heat. Only "direct_electric" is planned yet;
	// a heat pump needs its efficiency declared, which this section does not
	// carry.
	Source string `yaml:"source" json:"source"`
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
)

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
		if z.Boundary != HeatBoundaryOutdoor && !heatKey.MatchString(z.Boundary) {
			return fmt.Errorf("%s (%s): boundary must be \"outdoor\" or \"<driver>/<metric>\"", at, z.Name)
		}
		if z.Source != HeatSourceDirectElectric {
			return fmt.Errorf("%s (%s): source must be %q; a heat pump is not planned from this section yet", at, z.Name, HeatSourceDirectElectric)
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
