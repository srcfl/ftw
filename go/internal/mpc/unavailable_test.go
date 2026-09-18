package mpc

import "testing"

func TestUnavailableReasonOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		enabled     bool
		provider    string
		capWh       float64
		batteryless bool
		want        string
	}{
		{"disabled wins even with price and battery", false, "nordpool", 10000, true, ReasonPlannerDisabled},
		{"no provider", true, "", 10000, true, ReasonNoPriceProvider},
		{"provider none", true, "none", 10000, true, ReasonNoPriceProvider},
		{"without storage supported by engine", true, "nordpool", 0, true, ""},
		{"without storage rejected by engine", true, "nordpool", 0, false, ReasonNoBatteryCapacity},
		{"negative capacity is empty pool", true, "nordpool", -1, true, ReasonNoBatteryCapacity},
		{"ready", true, "nordpool", 9600, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := UnavailableReason(tc.enabled, tc.provider, tc.capWh, tc.batteryless); got != tc.want {
				t.Fatalf("UnavailableReason(%v, %q, %v, %v) = %q, want %q",
					tc.enabled, tc.provider, tc.capWh, tc.batteryless, got, tc.want)
			}
		})
	}
}
