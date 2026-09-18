package config

import (
	"strings"
	"testing"
)

func TestModbusProxyBindsSingleEndpointUsesDefaultListen(t *testing.T) {
	c := &Config{
		ModbusProxy: &ModbusProxy{Enabled: true},
		Drivers: []Driver{{
			Name:         "sungrow",
			Capabilities: Capabilities{Modbus: &ModbusConfig{Host: "192.168.1.10", Port: 502, UnitID: 1}},
		}},
	}
	binds, err := c.ModbusProxyBinds()
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 1 || binds[0].Listen != DefaultModbusProxyListen || binds[0].Host != "192.168.1.10" || binds[0].Port != 502 {
		t.Fatalf("binds = %+v", binds)
	}
}

func TestModbusProxyBindsRequiresPerEndpointListenWhenMultiple(t *testing.T) {
	c := &Config{
		ModbusProxy: &ModbusProxy{Enabled: true, Listen: ":1502"},
		Drivers: []Driver{
			{Name: "a", Capabilities: Capabilities{Modbus: &ModbusConfig{Host: "10.0.0.1", Port: 502}}},
			{Name: "b", Capabilities: Capabilities{Modbus: &ModbusConfig{Host: "10.0.0.2", Port: 502}}},
		},
	}
	_, err := c.ModbusProxyBinds()
	if err == nil || !strings.Contains(err.Error(), "proxy_listen") {
		t.Fatalf("err = %v, want proxy_listen required", err)
	}
	c.Drivers[0].Capabilities.Modbus.ProxyListen = ":1502"
	c.Drivers[1].Capabilities.Modbus.ProxyListen = ":1503"
	binds, err := c.ModbusProxyBinds()
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 2 || binds[0].Listen != ":1502" || binds[1].Listen != ":1503" {
		t.Fatalf("binds = %+v", binds)
	}
}

func TestModbusProxyBindsSharesOneListenForSameHostPort(t *testing.T) {
	c := &Config{
		ModbusProxy: &ModbusProxy{Enabled: true},
		Drivers: []Driver{
			{Name: "meter", Capabilities: Capabilities{Modbus: &ModbusConfig{Host: "10.0.0.5", Port: 502, UnitID: 1}}},
			{Name: "inverter", Capabilities: Capabilities{Modbus: &ModbusConfig{Host: "10.0.0.5", Port: 502, UnitID: 2}}},
		},
	}
	binds, err := c.ModbusProxyBinds()
	if err != nil {
		t.Fatal(err)
	}
	if len(binds) != 1 {
		t.Fatalf("binds = %+v, want one shared backend", binds)
	}
}

func TestModbusProxyDisabledIsNoop(t *testing.T) {
	c := &Config{
		Drivers: []Driver{{
			Name:         "sungrow",
			Capabilities: Capabilities{Modbus: &ModbusConfig{Host: "192.168.1.10", Port: 502}},
		}},
	}
	binds, err := c.ModbusProxyBinds()
	if err != nil || len(binds) != 0 {
		t.Fatalf("disabled proxy binds = %v err = %v", binds, err)
	}
}

func TestNormalizeListenAddr(t *testing.T) {
	got, err := NormalizeListenAddr("1502")
	if err != nil || got != ":1502" {
		t.Fatalf("1502 -> %q %v", got, err)
	}
	if _, err := NormalizeListenAddr("not-a-port"); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseModbusProxyYAML(t *testing.T) {
	c, err := Parse([]byte(minimalYAML+`
modbus_proxy:
  enabled: true
  listen: ":1502"
  allow_write: false
`), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if !c.ModbusProxy.On() || c.ModbusProxy.Listen != ":1502" || c.ModbusProxy.AllowWrite {
		t.Fatalf("parsed %+v", c.ModbusProxy)
	}
}
