package api

import (
	"net/http"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestDriverControlReportsExternalModbusOwnership(t *testing.T) {
	srv, _ := controlServerWithLuaConfig(t, controlProbeLua, config.Driver{
		Name: "heat", ObserveOnly: true,
		Capabilities: config.Capabilities{Modbus: &config.ModbusConfig{Host: "127.0.0.1", Port: 502, UnitID: 1}},
	})
	// The setting belongs to the endpoint; it is not a saved observe_only
	// flag on one driver that another driver could omit.
	srv.deps.Cfg.Drivers[0].ObserveOnly = false
	srv.deps.Cfg.ModbusProxy = &config.ModbusProxy{Enabled: true, AllowWrite: true}
	srv.deps.Cfg.PinModbusProxyOwnership()
	rec := post(t, srv, "/api/drivers/heat/control", `{"control":"set_offset","value":2,"duration_s":600}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("external owner POST = %d: %s", rec.Code, rec.Body.String())
	}
	srv.deps.Cfg.ModbusProxy.AllowWrite = false
	rec = post(t, srv, "/api/drivers/heat/control", `{"control":"set_offset","value":2,"duration_s":600}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("save returned ownership before restart: %d", rec.Code)
	}
}
