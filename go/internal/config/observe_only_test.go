package config

import "testing"

func TestObserveOnlyRequiresCapacity(t *testing.T) {
	cfg := &Config{
		Site: Site{ControlIntervalS: 5, GridToleranceW: 42, WatchdogTimeoutS: 60, SmoothingAlpha: 0.3},
		Fuse: Fuse{MaxAmps: 16, Phases: 3, Voltage: 230},
		Drivers: []Driver{{
			Name: "retailer-batt", Lua: "drivers/pixii.lua", IsSiteMeter: true,
			ObserveOnly: true,
			Capabilities: Capabilities{Modbus: &ModbusConfig{Host: "1.2.3.4", Port: 502, UnitID: 1}},
		}},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validate error for observe_only without capacity")
	}
}

func TestObserveOnlyDriverSet(t *testing.T) {
	cfg := &Config{
		Drivers: []Driver{
			{Name: "a", ObserveOnly: true},
			{Name: "b"},
		},
	}
	got := ObserveOnlyDriverSet(cfg)
	if !got["a"] || got["b"] {
		t.Fatalf("unexpected set: %v", got)
	}
}
