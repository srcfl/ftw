package drivers

import (
	"reflect"
	"testing"

	"github.com/srcfl/ftw/go/internal/thermal"
)

func intp(v int) *int { return &v }

const thermalDriver = `DRIVER = {
  id      = "probe",
  name    = "Probe",
  version = "1.0.0",
  controls = {
    {
      id = "set_heat_curve_offset", label = "Heat curve offset",
      evidence = "write_ack",
      asset = "space_heat", strategy = "curve_offset", zone = "z1",
      min_interval_s = 600,
      input = { type = "number", min = -3, max = 3, step = 1, unit = "°C" },
    },
    {
      id = "set_power", asset = "space_heat", strategy = "on_off",
      min_on_s = 1200, min_off_s = 900,
      input = { type = "boolean" },
    },
    {
      id = "set_dhw_mode", asset = "dhw", strategy = "discrete_mode",
      input = { type = "string", options = { "eco", "normal", "boost" } },
    },
  },
  sensors = {
    { metric = "hp_indoor_temp_c", asset = "space_heat", zone = "z1" },
    { metric = "hp_dhw_temp_c",    asset = "dhw" },
  },
}
`

func TestThermalDeclarationParses(t *testing.T) {
	entry := writeDriver(t, thermalDriver)
	if entry.ID != "probe" || entry.Version != "1.0.0" {
		t.Fatalf("thermal fields broke the surrounding block: %+v", entry)
	}
	if len(entry.Controls) != 3 {
		t.Fatalf("controls = %d, want 3: %+v", len(entry.Controls), entry.Controls)
	}
	want := []*CatalogThermalControl{
		{Asset: thermal.AssetSpaceHeat, Strategy: thermal.InfluenceCurveOffset, Zone: "z1", MinIntervalS: intp(600)},
		{Asset: thermal.AssetSpaceHeat, Strategy: thermal.InfluenceOnOff, MinOnS: intp(1200), MinOffS: intp(900)},
		{Asset: thermal.AssetDHW, Strategy: thermal.InfluenceDiscreteMode},
	}
	for i, w := range want {
		if got := entry.Controls[i].Thermal; !reflect.DeepEqual(got, w) {
			t.Errorf("control %s thermal = %+v, want %+v", entry.Controls[i].ID, got, w)
		}
	}
	if got := entry.Controls[2].Input.Options; !reflect.DeepEqual(got, []string{"eco", "normal", "boost"}) {
		t.Errorf("options = %v", got)
	}
	// The offset control's own input still parses as before.
	if in := entry.Controls[0].Input; in.Min == nil || *in.Min != -3 || in.Max == nil || *in.Max != 3 {
		t.Errorf("input = %+v", in)
	}

	wantSensors := []CatalogSensor{
		{Metric: "hp_indoor_temp_c", Asset: thermal.AssetSpaceHeat, Zone: "z1"},
		{Metric: "hp_dhw_temp_c", Asset: thermal.AssetDHW},
	}
	if !reflect.DeepEqual(entry.Sensors, wantSensors) {
		t.Errorf("sensors = %+v, want %+v", entry.Sensors, wantSensors)
	}
}

// A control that says nothing about heat keeps exactly the shape it had, so
// no existing driver changes for a field it never declared.
func TestControlWithoutThermalFieldsIsUnchanged(t *testing.T) {
	entry := writeDriver(t, offsetControl)
	if len(entry.Controls) != 1 || entry.Controls[0].Thermal != nil {
		t.Fatalf("controls = %+v, want one with no thermal", entry.Controls)
	}
	if entry.Sensors != nil {
		t.Errorf("sensors = %+v, want nil", entry.Sensors)
	}
}

// Each of these tries to declare a heat role and gets it wrong. The control
// is dropped rather than kept without its thermal fields: a planner trusting
// a misdeclared strategy, or missing a compressor's minimum off time because
// it was written as a string, is worse than the control not appearing.
func TestThermalControlDropsMisdeclarations(t *testing.T) {
	num := `input = { type = "number", min = -3, max = 3 }`
	cases := map[string]string{
		"asset only":           `{ id = "c", asset = "space_heat", ` + num + ` }`,
		"strategy only":        `{ id = "c", strategy = "curve_offset", ` + num + ` }`,
		"zone only":            `{ id = "c", zone = "z1", ` + num + ` }`,
		"unknown asset":        `{ id = "c", asset = "sauna", strategy = "curve_offset", ` + num + ` }`,
		"unknown strategy":     `{ id = "c", asset = "space_heat", strategy = "vibes", ` + num + ` }`,
		"synthetic price":      `{ id = "c", asset = "space_heat", strategy = "synthetic_price", ` + num + ` }`,
		"offset on boolean":    `{ id = "c", asset = "space_heat", strategy = "curve_offset", input = { type = "boolean" } }`,
		"on_off on number":     `{ id = "c", asset = "space_heat", strategy = "on_off", ` + num + ` }`,
		"mode without options": `{ id = "c", asset = "dhw", strategy = "discrete_mode", input = { type = "string" } }`,
		"mode with one option": `{ id = "c", asset = "dhw", strategy = "discrete_mode", input = { type = "string", options = { "eco" } } }`,
		"bad zone":             `{ id = "c", asset = "space_heat", strategy = "curve_offset", zone = "Living Room", ` + num + ` }`,
		"interval as string":   `{ id = "c", asset = "space_heat", strategy = "curve_offset", min_interval_s = "600", ` + num + ` }`,
		"negative interval":    `{ id = "c", asset = "space_heat", strategy = "curve_offset", min_interval_s = -1, ` + num + ` }`,
		"fractional interval":  `{ id = "c", asset = "space_heat", strategy = "curve_offset", min_interval_s = 1.5, ` + num + ` }`,
		"min_off as string":    `{ id = "c", asset = "space_heat", strategy = "on_off", min_off_s = "900", input = { type = "boolean" } }`,
		"min_on on a setpoint": `{ id = "c", asset = "space_heat", strategy = "setpoint", min_on_s = 60, ` + num + ` }`,
		"interval over a week": `{ id = "c", asset = "space_heat", strategy = "curve_offset", min_interval_s = 700000, ` + num + ` }`,
	}
	for name, entry := range cases {
		t.Run(name, func(t *testing.T) {
			parsed := writeDriver(t, "DRIVER = {\n  id = \"probe\",\n  controls = {\n    "+entry+",\n  },\n}\n")
			if len(parsed.Controls) != 0 {
				t.Errorf("controls = %+v, want none", parsed.Controls)
			}
		})
	}
}

