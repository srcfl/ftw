package api

import (
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/drivers"
	"github.com/frahlg/forty-two-watts/go/internal/scanner"
)

func TestProtocolMatches(t *testing.T) {
	cases := []struct {
		protos []string
		proto  string
		want   bool
	}{
		{[]string{"modbus"}, "modbus", true},
		{[]string{"modbus", "mqtt"}, "MQTT", true}, // case-insensitive
		{[]string{"http"}, "modbus", false},
		{nil, "modbus", false}, // no declared protocols matches nothing
	}
	for _, c := range cases {
		if got := protocolMatches(c.protos, c.proto); got != c.want {
			t.Errorf("protocolMatches(%v, %q) = %v, want %v", c.protos, c.proto, got, c.want)
		}
	}
}

func TestConnDefaultInt(t *testing.T) {
	m := map[string]any{"unit_id": int64(3), "f": float64(5), "i": 7, "s": "x"}
	for _, c := range []struct {
		key  string
		want int
		ok   bool
	}{
		{"unit_id", 3, true},
		{"f", 5, true},
		{"i", 7, true},
		{"s", 0, false}, // strings aren't ints
		{"missing", 0, false},
	} {
		got, ok := connDefaultInt(m, c.key)
		if got != c.want || ok != c.ok {
			t.Errorf("connDefaultInt(%q) = (%d,%v), want (%d,%v)", c.key, got, ok, c.want, c.ok)
		}
	}
}

func TestProbeConfigFor(t *testing.T) {
	dev := scanner.FoundDevice{IP: "192.168.1.15", Port: 503, Protocol: "modbus"}
	entry := drivers.CatalogEntry{
		Path:               "drivers/pixii.lua",
		Filename:           "pixii.lua",
		Protocols:          []string{"modbus"},
		ConnectionDefaults: map[string]any{"unit_id": int64(2)},
	}
	cfg, ok := probeConfigFor(entry, dev, ".")
	if !ok {
		t.Fatal("probeConfigFor returned ok=false for a modbus device")
	}
	mb := cfg.EffectiveModbus()
	if mb == nil {
		t.Fatal("expected a Modbus capability")
	}
	if mb.Host != "192.168.1.15" || mb.Port != 503 || mb.UnitID != 2 {
		t.Errorf("modbus cfg = %+v, want host .15 port 503 unit 2", mb)
	}

	// MQTT device gets an MQTT capability with the default port filled in.
	mq := scanner.FoundDevice{IP: "10.0.0.5", Port: 0, Protocol: "mqtt"}
	cfg, ok = probeConfigFor(drivers.CatalogEntry{Path: "drivers/ferroamp.lua", Protocols: []string{"mqtt"}}, mq, ".")
	if !ok || cfg.EffectiveMQTT() == nil || cfg.EffectiveMQTT().Port != 1883 {
		t.Errorf("mqtt probe cfg = %+v ok=%v, want mqtt port 1883", cfg.EffectiveMQTT(), ok)
	}

	// http/other protocols have no wired probe transport yet.
	if _, ok := probeConfigFor(drivers.CatalogEntry{Path: "drivers/x.lua", Protocols: []string{"http"}},
		scanner.FoundDevice{IP: "10.0.0.6", Port: 80, Protocol: "http"}, "."); ok {
		t.Error("probeConfigFor should return ok=false for http (no fingerprint transport)")
	}
}
