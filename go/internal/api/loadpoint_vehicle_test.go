package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func vehicleServer(t *testing.T) (*Server, *loadpoint.Manager) {
	t.Helper()
	srv, _, cfg := postConfigServer(t, nil)
	cfg.Site.Name = "Home"
	cfg.Drivers = []config.Driver{{Name: "easee", Lua: "/app/drivers/easee.lua", Config: map[string]any{"token": "preserve-me"}}}
	cfg.Loadpoints = []config.Loadpoint{{ID: "garage", DriverName: "easee", VehicleCapacityWh: 60000, MaxChargeW: 11000}, {ID: "guest", DriverName: "other", VehicleCapacityWh: 45000}}
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee", VehicleCapacityWh: 60000, MaxChargeW: 11000}, {ID: "guest", DriverName: "other", VehicleCapacityWh: 45000}})
	mgr.Observe("garage", true, 7000, 0, true)
	mgr.Observe("garage", true, 7000, 6000, true)
	mgr.SetCurrentSoC("garage", 0.42)
	srv.deps.Loadpoints = mgr
	srv.deps.SaveConfig = config.SaveAtomic
	return srv, mgr
}

func postVehicle(srv *Server, id, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/"+id+"/vehicle", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func TestVehicleCapacitySaveReadbackAndSession(t *testing.T) {
	srv, mgr := vehicleServer(t)
	beforeDrivers := srv.deps.Cfg.Drivers
	beforeGuest := srv.deps.Cfg.Loadpoints[1]
	rr := postVehicle(srv, "garage", `{"capacity_wh":77400}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var result struct {
		OK       bool    `json:"ok"`
		Capacity float64 `json:"vehicle_capacity_wh"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil || !result.OK || result.Capacity != 77400 {
		t.Fatalf("response %s: %v", rr.Body.String(), err)
	}
	if !reflect.DeepEqual(beforeDrivers, srv.deps.Cfg.Drivers) || beforeGuest.VehicleCapacityWh != srv.deps.Cfg.Loadpoints[1].VehicleCapacityWh || srv.deps.Cfg.Site.Name != "Home" {
		t.Fatal("unrelated settings changed")
	}
	raw, err := os.ReadFile(srv.deps.ConfigPath)
	if err != nil || !strings.Contains(string(raw), "vehicle_capacity_wh: 77400") || !strings.Contains(string(raw), "preserve-me") {
		t.Fatalf("saved config lost data: %v", err)
	}
	state, _ := mgr.State("garage")
	if state.VehicleCapacityWh != 77400 || math.Abs(state.CurrentSoC-0.42) > 0.000001 || state.DeliveredWhSession != 6000 {
		t.Fatalf("capacity edit changed current session: %+v", state)
	}
	read := httptest.NewRecorder()
	srv.Handler().ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/api/loadpoints", nil))
	if read.Code != 200 || !strings.Contains(read.Body.String(), `"vehicle_capacity_wh":77400`) {
		t.Fatalf("readback: %s", read.Body.String())
	}
}

func TestVehicleCapacityFailedSaveDoesNotApply(t *testing.T) {
	srv, mgr := vehicleServer(t)
	called := false
	srv.deps.ConfigApplier = func(_, _ *config.Config) { called = true }
	srv.deps.SaveConfig = func(string, *config.Config) error { return errors.New("disk full") }
	rr := postVehicle(srv, "garage", `{"capacity_wh":80000}`)
	state, _ := mgr.State("garage")
	if rr.Code != 500 || called || state.VehicleCapacityWh != 60000 || srv.deps.Cfg.Loadpoints[0].VehicleCapacityWh != 60000 {
		t.Fatalf("failed save reached runtime: status %d, callback %v, capacity %v", rr.Code, called, state.VehicleCapacityWh)
	}
}

func TestVehicleCapacityUsesSharedApplier(t *testing.T) {
	srv, _ := vehicleServer(t)
	var oldCapacity, nextCapacity float64
	srv.deps.ConfigApplier = func(next, old *config.Config) {
		oldCapacity = old.Loadpoints[0].VehicleCapacityWh
		nextCapacity = next.Loadpoints[0].VehicleCapacityWh
	}
	if rr := postVehicle(srv, "garage", `{"capacity_wh":80000}`); rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	if oldCapacity != 60000 || nextCapacity != 80000 {
		t.Fatalf("wrong apply snapshots: %v -> %v", oldCapacity, nextCapacity)
	}
}

func TestVehicleCapacityValidatesAndIsConfigure(t *testing.T) {
	srv, _ := vehicleServer(t)
	for _, body := range []string{``, `{`, `{}`, `{"capacity_wh":0}`, `{"capacity_wh":999}`, `{"capacity_wh":300001}`, `{"capacity_wh":1e1000}`, `{"capacity_wh":"80000"}`} {
		if rr := postVehicle(srv, "garage", body); rr.Code != 400 {
			t.Fatalf("body %q gave %d", body, rr.Code)
		}
	}
	if rr := postVehicle(srv, "unknown", `{"capacity_wh":80000}`); rr.Code != 404 {
		t.Fatalf("unknown id gave %d", rr.Code)
	}
	for _, body := range []string{`{"capacity_wh":1000}`, `{"capacity_wh":300000}`} {
		if rr := postVehicle(srv, "garage", body); rr.Code != 200 {
			t.Fatalf("boundary %q gave %d", body, rr.Code)
		}
	}
	facts := srv.Route(httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/vehicle", nil))
	if facts.Tier != apiauth.TierConfigure || facts.ReplacesAll {
		t.Fatalf("route price: %+v", facts)
	}
}
