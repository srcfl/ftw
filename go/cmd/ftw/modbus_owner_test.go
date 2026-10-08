package main

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestExternalModbusOwnerKeepsTelemetryButLeavesPlannerAndDispatch(t *testing.T) {
	cfg := &config.Config{
		ModbusProxy: &config.ModbusProxy{Enabled: true, AllowWrite: true},
		Drivers: []config.Driver{{Name: "sungrow", Lua: "drivers/sungrow.lua", BatteryCapacityWh: 10000,
			Capabilities: config.Capabilities{Modbus: &config.ModbusConfig{Host: "127.0.0.1", Port: 502, UnitID: 1}}}},
	}
	cfg.PinModbusProxyOwnership()
	if len(driverCapacitiesFrom(cfg.ModbusControlDrivers(), nil, evCatalog(), true)) != 0 {
		t.Fatal("external battery remains in planner pool")
	}
	if got := driverCapacitiesFrom(cfg.Drivers, nil, evCatalog(), false); got["sungrow"] != 10000 {
		t.Fatalf("telemetry capacity lost: %v", got)
	}
	if len(driverLimitsFrom(cfg.ModbusControlDrivers(), cfg.Batteries)) != 0 {
		t.Fatal("external battery remains in dispatch limits")
	}
	if !config.ObserveOnlyDriverSet(cfg)["sungrow"] {
		t.Fatal("watchdog does not see read-only owner")
	}
}
