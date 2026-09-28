package main

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
)

// A capacity is not a power rating. A 20 kWh battery with no power limits
// used to get a 10 kW plan but only a 5 kW command, even with slew disabled.
func TestBatteryPowerLimitsAgreeWithDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, driver, battery string
		charge, discharge     float64
	}{
		{"unset", "", "    weight: 1\n", 5000, 5000},
		{"driver charge only", "    max_charge_w: 8000\n", "    weight: 1\n", 8000, 5000},
		{"driver discharge only", "    max_discharge_w: 9000\n", "    weight: 1\n", 5000, 9000},
		{"battery override", "    max_charge_w: 8000\n    max_discharge_w: 9000\n", "    max_charge_w: 6000\n    max_discharge_w: 7000\n", 6000, 7000},
		{"charge forbidden", "", "    max_charge_w: 0\n", 0, 5000},
		{"discharge forbidden", "", "    max_discharge_w: 0\n", 5000, 0},
		{"both zero ignored", "", "    max_charge_w: 0\n    max_discharge_w: 0\n", 5000, 5000},
		{"both zero keeps known cap", "    max_charge_w: 2000\n", "    max_charge_w: 0\n    max_discharge_w: 0\n", 2000, 5000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseBatteryLimitConfig(t, tc.driver, tc.battery)
			cfg.Drivers[0].BatteryCapacityWh = 20000
			capacities := map[string]float64{"battery": 20000}
			fleet := mpcBatteryFleetFromConfig(cfg, capacities)
			if len(fleet) != 1 || fleet[0].MaxChargeW != tc.charge || fleet[0].MaxDischargeW != tc.discharge {
				t.Fatalf("planner limits = %+v, want charge %.0f W, discharge %.0f W", fleet, tc.charge, tc.discharge)
			}
			for _, charge := range []bool{true, false} {
				ctrl := newControlStateFromConfig(cfg)
				ctrl.SlewEnabled = false
				ctrl.Mode = control.ModeCharge
				want := tc.charge
				if !charge {
					ctrl.Mode = control.ModeSelfConsumption
					ctrl.SetBatteryManualHold(control.BatteryManualHold{PowerW: -20000, ExpiresAt: time.Now().Add(time.Minute)})
					want = -tc.discharge
				}
				targets := control.ComputeDispatch(batteryLimitStore(0), ctrl, capacities, 40000)
				if len(targets) != 1 || targets[0].TargetW != want {
					t.Fatalf("dispatch = %+v, want %.0f W (charge=%v)", targets, want, charge)
				}
			}
		})
	}
}
