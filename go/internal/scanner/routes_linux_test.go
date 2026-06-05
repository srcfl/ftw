//go:build linux

package scanner

import "testing"

func TestParseProcNetRoute(t *testing.T) {
	// Real /proc/net/route shape: header, a default route, a connected /24,
	// and a statically-routed 192.168.50.0/24 (the case we want to pick up).
	const content = `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
eth0	00000000	0101A8C0	0003	0	0	0	00000000	0	0	0
eth0	0001A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
eth0	0032A8C0	0101A8C0	0003	0	0	0	00FFFFFF	0	0	0
`
	got := parseProcNetRoute(content)

	want := map[string]int{ // cidr -> ones
		"0.0.0.0":      0,
		"192.168.1.0":  24,
		"192.168.50.0": 24,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d routes, want %d: %+v", len(got), len(want), got)
	}
	for _, n := range got {
		ones, _ := n.Mask.Size()
		w, ok := want[n.IP.String()]
		if !ok || w != ones {
			t.Errorf("unexpected route %s/%d", n.IP, ones)
		}
	}
}
