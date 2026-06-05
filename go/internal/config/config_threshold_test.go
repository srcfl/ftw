package config

import "testing"

func TestPlannerValidateRejectsNegativeArbitrageSpread(t *testing.T) {
	c := &Config{
		Site:    Site{SmoothingAlpha: 0.3},
		Fuse:    Fuse{MaxAmps: 16, Phases: 3, Voltage: 230},
		Planner: &Planner{Enabled: true, MinArbitrageSpreadOreKwh: -5},
	}
	if err := c.Validate(); err == nil {
		t.Error("expected error for negative min_arbitrage_spread_ore_kwh")
	}
}

func TestPlannerValidateAcceptsNonNegativeArbitrageSpread(t *testing.T) {
	c := &Config{
		Site:    Site{SmoothingAlpha: 0.3},
		Fuse:    Fuse{MaxAmps: 16, Phases: 3, Voltage: 230},
		Planner: &Planner{Enabled: true, MinArbitrageSpreadOreKwh: 20},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected error for valid threshold: %v", err)
	}
}
