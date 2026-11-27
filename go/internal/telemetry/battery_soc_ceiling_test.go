package telemetry

import (
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"
)

func TestParseSoCCeiling(t *testing.T) {
	for _, tc := range []struct {
		data string
		want float64
		ok   bool
	}{
		{`{"max_soc_fract":0.9}`, 0.9, true},
		{`{"max_soc_fract":1}`, 1, true},
		{`{"max_soc_fract":0}`, 0, false},
		{`{"max_soc_fract":-0.1}`, 0, false},
		{`{"max_soc_fract":90}`, 0, false},
		{`{"max_soc_fract":null}`, 0, false},
		{`{"soc":0.9}`, 0, false},
		{`not json`, 0, false},
		{``, 0, false},
	} {
		got, ok := parseSoCCeiling(json.RawMessage(tc.data))
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseSoCCeiling(%s) = %v, %v; want %v, %v", tc.data, got, ok, tc.want, tc.ok)
		}
	}
}

func batteryCeiling(s *Store) (float64, bool) {
	return s.Get("pixii", DerBattery).SoCCeilingNow(time.Now())
}

func updateBattery(s *Store, data string) {
	soc := .9
	s.Update("pixii", DerBattery, 0, &soc, json.RawMessage(data))
}

// One reading without the ceiling, from a limit register that missed a read,
// must not release a full battery. The ceiling holds for a bounded time.
func TestSoCCeilingHoldsAcrossReadingsWithoutIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewStore()
		updateBattery(s, `{"max_soc_fract":0.9}`)
		if c, ok := batteryCeiling(s); !ok || c != .9 {
			t.Fatalf("ceiling = %v, %v; want 0.9", c, ok)
		}
		time.Sleep(BatterySoCCeilingHold - time.Second)
		updateBattery(s, `{}`)
		if c, ok := batteryCeiling(s); !ok || c != .9 {
			t.Fatalf("ceiling after a reading without it = %v, %v; want the held 0.9", c, ok)
		}
		time.Sleep(2 * time.Second)
		updateBattery(s, `{}`)
		if c, ok := batteryCeiling(s); ok {
			t.Fatalf("ceiling %v outlived its hold", c)
		}
		updateBattery(s, `{"max_soc_fract":0.95}`)
		if c, ok := batteryCeiling(s); !ok || c != .95 {
			t.Fatalf("a new ceiling = %v, %v; want 0.95", c, ok)
		}
	})
}

func TestSoCCeilingDropsWithAStaleReading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewStore()
		updateBattery(s, `{"max_soc_fract":0.9}`)
		time.Sleep(BatterySoCMaxAge + time.Second)
		if c, ok := batteryCeiling(s); ok {
			t.Fatalf("a stale reading kept its ceiling %v", c)
		}
	})
}

func TestSoCCeilingBelowTrustedMinimumIsIgnored(t *testing.T) {
	s := NewStore()
	updateBattery(s, `{"max_soc_fract":0.05}`)
	if c, ok := batteryCeiling(s); ok {
		t.Fatalf("a ceiling of 5%% was trusted: %v", c)
	}
	updateBattery(s, `{"max_soc_fract":0.9}`)
	updateBattery(s, `{"max_soc_fract":0.3}`)
	if c, ok := batteryCeiling(s); !ok || c != .9 {
		t.Fatalf("an implausible read replaced the held ceiling: %v, %v", c, ok)
	}
	if got := s.lowSoCCeilings["pixii"]; got != .3 {
		t.Fatalf("logged low ceiling = %v, want 0.3", got)
	}
	updateBattery(s, `{"max_soc_fract":0.9}`)
	if _, logged := s.lowSoCCeilings["pixii"]; logged {
		t.Fatal("a trusted ceiling did not clear the low-ceiling log state")
	}
}

func TestSoCCeilingDoesNotSurviveRemove(t *testing.T) {
	s := NewStore()
	updateBattery(s, `{"max_soc_fract":0.3}`)
	updateBattery(s, `{"max_soc_fract":0.9}`)
	updateBattery(s, `{"max_soc_fract":0.3}`)
	s.Remove("pixii")
	if _, logged := s.lowSoCCeilings["pixii"]; logged {
		t.Fatal("Remove kept the driver's low-ceiling log state")
	}
	updateBattery(s, `{}`)
	if c, ok := batteryCeiling(s); ok {
		t.Fatalf("a replacement driver inherited ceiling %v", c)
	}
}

func TestAtFullUsesTheBatterysOwnCeiling(t *testing.T) {
	now := time.Now()
	soc := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name    string
		r       *DerReading
		want    bool
	}{
		{"no reading", nil, false},
		{"99 % with no ceiling", &DerReading{SoC: soc(0.99), SoCUpdatedAt: now, UpdatedAt: now}, true},
		{"97 % with no ceiling", &DerReading{SoC: soc(0.97), SoCUpdatedAt: now, UpdatedAt: now}, false},
		{"89 % under a 90 % ceiling", &DerReading{SoC: soc(0.89), SoCUpdatedAt: now, UpdatedAt: now, SoCCeiling: soc(0.9), SoCCeilingAt: now}, true},
		{"stale 100 %", &DerReading{SoC: soc(1), SoCUpdatedAt: now.Add(-BatterySoCMaxAge - time.Second), UpdatedAt: now}, false},
	} {
		if got := tc.r.AtFull(now); got != tc.want {
			t.Errorf("%s: AtFull = %v, want %v", tc.name, got, tc.want)
		}
	}
}
