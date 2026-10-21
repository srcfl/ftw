package control

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

var hourTestZone = time.FixedZone("CEST", 2*60*60)

// hourSite is the #1674 site with the ceiling on the clock hour's mean
// import, fed every second as main.go feeds it every tick. hourWh is the
// true import per clock hour, as state.db records it.
type hourSite struct {
	*evMeterSite
	hourWh map[time.Time]float64
	from   time.Time
}

func newHourSite(t *testing.T, ceilingW, houseW float64, start time.Time) *hourSite {
	t.Helper()
	h := &hourSite{hourWh: map[time.Time]float64{}, from: start}
	h.evMeterSite = newEVMeterSiteAt(t, ceilingW, houseW, start)
	s := h.evMeterSite
	s.onBoot = func() { s.state.importHourZone = hourTestZone }
	s.onBoot()
	s.beforeTick = func() {
		h.hourWh[importHourStart(s.now, hourTestZone)] += math.Max(0, s.houseW+s.evW) / 3600
		s.state.ObserveSiteImport(s.store)
	}
	return h
}

// restart boots Core again. With seed, it reads the hour's record first.
func (h *hourSite) restart(seed bool) {
	h.boot()
	if seed {
		start := importHourStart(h.now, hourTestZone)
		from := start
		if h.from.After(from) {
			from = h.from
		}
		h.state.SeedHourImport(h.now, h.hourWh[start], h.now.Sub(from))
	}
}

func (h *hourSite) plugIn() {
	h.unplugged = false
	h.ctrl.SetManualHold("garage", loadpoint.ManualHold{PowerW: 10350, PhaseMode: "3p", Persistent: true})
}

func (h *hourSite) meanW(hour time.Time) float64 { return h.hourWh[hour] }

func (h *hourSite) noStopAfterStart() {
	h.t.Helper()
	started := false
	for _, o := range h.offers {
		if o.w > 0 {
			started = true
		} else if started {
			h.t.Fatalf("the car stopped at %s: offers %v", o.at.In(hourTestZone).Format("15:04:05"), offerWatts(h.offers))
		}
	}
	if !started {
		h.t.Fatal("the car never started")
	}
}

func (h *hourSite) logChanges() {
	last := -1.0
	for _, o := range h.offers {
		if o.w != last {
			h.t.Logf("%s offer %.0f W", o.at.In(hourTestZone).Format("15:04:05"), o.w)
			last = o.w
		}
	}
	for hour, wh := range h.hourWh {
		h.t.Logf("hour %s mean %.0f W", hour.Format("15:04"), wh)
	}
}

func clock(day, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, hourTestZone)
}

// #1674, 23:45: a 7.5 kW ceiling, 0.9 kW of house load and the car at 9 A,
// 6.2 kW. On the hour's mean the charge runs steady, a step above what a
// per-second ceiling allowed, and never stops.
func TestImportCeilingHourMeanKeeps2345ChargeSteady(t *testing.T) {
	h := newHourSite(t, 7500, 900, clock(16, 22, 0))
	h.run(time.Hour)
	steadyFrom := h.now.Add(time.Minute)
	h.run(90 * time.Minute) // past 23:45 and midnight
	h.logChanges()
	h.noStopAfterStart()
	for _, o := range h.offersSince(steadyFrom) {
		if o.w != 6210 {
			t.Fatalf("offer %.0f W at %s, want a steady 6,210 W", o.w, o.at.In(hourTestZone).Format("15:04:05"))
		}
	}
	if h.evW != 6210 {
		t.Fatalf("car draws %.0f W, want 6,210 W", h.evW)
	}
	for _, hour := range []time.Time{clock(16, 22, 0), clock(16, 23, 0)} {
		if mean := h.meanW(hour); mean > 7500 {
			t.Fatalf("hour %s mean import %.0f W over the 7,500 W ceiling", hour.Format("15:04"), mean)
		}
	}
}

// A quiet first half-hour leaves budget. The car plugged in at 00:30 runs
// at full power, above the ceiling, and the hour's mean stays under it.
func TestImportCeilingQuietHalfHourLetsCarRunAboveCeiling(t *testing.T) {
	h := newHourSite(t, 7500, 900, clock(17, 0, 0))
	h.unplugged = true
	h.run(30 * time.Minute)
	h.plugIn()
	h.run(30*time.Minute - time.Second) // to 00:59:59
	h.logChanges()
	h.noStopAfterStart()
	if h.evW != 10350 || h.houseW+h.evW <= 7500 {
		t.Fatalf("car draws %.0f W, grid %.0f W: want 10,350 W, above the ceiling", h.evW, h.houseW+h.evW)
	}
	h.step() // 01:00
	if mean := h.meanW(clock(17, 0, 0)); mean > 7500 {
		t.Fatalf("hour mean import %.0f W over the 7,500 W ceiling", mean)
	}
}

