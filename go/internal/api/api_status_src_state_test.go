package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A driver on a wrong port has polled and failed from the first second, while
// its emit status still reads ok. src_state says what the apps say: it has
// never answered. The overview names it and keeps LIVE dark.
func TestHandleStatusGradesEachDriverAsTheAppsDo(t *testing.T) {
	tel := telemetry.NewStore()
	tel.Update("ferroamp", telemetry.DerMeter, 300, nil, nil)
	tel.RecordDriverSuccess("ferroamp")
	tel.DriverHealthMut("sungrow").RecordError("driver_poll: none of 17 modbus reads returned data")
	tel.Update("easee", telemetry.DerEV, 0, nil, nil)
	tel.RecordDriverSuccess("easee")
	tel.DriverHealthMut("easee").SetOffline()

	srv := New(&Deps{
		Tel:        tel,
		Ctrl:       &control.State{SiteMeterDriver: "ferroamp"},
		CtrlMu:     &sync.Mutex{},
		CapMu:      &sync.RWMutex{},
		Capacities: map[string]float64{},
		CfgMu:      &sync.RWMutex{},
		Cfg: &config.Config{Drivers: []config.Driver{
			{Name: "ferroamp"}, {Name: "sungrow"}, {Name: "easee"},
			{Name: "zaptec"}, {Name: "old", Disabled: true},
		}},
	})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Drivers map[string]map[string]any `json:"drivers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"ferroamp": "live",
		"sungrow":  "never", // status still says ok
		"easee":    "down",
		"zaptec":   "never", // configured, did not start
	}
	for name, state := range want {
		if got := resp.Drivers[name]["src_state"]; got != state {
			t.Errorf("%s src_state = %v, want %s (row %v)", name, got, state, resp.Drivers[name])
		}
	}
	if got := resp.Drivers["sungrow"]["status"]; got != "ok" {
		t.Fatalf("sungrow status = %v; this test needs the emit status to still say ok", got)
	}
	if _, ok := resp.Drivers["old"]["src_state"]; ok {
		t.Errorf("a disabled driver is not a source: %v", resp.Drivers["old"])
	}
	if age, ok := resp.Drivers["ferroamp"]["last_ok_age_ms"].(float64); !ok || age < 0 || age > 5_000 {
		t.Errorf("ferroamp last_ok_age_ms = %v, want a fresh age", resp.Drivers["ferroamp"]["last_ok_age_ms"])
	}
	if _, ok := resp.Drivers["sungrow"]["last_ok_age_ms"]; ok {
		t.Errorf("a driver that never answered has no age: %v", resp.Drivers["sungrow"])
	}
}
