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

// The plan follows the refusal latch at once: it leaves a car out as soon as
// the car has declined, and takes it in as soon as the latch ends while the
// car is still plugged in (srcfl/ftw#2080). An unplug replans on its own.
func TestDeclineEdgesReplan(t *testing.T) {
	for _, tc := range []struct {
		name               string
		prev, now, plugged bool
		want               string
	}{
		{"car declined", false, true, true, "loadpoint_declined"},
		{"refusal ended", true, false, true, "loadpoint_decline_ended"},
		{"unplugged", true, false, false, ""},
		{"still declined", true, true, true, ""},
		{"never declined", false, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lp := loadpoint.State{PluggedIn: tc.plugged, ChargingDeclined: tc.now}
			if got := declineReplanReason(lp, tc.prev); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}