// A house step late in the hour, as at 02:19 in #1674 but larger, takes
// the car down a step. The hour's budget still holds, so the car does not
// stop: under a per-second 7.5 kW ceiling, 4 kW of house load leaves less
// than its 4.14 kW minimum.
func TestImportCeilingHouseStepLateInHourStepsCarDown(t *testing.T) {
	h := newHourSite(t, 7500, 900, clock(16, 22, 0))
	h.run(110 * time.Minute) // steady at 6,210 W from 23:00
	stepAt := h.now
	h.setHouse(4000)
	h.run(10*time.Minute - time.Second) // to 23:59:59
	h.logChanges()
	h.noStopAfterStart()
	got := h.offersSince(stepAt)
	if len(got) == 0 || got[len(got)-1].w >= 6210 {
		t.Fatalf("offers after the house step %v, want a step down", offerWatts(got))
	}
	h.step() // midnight
	if mean := h.meanW(clock(16, 23, 0)); mean > 7500 {
		t.Fatalf("hour mean import %.0f W over the 7,500 W ceiling", mean)
	}
}

// The car held below its want by the hour's import ceiling says
// import_limit, not fuse_limit: at 23:00 in #1674 the phases sit near 12 A
// on a 25 A fuse. Held by the fuse instead, it says fuse_limit.
func TestImportCeilingSaysImportLimitNotFuse(t *testing.T) {
	for _, tc := range []struct {
		name             string
		ceilingW, houseW float64
		want             string
	}{
		{"hour's ceiling", 7500, 900, "import_limit"},
		{"fuse", 0, 9000, "fuse_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHourSite(t, tc.ceilingW, tc.houseW, clock(16, 22, 0))
			h.run(70 * time.Minute)
			st, _ := h.mgr.State("garage")
			if h.evW <= 0 || h.evW >= 10350 || st.CommandedReason != tc.want {
				t.Fatalf("car draws %.0f W of 10,350 W, reason %q, want %q", h.evW, st.CommandedReason, tc.want)
			}
		})
	}
}

// A Core restart mid-hour keeps the hour's import from state.db. A sauna
// spent most of the budget; without the record the box would count the
// first half-hour at the ceiling and let the car overrun the hour.
func TestImportCeilingRestartKeepsHoursImport(t *testing.T) {
	run := func(seed bool) (*hourSite, float64) {
		h := newHourSite(t, 7500, 9500, clock(16, 23, 0))
		h.run(25 * time.Minute)
		h.setHouse(900)
		h.run(5 * time.Minute)
		h.restart(seed)
		h.run(30*time.Minute - time.Second)
		h.step() // midnight
		return h, h.meanW(clock(16, 23, 0))
	}
	h, mean := run(true)
	h.logChanges()
	if mean > 7500 {
		t.Fatalf("with the record the hour's mean import is %.0f W, over the 7,500 W ceiling", mean)
	}
	if _, without := run(false); without <= 7500 {
		t.Fatalf("without the record the hour's mean is %.0f W; the test no longer needs the record", without)
	}
}

// The restart itself: the hour's import is the record, not zero, and the
// limits continue from it.
func TestImportCeilingSeedRestoresHoursImport(t *testing.T) {
	h := newHourSite(t, 7500, 9500, clock(16, 23, 0))
	h.run(25 * time.Minute)
	h.setHouse(900)
	h.run(5 * time.Minute)
	before := h.state.importHour.importedWh
	limitBefore, _, _ := h.state.importHourLimitsW()
	h.restart(true)
	h.step()
	after := h.state.importHour.importedWh
	if math.Abs(after-before) > 10 {
		t.Fatalf("hour's import %.0f Wh after the restart, %.0f Wh before", after, before)
	}
	limitAfter, _, _ := h.state.importHourLimitsW()
	if limitAfter >= 7500 || math.Abs(limitAfter-limitBefore) > 100 {
		t.Fatalf("limit %.0f W after the restart, %.0f W before", limitAfter, limitBefore)
	}
}

