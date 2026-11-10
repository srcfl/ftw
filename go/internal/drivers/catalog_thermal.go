package drivers

import (
	"math"
	"regexp"
	"strings"

	"github.com/srcfl/ftw/go/internal/thermal"
)

// What a heat control does, said by the driver in the thermal contract's own
// vocabulary, so Core can reason about a heat pump, a thermostat and a tank
// without knowing any of them by name. Proposed in docs/adr/0012.
//
// This only describes. Nothing in Core plans for or sends a thermal control
// because of it; the operator path in api_driver_control.go is unchanged.
type CatalogThermalControl struct {
	Asset    thermal.AssetKind         `json:"asset"`
	Strategy thermal.InfluenceStrategy `json:"strategy"`
	// Zone links the control to the sensors that judge it. Empty means the
	// driver does not say.
	Zone string `json:"zone,omitempty"`
	// MinIntervalS is the shortest time between two writes the device or its
	// vendor accepts: a cloud rate limit, or flash wear.
	MinIntervalS *int `json:"min_interval_s,omitempty"`
	// MinOnS and MinOffS protect a compressor from short cycling. They are
	// declared only on an on_off control.
	MinOnS  *int `json:"min_on_s,omitempty"`
	MinOffS *int `json:"min_off_s,omitempty"`
}

// CatalogSensor names one metric the driver already emits as the temperature
// of a heat store: a room, a hot-water tank, a buffer.
type CatalogSensor struct {
	Metric string            `json:"metric"`
	Asset  thermal.AssetKind `json:"asset"`
	Zone   string            `json:"zone,omitempty"`
}

// The strategies a control may declare. synthetic_price is a signal Core
// would publish, not a lever a driver offers, so it is not one of them.
var controlStrategyInput = map[thermal.InfluenceStrategy]string{
	thermal.InfluenceCurveOffset:  "number",
	thermal.InfluenceSetpoint:     "number",
	thermal.InfluenceOnOff:        "boolean",
	thermal.InfluenceDiscreteMode: "string",
}

var thermalControlKeys = []string{"asset", "strategy", "zone", "min_interval_s", "min_on_s", "min_off_s"}

var metricNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// pickThermalControl reads the thermal fields of one control entry. ok is
// false when the entry tries to declare them and gets them wrong, and the
// caller then drops the whole control, as it does for a bad evidence kind.
// A planner later trusting a misdeclared strategy, or silently missing a
// compressor's minimum off time because it was written as a string, is the
// failure this prevents.
func pickThermalControl(entry string, input CatalogControlInput) (*CatalogThermalControl, bool) {
	entry = withoutBlock(entry, "input")
	declared := false
	for _, key := range thermalControlKeys {
		if fieldPresent(entry, key) {
			declared = true
			break
		}
	}
	if !declared {
		return nil, true
	}

	out := &CatalogThermalControl{
		Asset:    thermal.AssetKind(fieldString(entry, "asset")),
		Strategy: thermal.InfluenceStrategy(fieldString(entry, "strategy")),
		Zone:     fieldString(entry, "zone"),
	}
	if !thermal.ValidAssetKind(out.Asset) {
		return nil, false
	}
	want, known := controlStrategyInput[out.Strategy]
	if !known || input.Type != want {
		return nil, false
	}
	if out.Strategy == thermal.InfluenceDiscreteMode && len(input.Options) < 2 {
		return nil, false
	}
	if fieldPresent(entry, "zone") && !validControlToken(out.Zone) {
		return nil, false
	}

	var ok bool
	if out.MinIntervalS, ok = pickSeconds(entry, "min_interval_s"); !ok {
		return nil, false
	}
	if out.MinOnS, ok = pickSeconds(entry, "min_on_s"); !ok {
		return nil, false
	}
	if out.MinOffS, ok = pickSeconds(entry, "min_off_s"); !ok {
		return nil, false
	}
	if (out.MinOnS != nil || out.MinOffS != nil) && out.Strategy != thermal.InfluenceOnOff {
		return nil, false
	}
	return out, true
}

// pickSeconds reads a whole, non-negative number of seconds. Absent is nil and
// fine; present and anything else is not.
func pickSeconds(entry, name string) (*int, bool) {
	if !fieldPresent(entry, name) {
		return nil, true
	}
	v := fieldNumber(entry, name)
	if v == nil || *v < 0 || *v != math.Trunc(*v) || *v > 7*24*3600 {
		return nil, false
	}
	s := int(*v)
	return &s, true
}

// pickOptions reads an input's `options = { "a", "b" }`. Absent is nil and
// fine. A list that holds anything but distinct control tokens is refused:
// an option the API would accept and the driver would not is the gap a
// closed set exists to remove.
func pickOptions(input string) ([]string, bool) {
	if !fieldPresent(input, "options") {
		return nil, true
	}
	body := nestedBlock(input, "options")
	if strings.TrimSpace(body) == "" {
		return nil, false
	}
	quoted := regexp.MustCompile(`"([^"]*)"`)
	if strings.Trim(quoted.ReplaceAllString(body, ""), ", \t\r\n") != "" {
		return nil, false
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range quoted.FindAllStringSubmatch(body, -1) {
		if !validControlToken(m[1]) || seen[m[1]] {
			return nil, false
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	return out, true
}

// pickSensors reads the DRIVER block's `sensors` list. Each entry stands on
// its own, so one malformed sensor is dropped without hiding the others.
func pickSensors(block string) []CatalogSensor {
	body := nestedBlock(stripLuaComments(block), "sensors")
	if body == "" {
		return nil
	}
	var out []CatalogSensor
	seen := map[string]bool{}
	for _, entry := range topLevelTables(body) {
		s := CatalogSensor{
			Metric: fieldString(entry, "metric"),
			Asset:  thermal.AssetKind(fieldString(entry, "asset")),
			Zone:   fieldString(entry, "zone"),
		}
		if len(s.Metric) > 128 || !metricNameRE.MatchString(s.Metric) || seen[s.Metric] {
			continue
		}
		if !thermal.ValidAssetKind(s.Asset) {
			continue
		}
		if fieldPresent(entry, "zone") && !validControlToken(s.Zone) {
			continue
		}
		seen[s.Metric] = true
		out = append(out, s)
	}
	return out
}

// fieldPresent reports whether `name =` appears as a whole field name,
// whatever its value. It is how a malformed value is told apart from an
// absent one.
func fieldPresent(block, name string) bool {
	return regexp.MustCompile(`(?:^|[\s,{])` + regexp.QuoteMeta(name) + `\s*=`).MatchString(block)
}

// withoutBlock blanks `name = { … }` so fields inside it are not read as the
// entry's own: a control's thermal `zone` is not something inside its input.
func withoutBlock(block, name string) string {
	inner := nestedBlock(block, name)
	if inner == "" {
		return block
	}
	return strings.Replace(block, inner, strings.Repeat(" ", len(inner)), 1)
}
