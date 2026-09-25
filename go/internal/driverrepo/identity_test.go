package driverrepo

import "testing"

// The cases are the publisher's (device-drivers tools/ftw_repository.py).
func TestIdentifiesSameDriverFollowsThePublishersRule(t *testing.T) {
	for _, tc := range []struct {
		declared, catalog string
		want              bool
	}{
		{"easee-cloud", "easee_cloud", true},
		{"esphome-dsmr", "esphome-dsmr", true},
		{"ctek-chargestorm", "ctek", true},
		{"ctek-chargestorm-hybrid", "ctek_hybrid", true},
		{"sungrow-shx", "sungrow", true},
		{"huawei-sun2000", "huawei", true},
		{"sourceful-zap", "zap", true},
		{"growatt", "deye", false},
		{"different-driver", "esphome-dsmr", false},
		{"cloud-easee", "easee_cloud", false},
		{"", "easee_cloud", false},
		{"easee-cloud", "", false},
	} {
		if got := IdentifiesSameDriver(tc.declared, tc.catalog); got != tc.want {
			t.Errorf("IdentifiesSameDriver(%q, %q) = %v, want %v", tc.declared, tc.catalog, got, tc.want)
		}
	}
}