// A zone written inside the input belongs to the input, not to the control,
// and must not make an ordinary control look like a half-declared heat one.
func TestThermalFieldsInsideInputAreNotTheControls(t *testing.T) {
	entry := writeDriver(t, `DRIVER = {
  id = "probe",
  controls = {
    { id = "c", input = { type = "number", min = 0, max = 1, zone = "z1" } },
  },
}
`)
	if len(entry.Controls) != 1 || entry.Controls[0].Thermal != nil {
		t.Fatalf("controls = %+v, want one with no thermal", entry.Controls)
	}
}

func TestControlOptionsValidation(t *testing.T) {
	cases := map[string]struct {
		entry string
		keep  bool
	}{
		"plain string options": {`{ id = "c", input = { type = "string", options = { "a", "b" } } }`, true},
		"empty options":        {`{ id = "c", input = { type = "string", options = { } } }`, false},
		"duplicate option":     {`{ id = "c", input = { type = "string", options = { "a", "a" } } }`, false},
		"bad option token":     {`{ id = "c", input = { type = "string", options = { "Eco Mode" } } }`, false},
		"non-string option":    {`{ id = "c", input = { type = "string", options = { "a", 2 } } }`, false},
		"options on a number":  {`{ id = "c", input = { type = "number", min = 0, max = 1, options = { "a" } } }`, false},
		"options on a boolean": {`{ id = "c", input = { type = "boolean", options = { "a" } } }`, false},
		"options not a table":  {`{ id = "c", input = { type = "string", options = "a" } }`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			parsed := writeDriver(t, "DRIVER = {\n  id = \"probe\",\n  controls = {\n    "+tc.entry+",\n  },\n}\n")
			if got := len(parsed.Controls) == 1; got != tc.keep {
				t.Errorf("kept = %v, want %v: %+v", got, tc.keep, parsed.Controls)
			}
		})
	}
}

// One bad sensor is dropped on its own; it does not hide the others.
func TestSensorsDropEachMalformedEntry(t *testing.T) {
	entry := writeDriver(t, `DRIVER = {
  id = "probe",
  sensors = {
    { metric = "room_temp_c", asset = "space_heat" },
    { metric = "Room Temp", asset = "space_heat" },
    { metric = "tank_temp_c", asset = "sauna" },
    { asset = "dhw" },
    { metric = "room_temp_c", asset = "cooling" },
    { metric = "zone_temp_c", asset = "space_heat", zone = "Bad Zone" },
    { metric = "tank_temp_c", asset = "dhw" },
  },
}
`)
	want := []CatalogSensor{
		{Metric: "room_temp_c", Asset: thermal.AssetSpaceHeat},
		{Metric: "tank_temp_c", Asset: thermal.AssetDHW},
	}
	if !reflect.DeepEqual(entry.Sensors, want) {
		t.Errorf("sensors = %+v, want %+v", entry.Sensors, want)
	}
}

// A field whose name only ends in "sensors" or "controls" is not that list.
func TestNestedBlockNeedsTheWholeName(t *testing.T) {
	entry := writeDriver(t, `DRIVER = {
  id = "probe",
  temp_sensors = { { metric = "room_temp_c", asset = "space_heat" } },
  my_controls = { { id = "c", input = { type = "boolean" } } },
}
`)
	if entry.Sensors != nil || entry.Controls != nil {
		t.Errorf("sensors = %+v, controls = %+v, want neither", entry.Sensors, entry.Controls)
	}
}

func TestSensorsIgnoreCommentedDeclarations(t *testing.T) {
	entry := writeDriver(t, `DRIVER = {
  id = "probe",
  -- sensors = { { metric = "room_temp_c", asset = "space_heat" } },
}
`)
	if entry.Sensors != nil {
		t.Errorf("sensors = %+v, want nil", entry.Sensors)
	}
}
