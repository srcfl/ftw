package main

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func TestActiveBatteryBoostTotalsWithoutPlannerController(t *testing.T) {
	powerW, reserveSoC := activeBatteryBoostTotals(nil, []loadpoint.State{{
		ID: "garage", PluggedIn: true, CurrentPowerW: 7400,
	}}, time.Now())
	if powerW != 0 || reserveSoC != 0 {
		t.Fatalf("no-controller totals = %.0f W, %.2f; want 0, 0", powerW, reserveSoC)
	}
}
