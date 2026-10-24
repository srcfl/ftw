package energywire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 24, 9, 30, 0, 0, time.UTC)

const valid = `{"schema":"ftw.energy/1","date":"2026-10-23","pv_generation_kwh":12,"grid_import_kwh":30,"grid_export_kwh":4,"battery_charge_kwh":9,"battery_discharge_kwh":8,"vehicle_charge_kwh":0}`

func TestOnlySixSumsAndADateAreAccepted(t *testing.T) {
	r, err := Decode([]byte(valid), now)
	if err != nil || r.PVGeneration != 12 || r.GridImport != 30 || r.Date != "2026-10-23" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{
		strings.Replace(valid, `"ftw.energy/1"`, `"ftw.energy/2"`, 1),
		strings.Replace(valid, `2026-10-23`, `2026-10-24`, 1), // today is still open
		strings.Replace(valid, `2026-10-23`, `2026-10-20`, 1), // four days back
		strings.Replace(valid, `2026-10-23`, `2026-10-32`, 1),
		strings.Replace(valid, `2026-10-23`, `2026-1-23`, 1),
		strings.Replace(valid, `:12,`, `:12.5,`, 1),
		strings.Replace(valid, `:12,`, `:1e1,`, 1),
		strings.Replace(valid, `:12,`, `:-1,`, 1),
		strings.Replace(valid, `:12,`, `:1001,`, 1),
		strings.Replace(valid, `:12,`, `:"12",`, 1),
		strings.Replace(valid, `,"vehicle_charge_kwh":0`, ``, 1),
		strings.TrimSuffix(valid, "}") + `,"home":"a"}`,
		strings.TrimSuffix(valid, "}") + `,"ftw_version":"v1.0.0"}`,
		strings.Replace(valid, `"vehicle_charge_kwh":0`, `"vehicle_charge_kwh":0,"vehicle_charge_kwh":0`, 1),
		valid + valid, valid + ` {}`, `[]`, `null`, strings.Repeat(" ", MaxBodyBytes) + valid,
	} {
		if _, err := Decode([]byte(bad), now); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestTheReportHasNoFieldThatNamesAHome(t *testing.T) {
	var fields map[string]any
	b, _ := json.Marshal(Report{})
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 8 || len(Fields) != 6 {
		t.Fatalf("fields %v", fields)
	}
	for _, name := range append([]string{"schema", "date"}, Fields...) {
		if _, ok := fields[name]; !ok {
			t.Fatalf("missing %s", name)
		}
	}
}

func TestOpenAndClosedDays(t *testing.T) {
	for date, want := range map[string][2]bool{
		"2026-10-24": {false, false},
		"2026-10-23": {true, false},
		"2026-10-21": {true, false},
		"2026-10-20": {false, true},
		"2026-09-01": {false, true},
		"2026-10-25": {false, false},
	} {
		if got := [2]bool{Accepts(date, now), Closed(date, now)}; got != want {
			t.Fatalf("%s: accepts, closed = %v, want %v", date, got, want)
		}
	}
}
