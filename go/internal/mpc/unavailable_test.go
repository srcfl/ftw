package mpc

import "testing"

func TestUnavailableReasonOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		enabled  bool
		provider string
		capWh    float64
		want     string
	}{
		{"disabled wins even with price and battery", false, "nordpool", 10000, ReasonPlannerDisabled},
		{"no provider", true, "", 10000, ReasonNoPriceProvider},
		{"provider none", true, "none", 10000, ReasonNoPriceProvider},
		{"without storage is supported", true, "nordpool", 0, ""},
		{"negative capacity is empty pool", true, "nordpool", -1, ReasonNoBatteryCapacity},
		{"ready", true, "nordpool", 9600, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := UnavailableReason(tc.enabled, tc.provider, tc.capWh); got != tc.want {
				t.Fatalf("UnavailableReason(%v, %q, %v) = %q, want %q",
					tc.enabled, tc.provider, tc.capWh, got, tc.want)
			}
		})
	}
}
