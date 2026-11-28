package state

import (
	"context"
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
	// The newest stored row starts a minute before end.
	if want := end.Add(-time.Minute - time.Hour).Truncate(15 * time.Minute).UnixMilli(); through != want {
		t.Fatalf("base through %d, want %d: whole quarter hours an hour behind the stored history", through, want)
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

	// Losing both rows while running rebuilds the base, but never below
	// what this process already returned.
	if _, err := s.history.Exec(`DELETE FROM energy_lifetime`); err != nil {
		t.Fatal(err)
	}
	sameFlows(t, "rebuilt while running", lifetimeFlows(readLifetime(t, s, end)), lifetimeFlows(prev))

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

func storedFlows(t *testing.T, s *Store, from, to time.Time) map[string]float64 {
	t.Helper()
	d, err := s.DailyEnergy(from.UnixMilli(), to.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	return dayFlows(d)
}

func lifetimeRowFlows(t *testing.T, s *Store, key string) (LifetimeEnergy, int64) {
	t.Helper()
	l, through, ok, err := s.lifetimeRow(context.Background(), key)
	if err != nil || !ok {
		t.Fatalf("energy_lifetime %s: %v, %v", key, ok, err)
	}
	return l, through
}

func steadyHome(at time.Time) HistoryPoint {
	return HistoryPoint{GridW: 1200, PVW: -800, BatW: 300, LoadW: 1700}
}

// A clock that runs ahead must not move the base past history the writer
// has yet to store, and one that runs behind must not hide stored history.
func TestLifetimeEnergyFollowsStoredHistoryNotTheClock(t *testing.T) {
	s := openLifetimeStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer s.Close()
	start := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	mid := start.Add(2 * time.Hour)
	lifetimeTicks(t, s, start, mid, 10*time.Second, steadyHome)
	before := readLifetime(t, s, mid)

	// Three days ahead, before any tick carries the wrong time.
	ahead := readLifetime(t, s, mid.Add(72*time.Hour))
	sameFlows(t, "clock ahead", lifetimeFlows(ahead), lifetimeFlows(before))
	if _, through := lifetimeRowFlows(t, s, "base"); through > mid.UnixMilli() {
		t.Fatalf("a clock ahead moved the base to %s, past the stored history", time.UnixMilli(through).UTC())
	}

	// The clock is back; the next hour counts in full.
	end := mid.Add(time.Hour)
	lifetimeTicks(t, s, mid, end, 10*time.Second, steadyHome)
	after := readLifetime(t, s, end)
	sameFlows(t, "clock back", lifetimeFlows(after), storedFlows(t, s, start, end))

	// Two hours behind: history stored after that time still counts.
	later := end.Add(30 * time.Minute)
	lifetimeTicks(t, s, end, later, 10*time.Second, steadyHome)
	behind := readLifetime(t, s, end.Add(-2*time.Hour))
	sameFlows(t, "clock behind", lifetimeFlows(behind), storedFlows(t, s, start, later))
	sameFlows(t, "clock right again", lifetimeFlows(readLifetime(t, s, later)), storedFlows(t, s, start, later))
}

// The first sum waits for a history import, which may still add older rows.
func TestLifetimeEnergyWaitsForAHistoryImport(t *testing.T) {
	s := openLifetimeStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer s.Close()
	start := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	lifetimeTicks(t, s, start, end, 10*time.Second, steadyHome)

	s.historyMigration = &historyMigration{status: HistoryMigrationStatus{State: "running", Phase: "sqlite"}}
	l, ok, err := s.LifetimeEnergy(end)
	s.historyMigration = nil
	if err != nil || ok {
		t.Fatalf("during an import = %v, %v, %v; want not ready", l, ok, err)
	}
	if _, _, found, err := s.lifetimeRow(context.Background(), "base"); err != nil || found {
		t.Fatalf("a base was saved during the import: %v, %v", found, err)
	}
	sameFlows(t, "after the import", lifetimeFlows(readLifetime(t, s, end)), storedFlows(t, s, start, end))
}

// The one-time history rebuild keeps the totals and their floor.
func TestRetireRawHistoryKeepsLifetimeEnergy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openLifetimeStore(t, path)
	start := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	end := start.Add(3 * time.Hour)
	lifetimeTicks(t, s, start, end, 10*time.Second, steadyHome)
	shown := readLifetime(t, s, end)
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "grid_w", TsMs: start.UnixMilli(), Value: 5}}); err != nil {
		t.Fatal(err)
	}
	base, through := lifetimeRowFlows(t, s, "base")
	if err := s.RetireRawHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	var raw int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&raw); err != nil || raw != 0 {
		t.Fatalf("raw polls survived the rebuild: %d, %v", raw, err)
	}
	copied, copiedThrough := lifetimeRowFlows(t, s, "base")
	if copied != base || copiedThrough != through {
		t.Fatalf("rebuilt base %+v through %d, want %+v through %d", copied, copiedThrough, base, through)
	}
	floor, _ := lifetimeRowFlows(t, s, "floor")
	sameFlows(t, "rebuilt floor", lifetimeFlows(floor), lifetimeFlows(shown))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openLifetimeStore(t, path)
	defer s.Close()
	sameFlows(t, "after the rebuild", lifetimeFlows(readLifetime(t, s, end)), lifetimeFlows(shown))
}

