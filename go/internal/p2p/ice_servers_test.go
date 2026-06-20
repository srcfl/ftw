package p2p

import "testing"

func TestICEHasTURN(t *testing.T) {
	cases := []struct {
		name string
		in   []ICEServer
		want bool
	}{
		{"nil", nil, false},
		{"stun only", []ICEServer{{URLs: []string{"stun:s.example:3478"}}}, false},
		{"stun+turn", []ICEServer{{URLs: []string{"stun:s.example:3478"}}, {URLs: []string{"turn:t.example:3478?transport=udp"}}}, true},
		{"turns", []ICEServer{{URLs: []string{"turns:t.example:5349"}}}, true},
		{"uppercase scheme", []ICEServer{{URLs: []string{"TURN:t.example:3478"}}}, true},
		{"leading whitespace", []ICEServer{{URLs: []string{"  turn:t.example:3478 "}}}, true},
		{"empty urls entry", []ICEServer{{URLs: nil}}, false},
	}
	for _, c := range cases {
		if got := iceHasTURN(c.in); got != c.want {
			t.Errorf("%s: iceHasTURN = %v, want %v", c.name, got, c.want)
		}
	}
}
