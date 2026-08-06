package config

import (
	"strings"
	"testing"
)

func TestValidateRejectsMoreThanThreePhases(t *testing.T) {
	c := &Config{
		Site: Site{SmoothingAlpha: 0.3},
		Fuse: Fuse{MaxAmps: 16, Phases: 4, Voltage: 230},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "fuse.phases") {
		t.Errorf("expected fuse.phases error for 4 phases, got %v", err)
	}
}

func TestValidateAcceptsOneToThreePhases(t *testing.T) {
	for phases := 1; phases <= 3; phases++ {
		c := &Config{
			Site: Site{SmoothingAlpha: 0.3},
			Fuse: Fuse{MaxAmps: 16, Phases: phases, Voltage: 230},
		}
		if err := c.Validate(); err != nil {
			t.Errorf("phases=%d: unexpected error: %v", phases, err)
		}
	}
}

func meterDriver(name string, siteMeter bool) Driver {
	return Driver{
		Name:        name,
		Lua:         "drivers/test.lua",
		IsSiteMeter: siteMeter,
		Capabilities: Capabilities{
			Modbus: &ModbusConfig{Host: "192.168.1.10", Port: 502},
		},
	}
}

func TestValidateRejectsDuplicateSiteMeter(t *testing.T) {
	c := &Config{
		Site:    Site{SmoothingAlpha: 0.3},
		Fuse:    Fuse{MaxAmps: 16, Phases: 3, Voltage: 230},
		Drivers: []Driver{meterDriver("a", true), meterDriver("b", true)},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "is_site_meter") {
		t.Errorf("expected duplicate is_site_meter error, got %v", err)
	}
}

func TestValidateAcceptsSingleSiteMeter(t *testing.T) {
	c := &Config{
		Site:    Site{SmoothingAlpha: 0.3},
		Fuse:    Fuse{MaxAmps: 16, Phases: 3, Voltage: 230},
		Drivers: []Driver{meterDriver("a", true), meterDriver("b", false)},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
