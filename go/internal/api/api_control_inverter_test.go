package api

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A hybrid's inverter reports on its inverter stream how much more it will
// take from or give to the grid now. A battery short of its order while
// that headroom is gone is limited by the inverter, not out of control.
func TestInverterHeadroomExplainsAShortfall(t *testing.T) {
	for _, tc := range []struct {
		name, battery, inverter string
		orderW, w               float64
		status, reason, kind    string
		limitW                  float64
	}{
		{"no import room while charging", "", `{"W":4300,"available_import_W":0,"available_export_W":5000}`, 5000, 3600, "limited", "battery_limit", "inverter_import", 0},
		{"import room within the tolerance", "", `{"W":4300,"available_import_W":60}`, 5000, 3600, "limited", "battery_limit", "inverter_import", 60},
		{"no export room while discharging", "", `{"W":-3200,"available_export_W":0}`, -5000, -3150, "limited", "battery_limit", "inverter_export", 0},
		{"no response, no import room", "", `{"W":800,"available_import_W":0}`, 5000, 0, "limited", "battery_limit", "inverter_import", 0},
		{"no response, import room left", "", `{"W":800,"available_import_W":3000}`, 5000, 0, "not_following", "no_power_response", "", 0},
		{"no response, room just above the tolerance", "", `{"available_export_W":150}`, -5000, 0, "not_following", "no_power_response", "", 0},
		{"import room left", "", `{"W":4300,"available_import_W":1400}`, 5000, 3600, "not_following", "power_below_target", "", 0},
		{"room just above the tolerance", "", `{"available_import_W":150}`, 5000, 3600, "not_following", "power_below_target", "", 0},
		{"export room gone while charging", "", `{"available_import_W":2000,"available_export_W":0}`, 5000, 3600, "not_following", "power_below_target", "", 0},
		{"import room gone while discharging", "", `{"available_import_W":0,"available_export_W":2000}`, -5000, -3150, "not_following", "power_below_target", "", 0},
		{"headroom left out", "", `{"W":4300}`, 5000, 3600, "not_following", "power_below_target", "", 0},
		{"headroom out of range", "", `{"W":4300,"available_import_W":-10}`, 5000, 3600, "not_following", "power_below_target", "", 0},
		{"no inverter stream", "", ``, 5000, 3600, "not_following", "power_below_target", "", 0},
		// The battery's own report says more, and comes first.
		{"battery reports its limit too", `,"available_charge_W":3600`, `{"available_import_W":0}`, 5000, 3600, "limited", "battery_limit", "battery_reported", 3600},
		{"standby", `,"device_state":"standby"`, `{"available_export_W":0}`, -5000, 0, "not_following", "device_standby", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newSungrowTrace()
				s.houseW = 800
				s.battery = func() (float64, float64, float64, string) { return tc.w, math.Abs(tc.w) / 190, 190, tc.battery }
				s.inverter = func() string { return tc.inverter }
				var last ControlFeedback
				s.run(90*time.Second, tc.orderW, func(f ControlFeedback) { last = f })
				if last.Status != tc.status || last.Reason != tc.reason || last.LimitKind != tc.kind {
					t.Fatalf("got %s/%s/%s %s, want %s/%s %s", last.Status, last.Reason, last.Severity, last.LimitKind, tc.status, tc.reason, tc.kind)
				}
				switch {
				case tc.reason == "battery_limit":
					if last.Severity != "info" || last.LimitSource != "device" || last.LimitW == nil || *last.LimitW != tc.limitW || last.LimitRoomW != nil {
						t.Fatalf("limit fields %s", jsonOf(last))
					}
				case last.LimitW != nil || last.LimitSource != "":
					t.Fatalf("unexplained shortfall carries a limit: %s", jsonOf(last))
				}
			})
		})
	}
}

// Headroom from an earlier poll says nothing about now. When the inverter
// read starts failing while the battery keeps reporting, the shortfall is
// not following again at once.
func TestStaleInverterStreamIsIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newSungrowTrace()
		s.houseW = 800
		s.battery = func() (float64, float64, float64, string) { return 3600, 3600 / 190.0, 190, "" }
		inverter := `{"W":4300,"available_import_W":0}`
		s.inverter = func() string { return inverter }
		var last ControlFeedback
		s.run(90*time.Second, 5000, func(f ControlFeedback) { last = f })
		if last.Reason != "battery_limit" || last.LimitKind != "inverter_import" {
			t.Fatalf("fresh headroom: %s", jsonOf(last))
		}
		inverter = ""
		var seen []string
		s.run(30*time.Second, 5000, func(f ControlFeedback) { seen = append(seen, f.Status+"/"+f.Reason) })
		for _, got := range seen {
			if got != "not_following/power_below_target" {
				t.Fatalf("old headroom still explained the shortfall: %v", seen)
			}
		}
		if _, ok := s.tel.Inverter("sungrow"); !ok {
			t.Fatal("the last inverter reading should stay for the driver views")
		}
	})
}

