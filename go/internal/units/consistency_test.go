package units_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/srcfl/ftw/go/internal/calendar"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/forecast"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/pvperf"
	"github.com/srcfl/ftw/go/internal/roofmodel"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/internal/units"
	"github.com/srcfl/ftw/go/internal/v2x"
)

// These tests are the harness: if a later change puts kWp or 0–100 SoC
// back into core structs, they fail. Doors (optimizer JSON, appproto)
// are allowed their own types.

func TestPVArrayCoreFieldIsRatedWatts(t *testing.T) {
	typ := reflect.TypeOf(config.PVArray{})
	if _, ok := typ.FieldByName("RatedW"); !ok {
		t.Fatal("config.PVArray must store RatedW (watts)")
	}
	f, ok := typ.FieldByName("KWp")
	if ok {
		tag := f.Tag.Get("yaml")
		if tag != "-" && tag != "kwp,omitempty" {
			t.Fatalf("legacy KWp must be omitempty yaml only, got %q", tag)
		}
	}
}

func TestForecastArrayHasNoKWp(t *testing.T) {
	typ := reflect.TypeOf(forecast.Array{})
	if _, ok := typ.FieldByName("KWp"); ok {
		t.Fatal("forecast.Array must not have KWp; store RatedW and convert at the forecast.solar URL")
	}
	if _, ok := typ.FieldByName("RatedW"); !ok {
		t.Fatal("forecast.Array must store RatedW (watts)")
	}
}

func TestPVPerfArrayHasNoKWp(t *testing.T) {
	typ := reflect.TypeOf(pvperf.Array{})
	if _, ok := typ.FieldByName("KWp"); ok {
		t.Fatal("pvperf.Array must not have KWp; store RatedW")
	}
	if _, ok := typ.FieldByName("RatedW"); !ok {
		t.Fatal("pvperf.Array must store RatedW (watts)")
	}
}

func TestRoofmodelArrayHasNoKWp(t *testing.T) {
	typ := reflect.TypeOf(roofmodel.Array{})
	if _, ok := typ.FieldByName("KWp"); ok {
		t.Fatal("roofmodel.Array must not have KWp; store RatedW")
	}
	if _, ok := typ.FieldByName("RatedW"); !ok {
		t.Fatal("roofmodel.Array must store RatedW (watts)")
	}
}

func TestMPCParamsSoCIsFraction(t *testing.T) {
	typ := reflect.TypeOf(mpc.Params{})
	for _, banned := range []string{"SoCMinPct", "SoCMaxPct", "InitialSoCPct"} {
		if _, ok := typ.FieldByName(banned); ok {
			t.Fatalf("mpc.Params.%s must not exist; use 0–1 SoCMin/SoCMax/InitialSoC", banned)
		}
	}
	for _, want := range []string{"SoCMin", "SoCMax", "InitialSoC"} {
		if _, ok := typ.FieldByName(want); !ok {
			t.Fatalf("mpc.Params.%s missing", want)
		}
	}
}

func TestActionJSONSoCIsFractionNotPercent(t *testing.T) {
	a := mpc.Action{SoC: 0.55, LoadpointSoC: 0.80}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["soc_pct"]; ok {
		t.Fatalf("Action JSON still emits soc_pct: %s", raw)
	}
	if _, ok := m["loadpoint_soc_pct"]; ok {
		t.Fatalf("Action JSON still emits loadpoint_soc_pct: %s", raw)
	}
	soc, ok := m["soc"].(float64)
	if !ok || soc != 0.55 {
		t.Fatalf("soc = %v (%T), want 0.55; json=%s", m["soc"], m["soc"], raw)
	}
	if !units.ValidFraction(soc) {
		t.Fatalf("soc %v is not a 0–1 fraction", soc)
	}
}

func TestPlanJSONSoCIsFraction(t *testing.T) {
	p := mpc.Plan{InitialSoC: 0.42, Actions: []mpc.Action{{SoC: 0.5}}}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["initial_soc_pct"]; ok {
		t.Fatalf("Plan JSON still emits initial_soc_pct: %s", raw)
	}
	got, ok := m["initial_soc"].(float64)
	if !ok || got != 0.42 {
		t.Fatalf("plan initial_soc = %v, want 0.42; json=%s", m["initial_soc"], raw)
	}
}

func TestNameplateSumsRatedWatts(t *testing.T) {
	arrays := []forecast.Array{
		{TiltDeg: 27, AzimuthDeg: 150, RatedW: 12960},
		{TiltDeg: 27, AzimuthDeg: 240, RatedW: 6000},
	}
	if got := forecast.NameplateW(18960, arrays); got != 18960 {
		t.Fatalf("nameplate = %v, want 18960 W", got)
	}
}

