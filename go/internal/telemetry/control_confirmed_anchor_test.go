package telemetry

import (
	"fmt"
	"testing"
	"time"
)

func confirmedAnchorData(at time.Time) []byte {
	return []byte(fmt.Sprintf(`{"control_power_observed_at":%q,"control_power_confirmed":true}`, at.Format(time.RFC3339Nano)))
}

func TestConfirmedControlAnchorRetainsSourceTimeAndOneSample(t *testing.T) {
	now := time.Now()
	source := now.Add(-time.Minute)
	s := NewStore()
	for i := 10; i >= 1; i-- {
		s.recordControlObservation("car", DerEV, 6183, confirmedAnchorData(source), now.Add(-time.Duration(i)*time.Second))
	}
	points := copyConfirmedControlPoints(s.controlObservations["car:ev"], now.Add(-20*time.Second), now)
	if len(points) != 1 || !points[0].At.Equal(source) || !points[0].Seen().Equal(now.Add(-time.Second)) {
		t.Fatalf("confirmed anchor lost or duplicated: %+v", points)
	}
	if physical := copyControlPoints(points, now.Add(-20*time.Second), now); len(physical) != 0 {
		t.Fatalf("response verifier received an old confirmed anchor: %+v", physical)
	}
	if w := observationWindow(points, now.Add(-20*time.Second), now); w.Count != 0 || w.Usable(now) {
		t.Fatalf("confirmation manufactured source samples: %+v", w)
	}
	if expired := copyConfirmedControlPoints(points, now, now.Add(time.Second)); len(expired) != 0 {
		t.Fatal("expired confirmation remained usable")
	}
}

func TestConfirmedControlAnchorMissingOrConflictingDataBreaksProof(t *testing.T) {
	now := time.Now()
	source := now.Add(-time.Minute)
	for _, tc := range []struct {
		name  string
		data  []byte
		power float64
	}{
		{"missing", []byte(`{"control_power_available":false,"control_power_confirmed":true}`), 6183},
		{"unconfirmed old source", []byte(fmt.Sprintf(`{"control_power_observed_at":%q}`, source.Format(time.RFC3339Nano))), 6183},
		{"different power at same source time", confirmedAnchorData(source), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore()
			s.recordControlObservation("car", DerEV, 6183, confirmedAnchorData(source), now.Add(-time.Second))
			s.recordControlObservation("car", DerEV, tc.power, tc.data, now)
			if ps := s.controlObservations["car:ev"]; len(ps) != 0 {
				t.Fatalf("invalid proof retained: %+v", ps)
			}
		})
	}
}

func TestConfirmedControlAnchorRetainsStopAndBoundsFrozenConfirmation(t *testing.T) {
	now := time.Now()
	s := NewStore()
	source := now.Add(-time.Minute)
	s.recordControlObservation("car", DerEV, 6183, confirmedAnchorData(source), now.Add(-time.Second))
	s.recordControlObservation("car", DerEV, 0, confirmedAnchorData(now), now)
	s.recordControlObservation("car", DerEV, 4757, confirmedAnchorData(now.Add(time.Second)), now.Add(time.Second))
	ps := copyConfirmedControlPoints(s.controlObservations["car:ev"], now.Add(-20*time.Second), now.Add(time.Second))
	if len(ps) != 3 || ps[1].PowerW != 0 {
		t.Fatalf("stop was lost: %+v", ps)
	}
	s.recordControlObservation("car", DerEV, 4757, confirmedAnchorData(now.Add(time.Second)), now.Add(10*time.Second))
	frozen := copyConfirmedControlPoints(s.controlObservations["car:ev"], now, now.Add(5*time.Second))
	for _, p := range frozen {
		if p.Seen().After(now.Add(5 * time.Second)) {
			t.Fatalf("confirmation escaped frozen interval: %+v", p)
		}
	}
}

func TestConfirmedControlAnchorDoesNotManufactureResponseWindow(t *testing.T) {
	now := time.Now()
	source := now.Add(-15 * time.Second)
	s := NewStore()
	for i := 10; i >= 1; i-- {
		s.recordControlObservation("car", DerEV, 6183, confirmedAnchorData(source), now.Add(-time.Duration(i)*time.Second))
	}
	ps := copyConfirmedControlPoints(s.controlObservations["car:ev"], now.Add(-20*time.Second), now)
	w := observationWindow(ps, now.Add(-20*time.Second), now)
	if len(ps) != 1 || w.Count != 1 || w.Usable(now) {
		t.Fatalf("ten confirmations must remain one physical sample: points=%+v window=%+v", ps, w)
	}
}

func TestConfirmedControlWindowsSeparateBoundsFromResponseSamples(t *testing.T) {
	s := NewStore()
	now := time.Now()
	s.Update("car", DerEV, 6183, nil, confirmedAnchorData(now.Add(-time.Minute)))
	physical := s.ControlWindows(now.Add(-20*time.Second), time.Now())["car:ev"]
	bounds := s.ConfirmedControlWindows(now.Add(-20*time.Second), time.Now())["car:ev"]
	if len(physical.Points) != 0 || len(bounds.Points) != 1 || physical.Window.Count != 0 || bounds.Window.Count != 0 {
		t.Fatalf("confirmed interval became physical response evidence: physical=%+v bounds=%+v", physical, bounds)
	}
}