func newImportHourState(now *time.Time) (*State, *telemetry.Store) {
	st := NewState(0, 0, "meter")
	st.SiteFuseAmps, st.SiteFuseVoltage, st.SiteFusePhases, st.SiteFuseSafetyA = 25, 230, 3, 0.5
	st.PeakImportCeilingW = 7500
	st.importHourZone = hourTestZone
	st.clock = func() time.Time { return *now }
	return st, telemetry.NewStore()
}

func feedMeter(st *State, store *telemetry.Store, now *time.Time, at time.Time, w float64) {
	*now = at
	store.Update("meter", telemetry.DerMeter, w, nil, nil)
	store.Get("meter", telemetry.DerMeter).UpdatedAt = at
	store.RecordDriverSuccess("meter")
	st.ObserveSiteImport(store)
}

// A meter gap never makes room: the time unseen counts at no less than the
// rate the hour had when it began, and the reading that ends it moves no
// limit up, even when it shows the house far lower.
func TestImportCeilingMeterGapNeverRaisesLimits(t *testing.T) {
	var now time.Time
	st, store := newImportHourState(&now)
	at := clock(16, 23, 0)
	for ; at.Before(clock(16, 23, 40)); at = at.Add(2 * time.Second) {
		feedMeter(st, store, &now, at, 6420)
	}
	rate, imported := st.hourRateW(now), st.importHour.importedWh
	limit, rise, _ := st.importHourLimitsW()
	if rise <= 7500 {
		t.Fatalf("setup: rise %.0f W, want room above the ceiling", rise)
	}
	gapEnd := now.Add(45 * time.Second)
	feedMeter(st, store, &now, gapEnd, 900)
	if got := st.importHour.importedWh - imported; got < rate*45/3600 {
		t.Fatalf("the gap counted %.1f Wh, under the hour's rate %.0f W for 45 s", got, rate)
	}
	if st.hourRateW(now) > rate+1e-6 {
		t.Fatalf("the gap raised the hour's rate from %.0f to %.0f W", rate, st.hourRateW(now))
	}
	if l, r, _ := st.importHourLimitsW(); l > limit+1e-6 || r > rise+1e-6 {
		t.Fatalf("limits %.0f/%.0f W after the gap, %.0f/%.0f W before", l, r, limit, rise)
	}
	// Steady readings at the same low import would have raised the rate.
	steady, _ := newImportHourState(&now)
	steadyStore := telemetry.NewStore()
	for at := clock(16, 23, 0); !at.After(gapEnd); at = at.Add(2 * time.Second) {
		w := 6420.0
		if at.After(clock(16, 23, 40)) {
			w = 900
		}
		feedMeter(steady, steadyStore, &now, at, w)
	}
	if steady.hourRateW(now) <= rate {
		t.Fatalf("setup: steady readings left the rate at %.0f W", steady.hourRateW(now))
	}
}

// A gap across the hour's end: the new hour starts at the ceiling and counts
// its unseen start at no less than the ceiling.
func TestImportCeilingGapAcrossHourCountsNewHourAtCeiling(t *testing.T) {
	var now time.Time
	st, store := newImportHourState(&now)
	for at := clock(16, 23, 30); at.Before(clock(16, 23, 59)); at = at.Add(2 * time.Second) {
		feedMeter(st, store, &now, at, 900)
	}
	feedMeter(st, store, &now, clock(16, 23, 59).Add(50*time.Second), 900)
	feedMeter(st, store, &now, clock(17, 0, 0).Add(30*time.Second), 900)
	if !st.importHour.start.Equal(clock(17, 0, 0)) {
		t.Fatalf("hour %v, want midnight", st.importHour.start)
	}
	if got := st.importHour.importedWh; got < 7500*30/3600.0 {
		t.Fatalf("the new hour's unseen 30 s counted %.1f Wh, under the ceiling", got)
	}
	if l, r, _ := st.importHourLimitsW(); l > 7500+1e-6 || r > 7500+1e-6 {
		t.Fatalf("new hour's limits %.0f/%.0f W, above the ceiling", l, r)
	}
}

