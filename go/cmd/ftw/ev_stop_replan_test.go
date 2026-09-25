package main

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func TestEVStopReplanOnlyWhenTheCarStoppedItself(t *testing.T) {
	charging := loadpoint.State{PluggedIn: true, CurrentPowerW: 0, CommandedKnown: true, CommandedW: 6900}
	cases := []struct {
		name  string
		state loadpoint.State
		prevW float64
		want  bool
	}{
		{"car stopped while Core offered power", charging, 6900, true},
		{"Core ordered the stop", loadpoint.State{PluggedIn: true, CommandedKnown: true, CommandedW: 0}, 6900, false},
		{"no command known yet", loadpoint.State{PluggedIn: true}, 6900, true},
		{"was not charging", charging, 20, false},
		{"still charging", loadpoint.State{PluggedIn: true, CurrentPowerW: 4140, CommandedKnown: true, CommandedW: 4140}, 6900, false},
		{"unplugged", loadpoint.State{CommandedKnown: true, CommandedW: 6900}, 6900, false},
	}
	for _, tc := range cases {
		if got := evStopNeedsReplan(tc.state, tc.prevW); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
