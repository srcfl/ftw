package state

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func openLifetimeStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHistoryAggregation(); err != nil {
		s.Close()
		t.Fatal(err)
	}
	return s
}

// lifetimeTicks records one aggregated tick every step in [from, to).
func lifetimeTicks(t *testing.T, s *Store, from, to time.Time, step time.Duration, point func(time.Time) HistoryPoint) {
	t.Helper()
	for at := from; at.Before(to); at = at.Add(step) {
		p := point(at)
		p.TsMs = at.UnixMilli()
		dashboardTick(t, s, fmt.Sprint(p.TsMs), &p)
	}
}

func lifetimeFlows(l LifetimeEnergy) map[string]float64 {
	return map[string]float64{
		"import": l.ImportWh, "export": l.ExportWh, "pv": l.PVWh,
		"charge": l.BatChargedWh, "discharge": l.BatDischargedWh, "load": l.LoadWh,
	}
}

func dayFlows(d DayEnergy) map[string]float64 {
	return map[string]float64{
		"import": d.ImportWh, "export": d.ExportWh, "pv": d.PVWh,
		"charge": d.BatChargedWh, "discharge": d.BatDischargedWh, "load": d.LoadWh,
	}
}

func sameFlows(t *testing.T, what string, got, want map[string]float64) {
	t.Helper()
	for flow, w := range want {
		if math.Abs(got[flow]-w) > 1e-6*math.Max(1, math.Abs(w)) {
			t.Errorf("%s: %s = %v, want %v", what, flow, got[flow], w)
		}
	}
}

func readLifetime(t *testing.T, s *Store, now time.Time) LifetimeEnergy {
	t.Helper()
	l, ok, err := s.LifetimeEnergy(now)
	if err != nil || !ok {
		t.Fatalf("LifetimeEnergy(%s) = %v, %v, %v", now.Format(time.TimeOnly), l, ok, err)
	}
	return l
}

func notLower(t *testing.T, what string, got, prev LifetimeEnergy) {
	t.Helper()
	p := lifetimeFlows(prev)
	for flow, v := range lifetimeFlows(got) {
		if v < p[flow] {
			t.Errorf("%s: %s fell from %v to %v", what, flow, p[flow], v)
		}
	}
}

// A sunny afternoon, then a night when the inverter draws standby power, so
// the stored PV total falls. The battery and the grid change direction each
// tick, so every flow moves.
func afternoonThenNight(start time.Time) func(time.Time) HistoryPoint {
	return func(at time.Time) HistoryPoint {
		i := at.Sub(start) / (10 * time.Second)
		p := HistoryPoint{GridW: 3000, PVW: -2000, BatW: 800, LoadW: 1500}
		if i%2 == 1 {
			p.GridW, p.BatW = -1200, -600
		}
		if at.Sub(start) >= 90*time.Minute {
			p.PVW = 20
		}
		return p
	}
}

func TestLifetimeEnergyCountsStoredHistoryOnceAndNeverFalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openLifetimeStore(t, path)
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	point := afternoonThenNight(start)

	// A new box starts at zero.
	prev := readLifetime(t, s, start)
	sameFlows(t, "empty history", lifetimeFlows(prev), dayFlows(DayEnergy{}))

	end := start.Add(200 * time.Minute)
	for at := start; at.Before(end); at = at.Add(5 * time.Minute) {
		lifetimeTicks(t, s, at, at.Add(5*time.Minute), 10*time.Second, point)
		got := readLifetime(t, s, at.Add(5*time.Minute))
		notLower(t, at.Format(time.TimeOnly), got, prev)
		prev = got
	}
	stored, err := s.DailyEnergy(start.UnixMilli(), end.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if stored.PVWh >= prev.PVWh {
		t.Fatalf("the test needs a night that lowers stored PV: stored %v, shown %v", stored.PVWh, prev.PVWh)
	}
	// Every flow but PV is the stored total. PV stays at its evening high.
	want := dayFlows(stored)
	want["pv"] = prev.PVWh
	sameFlows(t, "before restart", lifetimeFlows(prev), want)
	var through int64
	if err := s.history.QueryRow(`SELECT through_ms FROM energy_lifetime WHERE key='base'`).Scan(&through); err != nil {
		t.Fatal(err)
	}
	if want := end.Add(-time.Hour).Truncate(15 * time.Minute).UnixMilli(); through != want {
		t.Fatalf("base through %d, want %d: whole quarter hours an hour old", through, want)
	}

	// A restart at night shows the same totals: no history counted twice and
	// the PV floor kept.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openLifetimeStore(t, path)
	defer s.Close()
	again := readLifetime(t, s, end)
	sameFlows(t, "after restart", lifetimeFlows(again), lifetimeFlows(prev))

	// The next morning's sun counts again once it has made up the night.
	morning := end.Add(30 * time.Minute)
	lifetimeTicks(t, s, end, morning, 10*time.Second, func(at time.Time) HistoryPoint {
		p := point(at)
		p.PVW = -3000
		return p
	})
	later := readLifetime(t, s, morning)
	notLower(t, "morning", later, again)
	stored, err = s.DailyEnergy(start.UnixMilli(), morning.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	sameFlows(t, "morning", lifetimeFlows(later), dayFlows(stored))
}

func TestLifetimeEnergyAddsUpOlderHistoryBeforeShowingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openLifetimeStore(t, path)
	defer s.Close()
	start := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	now := start.Add(9 * 24 * time.Hour)
	lifetimeTicks(t, s, start, now, 10*time.Minute, func(at time.Time) HistoryPoint {
		return HistoryPoint{GridW: 400 + float64(at.Hour()*10), PVW: -float64(at.Hour() * 50), BatW: 100, LoadW: 700}
	})
	stored, err := s.DailyEnergy(start.UnixMilli(), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}

	// Nine days take two reads, a week each; the first shows nothing, so
	// Home Assistant never counts the history as new energy.
	if l, ok, err := s.LifetimeEnergy(now); err != nil || ok {
		t.Fatalf("first read = %v, %v, %v; want not ready", l, ok, err)
	}
	got := readLifetime(t, s, now)
	sameFlows(t, "nine days", lifetimeFlows(got), dayFlows(stored))

	// A history file that lost the row rebuilds it from the stored history.
	if _, err := s.history.Exec(`DELETE FROM energy_lifetime WHERE key='base'`); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LifetimeEnergy(now); err != nil || ok {
		t.Fatalf("rebuild first read = %v, %v; want not ready", ok, err)
	}
	rebuilt := readLifetime(t, s, now)
	sameFlows(t, "rebuilt", lifetimeFlows(rebuilt), dayFlows(stored))
}