// Readings count toward the local clock hour they fall in. Before any
// reading of a new hour, its limits are the ceiling.
func TestImportCeilingCountsPerLocalClockHour(t *testing.T) {
	var now time.Time
	st, store := newImportHourState(&now)
	for at := clock(16, 23, 59); !at.After(clock(17, 0, 0).Add(time.Second)); at = at.Add(time.Second) {
		feedMeter(st, store, &now, at, 3600)
	}
	if got := st.importHour.importedWh; math.Abs(got-1) > 1e-9 {
		t.Fatalf("the new hour's first second counted %.3f Wh, want 1 Wh", got)
	}
	st.importHour.limitW, st.importHour.riseW = 20000, 20000
	now = clock(17, 1, 2) // no reading in this hour yet
	if l, r, _ := st.importHourLimitsW(); l != 7500 || r != 7500 {
		t.Fatalf("an unread hour's limits %.0f/%.0f W, want the ceiling", l, r)
	}
	if got := importHourStart(time.Date(2026, 10, 17, 10, 47, 3, 0, time.FixedZone("IST", 19800)), time.UTC); !got.Equal(time.Date(2026, 10, 17, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("UTC hour %v", got)
	}
	ist := time.FixedZone("IST", 19800)
	if got := importHourStart(time.Date(2026, 10, 17, 10, 47, 3, 0, ist), ist); !got.Equal(time.Date(2026, 10, 17, 10, 0, 0, 0, ist)) {
		t.Fatalf("local hour %v, want 10:00 IST", got)
	}
}

// #1674, Göran, 17 October 14:26–14:28 UTC (16:26 here): the 7.5 kW ceiling
// went on with the car at 10,350 W beside 3.3 kW of house load. The car came
// down to its 4,140 W minimum; one reading of 7,524 W, 24 W over the ceiling,
// then stopped it, and it never restarted. On the hour's mean, 3.3 + 4.14 kW
// = 7.44 kW holds the ceiling: the lone reading changes nothing.
func TestImportCeilingLoneReadingOverCeilingKeepsMinimum(t *testing.T) {
	h := newHourSite(t, 0, 3300, clock(17, 16, 0))
	h.run(26*time.Minute + 8*time.Second)
	h.ceilingW, h.state.PeakImportCeilingW = 7500, 7500
	onAt, onWh := h.now, h.hourWh[clock(17, 16, 0)]
	h.run(time.Minute + 50*time.Second) // to 16:27:58
	h.setHouse(3384)
	h.step() // 16:27:59
	if grid := h.houseW + h.evW; grid != 7524 {
		t.Fatalf("setup: grid %.0f W at 16:27:59, want 7,524 W", grid)
	}
	h.setHouse(3300)
	h.run(32*time.Minute + time.Second) // to 17:00
	h.logChanges()
	got := h.offersSince(onAt)
	for _, o := range got {
		if o.w < 4140 {
			t.Fatalf("offer %.0f W at %s under the ceiling", o.w, o.at.In(hourTestZone).Format("15:04:05"))
		}
	}
	if len(got) == 0 || got[len(got)-1].w != 4140 {
		t.Fatalf("offers %v, want the car held at 4,140 W", offerWatts(got))
	}
	mean := (h.hourWh[clock(17, 16, 0)] - onWh) / h.now.Sub(onAt).Hours()
	if mean > 7500 {
		t.Fatalf("import averaged %.0f W since the ceiling went on", mean)
	}
}

// A stop under the ceiling restarts on the hour's budget: the dwell's mean
// house load leaves room for the 4,140 W minimum, without the step of margin
// the fuse keeps. With 3.3 kW of house load that room is 4.2 kW; a step of
// margin on the latest reading (4,830 W) never restarted the car.
func TestImportCeilingRestartsOnHoursBudget(t *testing.T) {
	h := newHourSite(t, 7500, 3300, clock(17, 16, 0))
	h.run(20 * time.Minute)
	if h.offer() != 4140 {
		t.Fatalf("setup: offer %.0f W, want the 4,140 W minimum", h.offer())
	}
	h.setHouse(3700) // 7.84 kW: a real overage, not one reading
	h.run(2 * time.Minute)
	if h.offer() != 0 {
		t.Fatalf("offer %.0f W after two minutes over the hour's budget, want a stop", h.offer())
	}
	backAt := h.now
	h.setHouse(3300)
	h.run(5 * time.Minute)
	h.logChanges()
	got := h.offersSince(backAt)
	if len(got) == 0 || got[len(got)-1].w != 4140 {
		t.Fatalf("offers after the house came back %v, want a restart at 4,140 W", offerWatts(got))
	}
	restartedAt := got[len(got)-1].at
	h.run(32*time.Minute + time.Second) // to 17:00
	h.logChanges()
	for _, o := range h.offersSince(restartedAt) {
		if o.w == 0 {
			t.Fatalf("the car stopped again at %s", o.at.In(hourTestZone).Format("15:04:05"))
		}
	}
	if mean := h.meanW(clock(17, 16, 0)); mean > 7500 {
		t.Fatalf("hour mean import %.0f W over the 7,500 W ceiling", mean)
	}
}

// A ceiling set during the hour counts from then: the rest of the hour may
// average it, whatever the hour imported before.
func TestImportCeilingSetMidHourCountsFromThen(t *testing.T) {
	var now time.Time
	st, store := newImportHourState(&now)
	st.PeakImportCeilingW = 0
	at := clock(17, 16, 0)
	for ; !at.After(clock(17, 16, 26)); at = at.Add(2 * time.Second) {
		feedMeter(st, store, &now, at, 13650)
	}
	st.PeakImportCeilingW = 7500
	feedMeter(st, store, &now, at, 13650)
	if l, r, _ := st.importHourLimitsW(); math.Abs(l-7500) > 1e-6 || math.Abs(r-7500) > 1e-6 {
		t.Fatalf("limits %.0f/%.0f W, want the ceiling from now on", l, r)
	}
	if left, _, _ := st.ImportHourStatus(17250); math.Abs(left-7500*now.Sub(clock(17, 16, 0)).Hours()*-1-7500) > 1 {
		t.Fatalf("budget left %.0f Wh, want the ceiling for the time left", left)
	}
}

// A car that could run only on the hour's slack does not start: with 5 kW
// of house load under a 7.5 kW ceiling, its 4.14 kW minimum fits only late
// in each hour and would stop when the next one starts.
func TestImportCeilingNoStartOnSlackAlone(t *testing.T) {
	h := newHourSite(t, 7500, 5000, clock(16, 21, 0))
	h.run(3 * time.Hour)
	h.logChanges()
	for _, o := range h.offers {
		if o.w > 0 {
			t.Fatalf("the car started at %s on slack alone", o.at.In(hourTestZone).Format("15:04:05"))
		}
	}
}

// A quiet first 40 minutes leave the hour so much slack that its limits sit
// above what the 25 A fuse allows. An 8 kW house still keeps an idle car from
// starting, by the ceiling's own start test, and the car says import_limit.
func TestImportCeilingStartTestSaysImportLimit(t *testing.T) {
	h := newHourSite(t, 7500, 0, clock(16, 21, 0))
	h.unplugged = true
	h.run(40 * time.Minute)
	h.setHouse(8000)
	h.plugIn()
	h.run(5 * time.Minute)
	if h.offer() != 0 {
		t.Fatalf("the car started at %.0f W beside 8 kW under a 7.5 kW ceiling", h.offer())
	}
	if st, _ := h.mgr.State("garage"); st.CommandedReason != "import_limit" {
		t.Fatalf("idle under the ceiling says %q, want import_limit", st.CommandedReason)
	}
}

// A reading after a clock jump of years counts only the hour it falls in.
func TestImportCeilingClockJumpCountsOnlyItsHour(t *testing.T) {
	var now time.Time
	st, store := newImportHourState(&now)
	feedMeter(st, store, &now, time.Date(1970, 1, 1, 0, 0, 2, 0, hourTestZone), 900)
	feedMeter(st, store, &now, clock(16, 23, 30), 900)
	if !st.importHour.start.Equal(clock(16, 23, 0)) {
		t.Fatalf("hour %v, want 23:00", st.importHour.start)
	}
	if got := st.importHour.importedWh; math.Abs(got-7500*0.5) > 1e-6 {
		t.Fatalf("the unseen half-hour counted %.1f Wh, want 3,750 Wh at the ceiling", got)
	}
}

// The status shows the budget left and the limit, and nothing while off.
func TestImportHourStatus(t *testing.T) {
	var now time.Time
	st, store := newImportHourState(&now)
	for at := clock(16, 23, 0); !at.After(clock(16, 23, 30)); at = at.Add(2 * time.Second) {
		feedMeter(st, store, &now, at, 3000)
	}
	left, limit, ok := st.ImportHourStatus(17250)
	if !ok || math.Abs(left-6000) > 2 || math.Abs(limit-12000) > 5 {
		t.Fatalf("status %v: left %.1f Wh limit %.1f W, want 6,000 Wh and 12,000 W", ok, left, limit)
	}
	st.PeakImportCeilingW = 0
	if _, _, ok := st.ImportHourStatus(17250); ok {
		t.Fatal("status reported with the ceiling off")
	}
}
