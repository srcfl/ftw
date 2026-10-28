package control

import (
	"log/slog"
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// PeakImportCeilingW bills like a Swedish power tariff: on the mean import
// over each local clock hour, the hours the planner's demand charges use
// (mpc/demand.go). The hour's budget is the ceiling for one hour. Only the
// site meter counts toward it. The fuse stays an instant limit.
const (
	// importHourChangeEvery is the fastest the hour's limits move.
	importHourChangeEvery = time.Minute
	// importHourRiseGap keeps a cut and the next rise apart.
	importHourRiseGap = 5 * time.Minute
	// importHourRiseMinLeft keeps a rise for at least a quarter hour before
	// the next hour starts again at the ceiling.
	importHourRiseMinLeft = 15 * time.Minute
)

// hourImport is the clock hour's import as the site meter measured it, and
// the two limits it sets. limitW is the mean import the rest of the hour may
// hold: above it the hour would overrun, so it cuts. riseW is the level an
// increase may reach. It rises in whole steps, keeps one step in hand and
// waits after a cut, so a charger does not hunt.
type hourImport struct {
	start      time.Time     // local clock hour
	importedWh float64       // measured import this hour
	unknown    time.Duration // time with no record; counts at the ceiling
	lastAt     time.Time     // newest site reading counted
	lastW      float64       // its import; NaN when only the record is known

	ceilingW         float64 // ceiling the limits were set for; 0 = not set
	limitW, riseW    float64
	changedAt, cutAt time.Time

	seen        bool    // a reading this hour has seen the ceiling setting
	seenCeiling float64 // the setting it saw, 0 when off
}

// ImportHourStart is the local clock hour that holds t.
func ImportHourStart(t time.Time) time.Time { return importHourStart(t, time.Local) }

func importHourStart(t time.Time, zone *time.Location) time.Time {
	l := t.In(zone)
	return l.Add(-time.Duration(l.Minute())*time.Minute - time.Duration(l.Second())*time.Second - time.Duration(l.Nanosecond()))
}

func (s *State) importHourStart(t time.Time) time.Time {
	zone := s.importHourZone
	if zone == nil {
		zone = time.Local
	}
	return importHourStart(t, zone)
}

// SeedHourImport restores the clock hour's import that state.db recorded up
// to at, so a restart keeps it. Time the record does not cover counts at the
// ceiling. Call it at boot, before the first ObserveSiteImport.
func (s *State) SeedHourImport(at time.Time, importWh float64, covered time.Duration) {
	if s == nil || !finiteFuseValue(importWh) || importWh < 0 {
		return
	}
	start := s.importHourStart(at)
	unknown := max(0, at.Sub(start)-covered)
	s.importHour = hourImport{start: start, importedWh: importWh, unknown: unknown, lastAt: at, lastW: math.NaN()}
}

// ObserveSiteImport adds the newest site-meter reading to the clock hour's
// import and moves the hour's limits. No device reading takes part. Call it
// every control tick under the control lock. A reading counts once, so a
// silent meter adds nothing and moves nothing.
func (s *State) ObserveSiteImport(store *telemetry.Store) {
	if s == nil || store == nil {
		return
	}
	gridW, at, ok := store.OnlineMeterW(s.SiteMeterDriver)
	h := &s.importHour
	if !ok || at.IsZero() || !at.After(h.lastAt) || at.After(s.now()) {
		return
	}
	w := math.Max(0, gridW)
	gap := false
	if h.lastAt.IsZero() {
		// No record: the hour so far is unknown.
		start := s.importHourStart(at)
		*h = hourImport{start: start, unknown: at.Sub(start)}
	} else {
		from := h.lastAt
		gap = at.Sub(from) > evSettle
		if at.Sub(from) > time.Hour {
			from = s.importHourStart(at) // Hours wholly unseen are over.
		}
		s.addHourImport(from, at, w, gap)
	}
	h.lastAt, h.lastW = at, w
	s.stepHourImportLimits(at, gap)
}

// addHourImport counts the import between two readings into each clock hour
// it spans. Across a gap the meter saw nothing; the gap counts at no less
// than the hour's rate when it began, so it can never make room.
func (s *State) addHourImport(from, to time.Time, w float64, gap bool) {
	h := &s.importHour
	prevW := h.lastW
	if math.IsNaN(prevW) {
		prevW = w
	}
	for from.Before(to) {
		if start := s.importHourStart(from); !start.Equal(h.start) {
			*h = hourImport{start: start, lastAt: h.lastAt, lastW: h.lastW}
		}
		end := h.start.Add(time.Hour)
		if to.Before(end) {
			end = to
		}
		rateW := (prevW + w) / 2
		if gap {
			rateW = math.Max(math.Max(prevW, w), s.hourRateW(from))
		}
		h.importedWh += rateW * end.Sub(from).Hours()
		from = end
	}
	if start := s.importHourStart(to); !start.Equal(h.start) {
		*h = hourImport{start: start, lastAt: h.lastAt, lastW: h.lastW} // A reading on the hour.
	}
}

// hourRateW is the mean import the rest of the hour may hold: the budget
// left over the time left. Zero while the ceiling is off.
func (s *State) hourRateW(at time.Time) float64 {
	ceiling := s.PeakImportCeilingW
	if !(ceiling > 0) {
		return 0
	}
	h := &s.importHour
	leftWh := ceiling*(1-h.unknown.Hours()) - h.importedWh
	left := max(time.Second, h.start.Add(time.Hour).Sub(at))
	return math.Max(0, leftWh/left.Hours())
}

// stepHourImportLimits moves the limits at most once a minute. The import
// limit follows the hour's rate. A rise needs a step in hand, a quarter hour
// left, and five minutes since the last cut. A gap never raises a limit.
func (s *State) stepHourImportLimits(at time.Time, gap bool) {
	h, ceiling := &s.importHour, s.PeakImportCeilingW
	if !(ceiling > 0) {
		ceiling = 0
	}
	if h.seen && ceiling != h.seenCeiling && ceiling > 0 {
		// A ceiling set during the hour counts from now: the rest of the
		// hour may average it, whatever the hour imported before.
		h.importedWh, h.unknown = 0, at.Sub(h.start)
	}
	h.seen, h.seenCeiling = true, ceiling
	if ceiling == 0 {
		h.ceilingW = 0
		return
	}
	rateW := s.hourRateW(at)
	if h.ceilingW != ceiling {
		// A new hour, a boot or a new ceiling.
		h.ceilingW, h.limitW, h.riseW = ceiling, rateW, math.Min(ceiling, rateW)
		h.changedAt, h.cutAt = at, time.Time{}
		return
	}
	if at.Sub(h.changedAt) < importHourChangeEvery || (gap && rateW >= h.limitW) {
		return
	}
	if rateW < h.limitW-1 {
		h.cutAt = at
		slog.Info("import ceiling: hour's budget cuts the limit", "hour", h.start.Format("15:04"),
			"imported_wh", h.importedWh, "limit_w", rateW, "was_w", h.limitW)
	}
	h.limitW, h.riseW, h.changedAt = rateW, math.Min(h.riseW, rateW), at
	step := evStepA * s.SiteFuseVoltage * float64(s.SiteFusePhases)
	left := h.start.Add(time.Hour).Sub(at)
	if gap || !(step > 0) || at.Sub(h.cutAt) < importHourRiseGap || left < importHourRiseMinLeft || rateW < h.riseW+2*step {
		return
	}
	h.riseW += step * (math.Floor((rateW-h.riseW)/step) - 1)
	slog.Info("import ceiling: hour's budget lets increases rise", "hour", h.start.Format("15:04"),
		"imported_wh", h.importedWh, "limit_w", rateW, "rise_w", h.riseW)
}

// importHourLimitsW returns the hour's import limit and the level an
// increase may reach. ok is false while the ceiling is off. Before the first
// reading, after a new ceiling or in a new hour they are the ceiling: that is
// the rate a whole hour may hold.
func (s *State) importHourLimitsW() (limitW, riseW float64, ok bool) {
	if s == nil || !(s.PeakImportCeilingW > 0) {
		return 0, 0, false
	}
	if !s.importHourMeasured() {
		return s.PeakImportCeilingW, s.PeakImportCeilingW, true
	}
	return s.importHour.limitW, s.importHour.riseW, true
}

// importHourMeasured reports whether this clock hour's import is measured
// against the ceiling in force.
func (s *State) importHourMeasured() bool {
	h := &s.importHour
	return s.PeakImportCeilingW > 0 && h.ceilingW == s.PeakImportCeilingW && s.importHourStart(s.now()).Equal(h.start)
}

// importRiseCeilingW is the import battery charge may reach beside an EV
// that still wants power (EV first). Never above effectiveImportCeilingW.
func (s *State) importRiseCeilingW(fuseMaxW float64) float64 {
	eff := s.effectiveImportCeilingW(fuseMaxW)
	if _, riseW, ok := s.importHourLimitsW(); ok && riseW < eff {
		eff = riseW
	}
	return eff
}

// PlanningImportCeiling is the ceiling as the planner gets it, from the hour
// the live limit counts: the clock hour in progress and what it has used.
// Time with no record counts at the ceiling. Until a reading measures the
// hour against this ceiling, the rest of the hour may hold the ceiling, so
// the time before counts as used. A planned hour holds the ceiling less half
// a rise step: the car runs in whole steps and a rise keeps a step in hand,
// so a car under the ceiling ends each hour 0.4-0.6 kWh short of it. Planned
// at the ceiling the plan promised the car 8-15 % more than it got; at half
// a step less the car gets as much, within 1 % of the promise with the
// battery covering the house and 6 % without
// (TestSiteSimPlanUnderImportCeiling). Zero while the ceiling is off.
// Callers hold the control lock.
func PlanningImportCeiling(s *State) mpc.ImportCeiling {
	if s == nil || !(s.PeakImportCeilingW > 0) {
		return mpc.ImportCeiling{}
	}
	now, ceiling := s.now(), s.PeakImportCeilingW
	start := s.importHourStart(now)
	elapsedWh := ceiling * now.Sub(start).Hours()
	if h := &s.importHour; s.importHourMeasured() {
		elapsedWh = h.importedWh + ceiling*h.unknown.Hours()
	}
	step := evStepA * s.SiteFuseVoltage * float64(s.SiteFusePhases)
	return mpc.ImportCeiling{W: ceiling, MeanW: max(0, ceiling-step/2), HourStart: start, ElapsedWh: elapsedWh}
}

// ImportHourStatus is the clock hour's import budget left, negative once it
// is overrun, and the import limit in force. ok is false while the ceiling is
// off. Callers hold the control lock or a copy of State.
func (s *State) ImportHourStatus(fuseMaxW float64) (leftWh, limitW float64, ok bool) {
	if limitW, _, ok = s.importHourLimitsW(); !ok {
		return 0, 0, false
	}
	leftWh = s.PeakImportCeilingW
	if h := &s.importHour; s.importHourStart(s.now()).Equal(h.start) {
		leftWh = s.PeakImportCeilingW*(1-h.unknown.Hours()) - h.importedWh
	}
	if fuseMaxW > 0 {
		limitW = s.effectiveImportCeilingW(fuseMaxW)
	}
	return leftWh, limitW, true
}
