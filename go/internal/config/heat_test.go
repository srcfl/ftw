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
	ok := HeatZone{Name: "a", Sensor: "d/m", TargetC: 22, MinC: 20, MaxC: 24}
	for name, tc := range map[string]struct {
		h    Heat
		want string
	}{
		"no name":          {Heat{Zones: []HeatZone{{Sensor: "d/m", TargetC: 22, MinC: 20, MaxC: 24}}}, "name"},
		"twice":            {Heat{Zones: []HeatZone{ok, ok}}, "twice"},
		"bad sensor":       {Heat{Zones: []HeatZone{{Name: "a", Sensor: "dm", TargetC: 22, MinC: 20, MaxC: 24}}}, "sensor"},
		"bad control":      {Heat{Zones: []HeatZone{{Name: "a", Sensor: "d/m", Control: "x", TargetC: 22, MinC: 20, MaxC: 24}}}, "control"},
		"target outside":   {Heat{Zones: []HeatZone{{Name: "a", Sensor: "d/m", TargetC: 25, MinC: 20, MaxC: 24}}}, "min_c"},
		"no band":          {Heat{Zones: []HeatZone{{Name: "a", Sensor: "d/m", TargetC: 22, MinC: 22, MaxC: 22}}}, "band"},
		"bad gate":         {Heat{Savings: &heatplan.SavingsGate{MinPerDay: 5, KeepShare: 2}, Zones: []HeatZone{ok}}, "savings"},
		"bad relief":       {Heat{Zones: []HeatZone{{Name: "a", Sensor: "d/m", TargetC: 22, MinC: 20, MaxC: 24, PeakRelief: &heatplan.PeakRelief{MaxHours: 3, Factor: 1, WindowH: 12}}}}, "peak_relief"},
		"write no control": {Heat{Write: true, Zones: []HeatZone{ok}}, "control"},
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
