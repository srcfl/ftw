package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

const controlOptionsProbeLua = `DRIVER = {
  id      = "probe_options",
  name    = "Probe options",
  version = "1.0.0",
  controls = {
    {
      id = "set_mode", asset = "dhw", strategy = "discrete_mode",
      input = { type = "string", options = { "eco", "boost" } },
    },
  },
  sensors = {
    { metric = "tank_temp_c", asset = "dhw" },
  },
}

local applied = 0

function driver_init(config)
    host.set_make("Probe options")
    host.set_poll_interval(100)
end

function driver_poll()
    host.emit_metric("applied", applied, "n")
    return 100
end

function driver_command(action, power_w, cmd)
    if action == "set_mode" then
        if cmd.value == "eco" then applied = 1 elseif cmd.value == "boost" then applied = 2 else applied = 9 end
        return true
    end
    return false
end

function driver_default_mode()
    applied = 0
end
`

// A declared closed set is the allowlist. A value outside it is a 400 and
// never reaches the driver, which would otherwise have to guess.
func TestDriverControlRefusesValueOutsideDeclaredOptions(t *testing.T) {
	srv, tel := controlServerWithLua(t, controlOptionsProbeLua)

	rec := post(t, srv, "/api/drivers/heat/control", `{"control":"set_mode","value":"turbo","duration_s":600}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("undeclared option = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if hold := srv.activeControlHold("heat"); hold != nil {
		t.Fatalf("hold = %+v after a refused value, want none", hold)
	}
	// Give the driver a few polls; a refused value must not have been sent.
	time.Sleep(300 * time.Millisecond)
	if v, _, ok := tel.LatestMetric("heat", "applied"); ok && v == 9 {
		t.Fatalf("the refused value reached the driver")
	}

	rec = post(t, srv, "/api/drivers/heat/control", `{"control":"set_mode","value":"boost","duration_s":600}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("declared option = %d, body %s", rec.Code, rec.Body.String())
	}
	waitMetric(t, tel, "heat", "applied", 2)
}

func TestDriverDetailSurfacesThermalDeclaration(t *testing.T) {
	srv := serverWithDrivers(t,
		map[string]string{"probe.lua": controlOptionsProbeLua},
		&config.Config{Drivers: []config.Driver{{Name: "tank", Lua: "probe.lua"}}})

	got := driverDetail(t, srv, "tank")
	if len(got.Controls) != 1 || got.Controls[0].Thermal == nil {
		t.Fatalf("controls = %+v, want one thermal control", got.Controls)
	}
	th := got.Controls[0].Thermal
	if th.Asset != "dhw" || th.Strategy != "discrete_mode" {
		t.Errorf("thermal = %+v", th)
	}
	if opts := got.Controls[0].Input.Options; len(opts) != 2 || opts[0] != "eco" || opts[1] != "boost" {
		t.Errorf("options = %v", opts)
	}
	if len(got.Sensors) != 1 || got.Sensors[0].Metric != "tank_temp_c" || got.Sensors[0].Asset != "dhw" {
		t.Errorf("sensors = %+v", got.Sensors)
	}
}

// A driver that declares no heat role keeps its JSON exactly as before: no
// sensors key, and no thermal key on its controls.
func TestDriverDetailOmitsThermalKeysWhenNoneDeclared(t *testing.T) {
	srv := serverWithDrivers(t,
		map[string]string{"probe.lua": controlDriverLua},
		&config.Config{Drivers: []config.Driver{{Name: "heat", Lua: "probe.lua"}}})

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/drivers/heat", nil))
	var raw struct {
		Sensors  json.RawMessage   `json:"sensors"`
		Controls []json.RawMessage `json:"controls"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if raw.Sensors != nil {
		t.Errorf("sensors key present: %s", rec.Body.String())
	}
	if len(raw.Controls) != 1 {
		t.Fatalf("controls = %d, want 1", len(raw.Controls))
	}
	var control map[string]any
	if err := json.Unmarshal(raw.Controls[0], &control); err != nil {
		t.Fatal(err)
	}
	if _, present := control["thermal"]; present {
		t.Errorf("thermal key present on a control that declares none: %s", raw.Controls[0])
	}
}