// A restart at night between two folds returns what Home Assistant already
// saw, though the stored PV total has fallen since and the last fold saved
// a lower floor.
func TestLifetimeEnergyRestartAtNightBetweenFolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openLifetimeStore(t, path)
	start := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	// The read at 13:16 folds 12:00–12:15 and saves the floor as shown at
	// 13:15. Its last sunny minute runs the totals ahead of that floor by
	// less than the margin. The quiet night adds nearly nothing, so no read
	// saves the floor again before the next fold at 13:31.
	night := start.Add(76 * time.Minute)
	point := func(at time.Time) HistoryPoint {
		if !at.Before(night) {
			return HistoryPoint{GridW: 30, PVW: 20, LoadW: 10}
		}
		return HistoryPoint{GridW: 1500, PVW: -2000, BatW: 200, LoadW: 1300}
	}
	restart := start.Add(85 * time.Minute)
	var shown LifetimeEnergy
	for at := start; at.Before(restart); at = at.Add(time.Minute) {
		lifetimeTicks(t, s, at, at.Add(time.Minute), 10*time.Second, point)
		shown = readLifetime(t, s, at.Add(time.Minute))
		floor, _ := lifetimeRowFlows(t, s, "floor")
		if shown.above(floor, lifetimeFloorMarginWh) {
			t.Fatalf("%s: the saved floor %+v is more than %d Wh behind %+v", at.Format(time.TimeOnly), floor, lifetimeFloorMarginWh, shown)
		}
	}
	stored := storedFlows(t, s, start, restart)
	if stored["pv"] >= shown.PVWh {
		t.Fatalf("the test needs a night that lowers stored PV: stored %v, shown %v", stored["pv"], shown.PVWh)
	}
	floor, _ := lifetimeRowFlows(t, s, "floor")
	if floor.PVWh >= shown.PVWh {
		t.Fatalf("the test needs a saved floor below the shown PV: floor %v, shown %v", floor.PVWh, shown.PVWh)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openLifetimeStore(t, path)
	defer s.Close()
	sameFlows(t, "after a restart at night", lifetimeFlows(readLifetime(t, s, restart)), lifetimeFlows(shown))
}

// A found base that the history has passed by more than a week, as after a
// box was off or ran an older FTW, catches up a week per read.
func TestLifetimeEnergyCatchesUpAfterAGapOfOverAWeek(t *testing.T) {
	s := openLifetimeStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer s.Close()
	start := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	lifetimeTicks(t, s, start, start.Add(2*time.Hour), 10*time.Second, steadyHome)
	before := readLifetime(t, s, start.Add(2*time.Hour))

	back := start.Add(9 * 24 * time.Hour)
	end := back.Add(2 * time.Hour)
	lifetimeTicks(t, s, back, end, 10*time.Second, steadyHome)
	if l, ok, err := s.LifetimeEnergy(end); err != nil || ok {
		t.Fatalf("first read after the gap = %v, %v, %v; want not ready", l, ok, err)
	}
	after := readLifetime(t, s, end)
	notLower(t, "after the gap", after, before)
	sameFlows(t, "after the gap", lifetimeFlows(after), storedFlows(t, s, start, end))
}