// Headroom explains only a shortfall in the right direction. Every other
// verdict keeps its own reason, however empty the inverter says it is.
func TestInverterHeadroomNeverExplainsOtherVerdicts(t *testing.T) {
	const none = `{"W":0,"available_import_W":0,"available_export_W":0}`
	for _, tc := range []struct {
		name, extra  string
		orderW       float64
		before, then func(s *sungrowTrace)
		reasons      []string // any of these
	}{
		{"setpoint changed", `,"setpoint_w":-2000`, -3000, nil, nil, []string{"setpoint_changed"}},
		{"power above target", "", -1500, nil, nil, []string{"power_above_target"}},
		{"power while idle", "", 0, nil, nil, []string{"power_while_idle"}},
		{"wrong direction", "", 3000, nil, nil, []string{"power_wrong_direction"}},
		{"device fault", "", -3000, func(s *sungrowTrace) { s.tel.SetDriverDeviceFault("sungrow", true, "test fault") }, nil, []string{"device_fault"}},
		{"readings lost", "", -2000, nil, func(s *sungrowTrace) {
			for range 24 {
				time.Sleep(5 * time.Second)
				s.srv.JournalVerdicts(time.Now())
			}
		}, []string{"readings_lost"}},
		{"driver says offline", `,"is_online":false`, -3000, nil, nil, []string{"telemetry_stale", "readings_lost"}},
		{"driver offline", "", -3000, nil, func(s *sungrowTrace) {
			s.tel.DriverHealthMut("sungrow").SetOffline()
		}, []string{"telemetry_stale", "readings_lost"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newSungrowTrace()
				s.houseW = 800
				s.battery = func() (float64, float64, float64, string) { return -2000, 2000 / 190.0, 190, tc.extra }
				s.inverter = func() string { return none }
				if tc.before != nil {
					tc.before(s)
				}
				var last ControlFeedback
				s.run(10*time.Minute, tc.orderW, func(f ControlFeedback) { last = f })
				if tc.then != nil {
					tc.then(s)
					last = s.batteryRow(time.Now())
				}
				if !slices.Contains(tc.reasons, last.Reason) || last.LimitSource != "" || last.LimitW != nil || last.Status == "limited" {
					t.Fatalf("got %s/%s/%s %s, want one of %v and no limit", last.Status, last.Reason, last.Severity, jsonOf(last), tc.reasons)
				}
			})
		})
	}
}

// The status and driver views show the inverter stream beside the driver's
// readings. The site's grid, solar, battery and load stay what the DER
// readings say.
func TestStatusShowsTheInverterStreamApartFromSitePower(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tel := telemetry.NewStore()
	soc := 0.5
	tel.Update("solis", telemetry.DerMeter, 1800, nil, []byte(`{"power_origin":"external_meter"}`))
	tel.Update("solis", telemetry.DerBattery, 3600, &soc, nil)
	tel.Update("solis", telemetry.DerPV, -2400, nil, nil)
	tel.RecordDriverSuccess("solis")
	srv := New(&Deps{State: st, Tel: tel, Ctrl: &control.State{SiteMeterDriver: "solis"}, CtrlMu: &sync.Mutex{}, CapMu: &sync.RWMutex{},
		CfgMu: &sync.RWMutex{}, Cfg: &config.Config{}})
	status := func() map[string]any {
		rr := httptest.NewRecorder()
		srv.handleStatus(rr, httptest.NewRequest("GET", "/api/status", nil))
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("status: %v %s", err, rr.Body.String())
		}
		return body
	}
	before := status()
	r, err := telemetry.ParseInverterReading([]byte(`{"W":-9000,"available_import_W":0,"available_export_W":4100,"Hz":50}`))
	if err != nil {
		t.Fatal(err)
	}
	tel.UpdateInverter("solis", r)
	after := status()
	for _, key := range []string{"grid_w", "pv_w", "bat_w", "load_w", "ev_w"} {
		if before[key] != after[key] {
			t.Fatalf("%s moved: %v -> %v", key, before[key], after[key])
		}
	}
	d, _ := after["drivers"].(map[string]any)["solis"].(map[string]any)
	if d["inverter_w"] != -9000.0 || d["inverter_available_import_w"] != 0.0 || d["inverter_available_export_w"] != 4100.0 || d["bat_w"] == nil {
		t.Fatalf("driver status %v", d)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/drivers/solis", nil)
	req.SetPathValue("name", "solis")
	srv.handleDriverDetail(rr, req)
	var detail driverDetailResp
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Inverter == nil || detail.Inverter.W == nil || *detail.Inverter.W != -9000 || detail.Inverter.AvailableImportW == nil ||
		*detail.Inverter.AvailableImportW != 0 || detail.Inverter.Stale || detail.Inverter.UpdatedAt == 0 || len(detail.Readings) != 3 {
		t.Fatalf("driver detail %s", rr.Body.String())
	}
}
