package config

import (
	"reflect"
	"testing"
)

func proxyOwnershipConfig() *Config {
	return &Config{
		ModbusProxy: &ModbusProxy{Enabled: true, AllowWrite: true},
		Drivers: []Driver{
			{Name: "battery", BatteryCapacityWh: 10000, Capabilities: Capabilities{Modbus: &ModbusConfig{Host: "10.0.0.1", UnitID: 1}}},
			{Name: "meter", Capabilities: Capabilities{Modbus: &ModbusConfig{Host: "10.0.0.1", Port: 502, UnitID: 2}}},
			{Name: "mqtt", Capabilities: Capabilities{MQTT: &MQTTConfig{Host: "10.0.0.2"}}},
		},
	}
}

func TestModbusProxyOwnershipSurvivesSavedModeChanges(t *testing.T) {
	cfg := proxyOwnershipConfig()
	cfg.PinModbusProxyOwnership()
	cfg.ModbusProxy.AllowWrite = false
	if got := ObserveOnlyDriverSet(cfg); !got["battery"] || !got["meter"] || got["mqtt"] {
		t.Fatalf("active ownership changed before restart: %v", got)
	}
	runtime := cfg.ModbusControlDrivers()
	if !runtime[0].ObserveOnly || !runtime[1].ObserveOnly || runtime[2].ObserveOnly {
		t.Fatalf("runtime drivers: %+v", runtime)
	}
	if cfg.Drivers[0].ObserveOnly || cfg.Drivers[1].ObserveOnly {
		t.Fatal("derived flags changed saved settings")
	}
	incoming := proxyOwnershipConfig()
	incoming.ModbusProxy.Enabled = false
	incoming.PreserveMaskedSecrets(cfg)
	if !incoming.ModbusProxyOwnsWrites(incoming.Drivers[0]) {
		t.Fatal("settings save lost active ownership")
	}
	incoming.PinModbusProxyOwnership()
	if len(ObserveOnlyDriverSet(incoming)) != 0 {
		t.Fatal("restart did not return ownership to FTW")
	}
}

func TestModbusProxyReadOnlyDoesNotTakeControlOnSave(t *testing.T) {
	cfg := proxyOwnershipConfig()
	cfg.ModbusProxy.AllowWrite = false
	cfg.PinModbusProxyOwnership()
	cfg.ModbusProxy.AllowWrite = true
	if len(ObserveOnlyDriverSet(cfg)) != 0 {
		t.Fatal("write ownership changed before restart")
	}
	cfg.PinModbusProxyOwnership()
	if !cfg.ModbusProxyOwnsWrites(cfg.Drivers[0]) {
		t.Fatal("restart did not apply external ownership")
	}
}

func TestModbusProxyAllowlistAndRestartFollowEffectiveEndpoints(t *testing.T) {
	cfg := proxyOwnershipConfig()
	binds, err := cfg.ModbusProxyBinds()
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 1 || !reflect.DeepEqual(binds[0].UnitIDs, []uint8{1, 2}) {
		t.Fatalf("binds: %+v", binds)
	}
	other := proxyOwnershipConfig()
	other.Drivers[0], other.Drivers[1] = other.Drivers[1], other.Drivers[0]
	if got := RestartRequiredFor(cfg, other); len(got) != 0 {
		t.Fatalf("driver order changed effective listener: %v", got)
	}
	other.Drivers[0].Capabilities.Modbus.UnitID = 3
	if got := RestartRequiredFor(cfg, other); len(got) == 0 {
		t.Fatal("unit allowlist change needs restart")
	}
}

func TestUnusedModbusProxySettingsDoNotRequireRestart(t *testing.T) {
	old := &Config{}
	newCfg := &Config{ModbusProxy: &ModbusProxy{Listen: ":1502"}}
	if got := RestartRequiredFor(old, newCfg); len(got) != 0 {
		t.Fatalf("disabled defaults require restart: %v", got)
	}
}
