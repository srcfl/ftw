package config

import (
	"os"
	"testing"
)

func TestExampleConfigParsesOCPP(t *testing.T) {
	data, err := os.ReadFile("../../../config.example.yaml")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	c, err := Parse(data, ".")
	if err != nil {
		t.Fatalf("parse example config: %v", err)
	}
	if c.OCPP == nil {
		t.Fatal("example config has no ocpp section")
	}
	if c.OCPP.Enabled {
		t.Error("example config must ship with ocpp disabled")
	}
	if c.OCPP.Port != 8887 {
		t.Errorf("port: got %d want 8887", c.OCPP.Port)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("example config must validate: %v", err)
	}
	t.Logf("ocpp parsed: enabled=%v port=%d path=%q heartbeat=%d",
		c.OCPP.Enabled, c.OCPP.Port, c.OCPP.Path, c.OCPP.HeartbeatIntervalS)
}
