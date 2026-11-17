package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/srcfl/ftw/go/internal/heatplan"
)

// The numbers the house that built the planner runs, as its owner would write
// them. Every one of them is the owner's, and none has a default.
const houseHeatYAML = `
write: false
savings: { min_per_day: 500, keep_share: 0.8 }
zones:
  - name: badrum
    sensor: wiser_7/floor_temp_c
    control: wiser_7/set_setpoint
    boundary: wiser_7/room_temp_c
    source: direct_electric
    heat_kw: 0.6
    target_c: 24.5
    min_c: 20
    max_c: 27
    price_comfort: { cheap_price: 150, normal_price: 260, dear_price: 400, raise_k: 1, lower_k: 1, step_k: 0.5 }
    peak_relief: { min_price: 200, factor: 3, window_h: 12, max_hours: 3 }
`

func TestHeatSectionReadsTheOwnersNumbers(t *testing.T) {
	var h Heat
	if err := yaml.Unmarshal([]byte(houseHeatYAML), &h); err != nil {
		t.Fatal(err)
	}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	z := h.Zones[0]
	if h.Write || h.Savings.MinPerDay != 500 || h.Savings.KeepShare != 0.8 ||
		z.MaxC != 27 || z.PriceComfort.DearPrice != 400 || z.PeakRelief.MaxHours != 3 {
		t.Fatalf("numbers were not read: %+v %+v", h, z)
	}
	var _ heatplan.PeakRelief = *z.PeakRelief
}

func TestHeatSectionLeftOutOrEmptyChangesNothing(t *testing.T) {
	if (*Heat)(nil).Validate() != nil || (&Heat{}).Validate() != nil {
		t.Fatal("an absent or empty heat section was refused")
	}
}

func TestHeatSectionRefusesWhatCannotBeReadOneWay(t *testing.T) {
	ok := HeatZone{Name: "a", Sensor: "d/m", Boundary: "outdoor", Source: "direct_electric", HeatKW: 1, TargetC: 22, MinC: 20, MaxC: 24}
	for name, tc := range map[string]struct {
		h    Heat
		want string
	}{
		"no name":          {Heat{Zones: []HeatZone{{Sensor: "d/m", TargetC: 22, MinC: 20, MaxC: 24}}}, "name"},
		"twice":            {Heat{Zones: []HeatZone{ok, ok}}, "twice"},
		"bad sensor":       {Heat{Zones: []HeatZone{{Name: "a", Sensor: "dm", TargetC: 22, MinC: 20, MaxC: 24}}}, "sensor"},
		"bad control":      {Heat{Zones: []HeatZone{{Name: "a", Sensor: "d/m", Control: "x", TargetC: 22, MinC: 20, MaxC: 24}}}, "control"},
		"target outside":   {Heat{Zones: []HeatZone{with(ok, func(z *HeatZone) { z.TargetC = 25 })}}, "min_c"},
		"no band":          {Heat{Zones: []HeatZone{with(ok, func(z *HeatZone) { z.MinC, z.TargetC, z.MaxC = 22, 22, 22 })}}, "band"},
		"bad gate":         {Heat{Savings: &heatplan.SavingsGate{MinPerDay: 5, KeepShare: 2}, Zones: []HeatZone{ok}}, "savings"},
		"bad relief":       {Heat{Zones: []HeatZone{with(ok, func(z *HeatZone) { z.PeakRelief = &heatplan.PeakRelief{MaxHours: 3, Factor: 1, WindowH: 12} })}}, "peak_relief"},
		"write no control": {Heat{Write: true, Zones: []HeatZone{ok}}, "control"},
		"bad boundary":     {Heat{Zones: []HeatZone{with(ok, func(z *HeatZone) { z.Boundary = "" })}}, "boundary"},
		"no heat":          {Heat{Zones: []HeatZone{with(ok, func(z *HeatZone) { z.HeatKW = 0 })}}, "heat_kw"},
		"bad heat input":   {Heat{Zones: []HeatZone{with(ok, func(z *HeatZone) { z.HeatInput = "ctc" })}}, "heat_input"},
		"unknown source":   {Heat{Zones: []HeatZone{with(ok, func(z *HeatZone) { z.Source = "gas" })}}, "source"},
		"pump on electric": {Heat{Zones: []HeatZone{with(ok, func(z *HeatZone) { z.HeatPump = &HeatPump{} })}}, "heat_pump"},
		"bad curve": {Heat{Zones: []HeatZone{with(ok, func(z *HeatZone) {
			z.Source = "heat_pump"
			z.HeatPump = &HeatPump{Supply: &HeatSupplyCurve{ColdOutdoorC: 15, ColdSupplyC: 45, MildOutdoorC: -15, MildSupplyC: 25}}
		})}}, "heat_pump"},
		"bad effect": {Heat{Zones: []HeatZone{with(ok, func(z *HeatZone) { z.CurveEffect = &CurveEffect{ZoneKPerUnit: 0} })}}, "curve_effect"},
	} {
		err := tc.h.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one mentioning %q", name, err, tc.want)
		}
	}
	w := ok
	w.Control = "d/set_setpoint"
	if err := (&Heat{Write: true, Zones: []HeatZone{w}}).Validate(); err != nil {
		t.Errorf("a zone with a control and write on was refused: %v", err)
	}
}

func with(z HeatZone, f func(*HeatZone)) HeatZone { f(&z); return z }

// The house the planner was built for: a CTC with an air-to-water pump on
// a heat curve, the rooms read by an underfloor gateway.
const houseHeatPumpYAML = `
zones:
  - name: house
    sensor: LK/zone_2_temp_c
    heat_input: CTC/space_heat_w
    boundary: outdoor
    source: heat_pump
    heat_kw: 6
    target_c: 21
    min_c: 20
    max_c: 22.5
    heat_pump:
      carnot_fraction: 0.45
      supply: { cold_outdoor_c: -20, cold_supply_c: 45, mild_outdoor_c: 15, mild_supply_c: 24 }
    curve_effect: { zone_k_per_unit: 0.5, supply_k_per_unit: 1 }
`

func TestHeatSectionReadsAHeatPump(t *testing.T) {
	var h Heat
	if err := yaml.Unmarshal([]byte(houseHeatPumpYAML), &h); err != nil {
		t.Fatal(err)
	}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	src := h.Zones[0].PlanSource()
	if src.Kind != heatplan.SourceHeatPump || src.CarnotFraction != 0.45 || src.Supply == nil || src.Supply.At(-20) != 45 {
		t.Fatalf("source = %+v", src)
	}
	if h.Zones[0].CurveEffect.ZoneKPerUnit != 0.5 || h.Zones[0].HeatInput != "CTC/space_heat_w" {
		t.Fatalf("zone = %+v", h.Zones[0])
	}
}
