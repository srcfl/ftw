package scanner

import (
	"net"
	"testing"
)

func TestIsPrivateV4(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"192.168.50.0", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"172.31.255.0", true},
		{"172.15.0.1", false}, // just below the 172.16/12 block
		{"172.32.0.1", false}, // just above
		{"8.8.8.8", false},
		{"100.64.0.1", false}, // CGNAT, not RFC1918
	}
	for _, c := range cases {
		if got := isPrivateV4(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("isPrivateV4(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}
