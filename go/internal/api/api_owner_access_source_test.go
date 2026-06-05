package api

import (
	"net/http"
	"testing"
)

// isLANClientSource / isLoopbackSource are the fail-closed second line of
// defence behind the X-FTW-Tunnel marker (see authorizeOwner). They classify
// the request's SOURCE address (RemoteAddr), so this table pins the exact
// trust boundary: private-range = LAN client, everything else (public, CGNAT,
// loopback) = not a LAN client.
func TestIsLANClientSource(t *testing.T) {
	cases := []struct {
		name       string
		remoteAddr string
		wantLAN    bool
		wantLoop   bool
	}{
		{"private 192.168/16", "192.168.1.50:1234", true, false},
		{"private 10/8", "10.0.0.5:9", true, false},
		{"private 172.16/12", "172.16.5.5:9", true, false},
		{"ipv4 link-local 169.254/16", "169.254.10.10:9", true, false},
		{"ula fc00::/7", "[fc00::1]:9", true, false},
		{"ipv6 link-local fe80::/10", "[fe80::1]:9", true, false},
		{"ipv4-mapped private", "[::ffff:192.168.1.1]:9", true, false},
		{"no port falls back to whole RemoteAddr", "192.168.1.50", true, false},
		{"loopback v4 is NOT a LAN client", "127.0.0.1:9", false, true},
		{"loopback v6 is NOT a LAN client", "[::1]:9", false, true},
		{"public ipv4", "8.8.8.8:9", false, false},
		{"public ipv6", "[2606:4700:4700::1111]:9", false, false},
		{"CGNAT 100.64/10 is NOT private", "100.64.0.1:9", false, false},
		{"garbage", "not-an-ip", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tc.remoteAddr}
			if got := isLANClientSource(r); got != tc.wantLAN {
				t.Errorf("isLANClientSource(%q) = %v, want %v", tc.remoteAddr, got, tc.wantLAN)
			}
			if got := isLoopbackSource(r); got != tc.wantLoop {
				t.Errorf("isLoopbackSource(%q) = %v, want %v", tc.remoteAddr, got, tc.wantLoop)
			}
		})
	}
}