func TestLoadpointStateJSONIsFractionNotPercent(t *testing.T) {
	st := loadpoint.State{CurrentSoC: 0.42, TargetSoC: 0.80, VehicleSoC: 0.41, VehicleChargeLimit: 0.60}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"current_soc_pct", "target_soc_pct", "vehicle_soc_pct", "vehicle_charge_limit_pct"} {
		if _, ok := m[banned]; ok {
			t.Fatalf("loadpoint.State JSON still emits %s: %s", banned, raw)
		}
	}
	if m["current_soc"] != 0.42 {
		t.Fatalf("current_soc = %v, want 0.42; json=%s", m["current_soc"], raw)
	}
	if !units.ValidFraction(m["current_soc"].(float64)) {
		t.Fatalf("current_soc is not a 0–1 fraction")
	}
}

func TestLoadpointScheduleJSONIsFraction(t *testing.T) {
	s := loadpoint.Schedule{SoC: 0.80, SurplusUnlockBatSoC: 0.75, TimeOfDayMinUTC: 360}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["soc_pct"]; ok {
		t.Fatalf("Schedule JSON still emits soc_pct: %s", raw)
	}
	if m["soc"] != 0.80 {
		t.Fatalf("soc = %v, want 0.80", m["soc"])
	}
	var back loadpoint.Schedule
	if err := json.Unmarshal([]byte(`{"soc_pct":80,"time_of_day_min_utc":360}`), &back); err != nil {
		t.Fatal(err)
	}
	if back.SoC != 0.80 {
		t.Fatalf("legacy soc_pct 80 must hydrate as 0.80, got %v", back.SoC)
	}
}

func TestLoadpointConfigHasNoPluginSoCPct(t *testing.T) {
	typ := reflect.TypeOf(loadpoint.Config{})
	if _, ok := typ.FieldByName("PluginSoCPct"); ok {
		t.Fatal("loadpoint.Config.PluginSoCPct must not exist; store PluginSoC as 0–1")
	}
	if _, ok := typ.FieldByName("PluginSoC"); !ok {
		t.Fatal("loadpoint.Config.PluginSoC missing")
	}
}

func TestVehiclePickSoCIsFraction(t *testing.T) {
	typ := reflect.TypeOf(telemetry.VehiclePick{})
	for _, banned := range []string{"SoCPct", "ChargeLimitPct"} {
		if _, ok := typ.FieldByName(banned); ok {
			t.Fatalf("telemetry.VehiclePick.%s must not exist; use SoC/ChargeLimit 0–1", banned)
		}
	}
}

func TestCalendarDeadlineSoCIsFraction(t *testing.T) {
	typ := reflect.TypeOf(calendar.EVDeadline{})
	if _, ok := typ.FieldByName("TargetSoCPct"); ok {
		t.Fatal("calendar.EVDeadline.TargetSoCPct must not exist; store TargetSoC as 0–1")
	}
	d := calendar.EVDeadline{TargetSoC: 0.80}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["target_soc_pct"]; ok {
		t.Fatalf("EVDeadline JSON still emits target_soc_pct: %s", raw)
	}
}

func TestV2XEnvelopeSoCJSONIsFraction(t *testing.T) {
	env := v2x.Envelope{MinReserveSoC: 0.35, DepartureTargetSoC: 0.80}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["min_reserve_soc_pct"]; ok {
		t.Fatalf("v2x Envelope JSON still emits min_reserve_soc_pct: %s", raw)
	}
	if m["min_reserve_soc"] != 0.35 {
		t.Fatalf("min_reserve_soc = %v, want 0.35", m["min_reserve_soc"])
	}
}

func TestShadowEvaluationJSONIsFraction(t *testing.T) {
	s := mpc.ShadowEvaluation{ChampionVirtualSoC: 0.55, ChallengerVirtualSoC: 0.45}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["champion_virtual_soc_pct"]; ok {
		t.Fatalf("shadow JSON still emits champion_virtual_soc_pct: %s", raw)
	}
	if m["champion_virtual_soc"] != 0.55 {
		t.Fatalf("champion_virtual_soc = %v, want 0.55", m["champion_virtual_soc"])
	}
}

func TestCoreBannedSoCPercentFieldNames(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeOf(loadpoint.State{}),
		reflect.TypeOf(loadpoint.Config{}),
		reflect.TypeOf(loadpoint.Schedule{}),
		reflect.TypeOf(mpc.Params{}),
		reflect.TypeOf(mpc.Action{}),
		reflect.TypeOf(mpc.Plan{}),
		reflect.TypeOf(mpc.SlotDirective{}),
		reflect.TypeOf(forecast.Array{}),
		reflect.TypeOf(pvperf.Array{}),
		reflect.TypeOf(roofmodel.Array{}),
	}
	banned := []string{"CurrentSoCPct", "TargetSoCPct", "PluginSoCPct", "VehicleSoCPct", "SoCPct", "SoCMinPct", "SoCMaxPct", "SoCTargetPct", "LivePVSurplusSoCCapPct", "LoadpointSoCTargetPct", "KWp"}
	for _, typ := range types {
		for _, name := range banned {
			if _, ok := typ.FieldByName(name); ok {
				t.Errorf("%s.%s must not exist in core", typ.String(), name)
			}
		}
	}
}
