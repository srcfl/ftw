package scanner

import (
	"net"
	"testing"
)

func TestPrivateAndRoutedSubnetBounds(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"192.168.50.0", true}, {"10.1.2.3", true}, {"172.16.0.1", true},
		{"172.31.255.0", true}, {"172.15.0.1", false}, {"172.32.0.1", false},
		{"8.8.8.8", false}, {"100.64.0.1", false},
	} {
		if got := isPrivateV4(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("isPrivateV4(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}

	_, subnet, _ := net.ParseCIDR("192.168.50.16/28")
	hosts := subnetHosts(*subnet)
	if len(hosts) != 14 || hosts[0] != "192.168.50.17" || hosts[len(hosts)-1] != "192.168.50.30" {
		t.Fatalf("/28 hosts = %v, want .17 through .30", hosts)
	}
}
