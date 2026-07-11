//go:build linux

package scanner

import "testing"

func TestParseProcNetRoute(t *testing.T) {
	const table = `Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT
eth0 00000000 0101A8C0 0003 0 0 0 00000000 0 0 0
eth0 0001A8C0 00000000 0001 0 0 0 00FFFFFF 0 0 0
eth0 0032A8C0 0101A8C0 0003 0 0 0 00FFFFFF 0 0 0
eth0 0042A8C0 00000000 0000 0 0 0 00FFFFFF 0 0 0
`
	routes := parseProcNetRoute(table)
	if len(routes) != 3 {
		t.Fatalf("routes = %v, want three up routes", routes)
	}
	if got := routes[2].String(); got != "192.168.50.0/24" {
		t.Fatalf("static route = %s, want 192.168.50.0/24", got)
	}
}
