package control

import (
	"log/slog"
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// evHeadroomW is the shared grid balance for an EV pool. Power outside the
// pool stays in gridW. No unmeasured future battery relief earns headroom.
func evHeadroomW(ceiling, gridW, poolW float64) float64 {
	if !finiteFuseValue(ceiling) || !finiteFuseValue(gridW) || !finiteFuseValue(poolW) || ceiling <= 0 || poolW < 0 {
		return 0
	}
	return math.Max(0, ceiling-gridW+poolW)
}

func finiteFuseValue(w float64) bool { return !math.IsNaN(w) && !math.IsInf(w, 0) }

// evSettle is how long one of our commands may take to reach the site meter
// when the device declares nothing longer.
const evSettle = loadpoint.CommandSettle

// evUnknownSettle is how long an EV rise counts as flowing when the charger
// declares no response time: the slowest we know, Easee Cloud's 180 s.
const evUnknownSettle = 3 * time.Minute

// evRestartDwell is how long the meter must keep showing room before an idle
// pool starts again. It stays above Easee's ~30 s contactor minimum.
const evRestartDwell = 35 * time.Second

// evPhaseMarginA is the working margin below the breaker on each phase. One
// amp on the pool's phases is also the rise step and the gap between the cut
// and rise thresholds.
const evPhaseMarginA = 1.0

// fuseHourFactor is the phase current, as a multiple of the fuse rating,
// that a gG fuse of 16-63 A carries for an hour. A cut on its way covers the
// overage it was sent for only while every phase's filtered current stays
// at or below it.
const fuseHourFactor = 1.25

// sentCommand is one of our own commands, never a device reading.
type sentCommand struct {
	at time.Time
	w  float64
	a  float64 // EV offers: current on each phase it may land on
	n  int     // EV offers: the phases it lands on, 1 or the site's
}

// commandLog keeps a device's commands for evSettle, plus the one in force
// when that window opened. The meter may show any of them.
type commandLog []sentCommand

func (l commandLog) add(now time.Time, w float64) commandLog {
	return l.keep(sentCommand{at: now, w: w}, now.Add(-evSettle))
}

// keep appends c and drops the commands replaced before from.
func (l commandLog) keep(c sentCommand, from time.Time) commandLog {
	l = append(l, c)
	i := 0
	for i+1 < len(l) && !l[i+1].at.After(from) {
		i++
	}
	if i > 0 {
		return append(commandLog(nil), l[i:]...)
	}
	return l
}

// span returns the lowest, highest and latest command the meter may show.
// When the log is younger than evSettle, the meter may still show the time
// before our first command, which counts as 0.
func (l commandLog) span(now time.Time) (low, high, latest float64, ok bool) {
	for j, c := range l {
		if j+1 < len(l) && now.Sub(l[j+1].at) >= evSettle {
			continue // Replaced before the window opened.
		}
		if !ok || c.w < low {
			low = c.w
		}
		if !ok || c.w > high {
			high = c.w
		}
		latest, ok = c.w, true
	}
	if ok && now.Sub(l[0].at) < evSettle {
		low, high = math.Min(low, 0), math.Max(high, 0)
	}
	return low, high, latest, ok
}

// unshown is how much more a charger may draw than a reading taken at `at`
// shows, in watts and in current per phase: the highest offer sent since
// settle before the reading and not replaced by a lower one, less the
// lowest offer the reading may show. An offer set before that window is in
// the reading. A cut needs nothing: the reading shows the draw until the
// cut lands, and the charger moves to its latest order, so a cut also ends
// the reservation of the higher offers before it. Before our first offer
// the charger counts as 0.
func (l commandLog) unshown(at time.Time, settle time.Duration) (w, a float64) {
	from := at.Add(-settle)
	var settledW, settledA float64
	lowW, lowA, highW, highA := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, c := range l {
		if !c.at.After(from) {
			settledW, settledA = c.w, c.a
			continue
		}
		if c.w < highW {
			highW = c.w
		}
		if c.a < highA {
			highA = c.a
		}
		highW, highA = math.Max(highW, c.w), math.Max(highA, c.a)
		if !c.at.After(at) {
			lowW, lowA = math.Min(lowW, c.w), math.Min(lowA, c.a)
		}
	}
	lowW, lowA = math.Min(lowW, settledW), math.Min(lowA, settledA)
	return math.Max(0, highW-lowW), math.Max(0, highA-lowA)
}

// toCome is what a charger's latest offer will still change in a reading
// taken at `at`, as a cut counts it, in watts and in current per phase:
// positive is a raise on its way, negative a cut on its way. The reading may
// show the offer in force at from or one sent since, up to `at`, but a cut
// and every offer after it count as not shown yet. Of the offers before the
// first cut, a raise counts from the lowest. An offer on other phases than
// the latest says nothing of the latest's phases, so the latest then counts
// in full as a raise on each. Before the first logged offer the charger
// counts as before.
func (l commandLog) toCome(from, at time.Time, before sentCommand) (w, a float64) {
	settled, latest := before, before
	for _, c := range l {
		latest = c
		if !c.at.After(from) {
			settled = c
		}
	}
	low, prev, cutW, cutA := settled, settled, false, false
	otherPhases := settled.a > 0 && settled.n != latest.n
	for _, c := range l {
		if !c.at.After(from) || c.at.After(at) {
			continue
		}
		otherPhases = otherPhases || c.a > 0 && c.n != latest.n
		cutW, cutA = cutW || c.w < prev.w, cutA || c.a < prev.a
		if !cutW {
			low.w = math.Min(low.w, c.w)
		}
		if !cutA {
			low.a = math.Min(low.a, c.a)
		}
		prev = c
	}
	if otherPhases {
		low.a = 0
	}
	return latest.w - low.w, latest.a - low.a
}

// chargerOffers is one charger's offers, kept for the time its driver
// declares its power takes to show.
type chargerOffers struct {
	log    commandLog
	settle time.Duration
	// found is the offer in force before the limit's first site reading,
	// after a restart: Core did not send it in this run. A rise counts the
	// charger as 0 before it; a cut takes it as shown, since the reading is
	// all Core knows of it.
	found sentCommand
}

// pendingLoad is load we ordered that a site reading may not show yet:
// each EV offer sent above what the reading may show, until its charger
// can have applied it. A single-phase offer may land on any phase. Rises
// and starts count it on top of the meter; a cut never waits for it.
// stepEVMeterLimit adds our unshown battery orders (withBatteryOrders).
type pendingLoad struct {
	w      float64    // in total import
	phaseA [3]float64 // on each phase
}

func (s *State) pendingLoad(at time.Time) pendingLoad {
	var p pendingLoad
	for _, c := range s.evLimit.offers {
		w, a := c.log.unshown(at, c.settle)
		p.w += w
		for i := range p.phaseA {
			p.phaseA[i] += a
		}
	}
	return p
}

// unshownOrders is what a cut counts beside a reading taken at `at`: rise,
// the load our latest offers add that the reading may not show yet, and
// cover, the load it may still show that our cuts on their way take off
// (toCome). latestW is the latest offers. A cut covers only within the run
// over the limit it was sent in, for its charger's response time: once the
// meter has shown every phase under the limit for evSettle, a cut sent
// before counts as shown. Once the run's first cut has outlasted that time, a cut covers for
// evSettle, as before; so it does without phase currents, where Core
// cannot see the hottest phase. A single-phase cut covers on the phase
// it lands on, which Core does not know, so with other chargers in the pool
// it covers no phase.
func (s *State) unshownOrders(at time.Time, perPhase bool) (rise, cover pendingLoad, latestW float64) {
	lim := &s.evLimit
	for _, c := range lim.offers {
		w, a := c.log.toCome(at.Add(-c.settle), at, c.found)
		coverFor := c.settle
		if !perPhase || !lim.fuseCutAt.IsZero() && at.Sub(lim.fuseCutAt) > c.settle {
			coverFor = evSettle
		}
		from := at.Add(-coverFor)
		if lim.fuseOverAt.After(from) {
			from = lim.fuseOverAt
		}
		runW, runA := c.log.toCome(from, at, c.found)
		coverA := math.Max(0, math.Min(-a, -runA))
		var latest sentCommand
		if n := len(c.log); n > 0 {
			latest = c.log[n-1]
		}
		if latest.n == 1 && len(lim.offers) > 1 {
			coverA = 0
		}
		rise.w += math.Max(0, w)
		cover.w += math.Max(0, math.Min(-w, -runW))
		for i := range rise.phaseA {
			rise.phaseA[i] += math.Max(0, a)
			cover.phaseA[i] += coverA
		}
		latestW += latest.w
	}
	return rise, cover, latestW
}

// recordEVOffers keeps each plugged charger's offers, with the current each
// puts on the phases its command names, for the longer of evSettle and the
// power_max_age_s its driver declares, or evUnknownSettle when it declares
// none. Our own commands only: a charger's power reading never counts.
func (s *State) recordEVOffers(store *telemetry.Store, points []loadpoint.State) {
	now := s.now()
	read := now
	if !s.evLimit.readingAt.IsZero() && s.evLimit.readingAt.Before(now) {
		read = s.evLimit.readingAt
	}
	offers := make(map[string]chargerOffers, len(points))
	for _, point := range points {
		if !point.PluggedIn || !point.CommandedKnown || !finiteFuseValue(point.CommandedW) {
			continue
		}
		c := s.evLimit.offers[point.ID]
		c.settle = evUnknownSettle
		if r := store.Get(point.DriverName, telemetry.DerEV); r != nil {
			if declared := telemetry.DeclaredPowerMaxAge(r.Data); declared > 0 {
				c.settle = max(evSettle, declared)
			}
		}
		w := math.Max(0, point.CommandedW)
		a, phases := s.offerPhaseA(w, point.CommandedPhases)
		sent := sentCommand{at: now, w: w, a: a, n: phases}
		if n := len(c.log); n == 0 || c.log[n-1].w != sent.w || c.log[n-1].a != sent.a || c.log[n-1].n != sent.n {
			if n == 0 && !s.evLimit.set {
				c.found = sent
			}
			c.log = c.log.keep(sent, read.Add(-c.settle))
		}
		offers[point.ID] = c
	}
	s.evLimit.offers = offers
}

// offerPhaseA is the current an offer of w puts on each phase it may land
// on, and how many phases it lands on. Unknown phases count as one.
func (s *State) offerPhaseA(w float64, phases int) (float64, int) {
	n := 1
	if phases == 3 && s.SiteFusePhases > 1 {
		n = s.SiteFusePhases
	}
	if s.SiteFuseVoltage <= 0 {
		return 0, n
	}
	return w / (s.SiteFuseVoltage * float64(n)), n
}

// EVFusePhaseRoomA is the current the chargers may still add on the
// busiest phase: below the fuse less its margin, the last reading's
// current or heat, and every charger's offers that reading may not show.
// false when the site meter reports no phase currents.
func EVFusePhaseRoomA(state *State) (float64, bool) {
	if state == nil || !state.evLimit.set || !state.evLimit.phaseRoomKnown {
		return 0, false
	}
	p := state.pendingLoad(state.evLimit.readingAt)
	return state.evLimit.phaseRoomA - max(p.phaseA[0], p.phaseA[1], p.phaseA[2]), true
}

// evMeterLimit is the managed EVs' common hard cap. It reads only the site
// meter, which measures the protected quantities, and our own commands. No
// charger, car or battery reading takes part in a cut, a rise or a start.
type evMeterLimit struct {
	set            bool
	capW           float64
	cutAt          time.Time                // last cut
	readingAt      time.Time                // newest site reading seen
	roomSince      time.Time                // first reading of the current run with room to start
	offers         map[string]chargerOffers // each plugged charger's, by loadpoint
	phaseRoomA     float64                  // below the working limit on the busiest phase at readingAt
	phaseRoomKnown bool                     // the meter reports phase currents
	overSince      time.Time                // first reading of a run over the hour's import limit
	fuseOverAt     time.Time                // first reading of a run with a phase, or the total, over the fuse's working limit
	fuseCutAt      time.Time                // the first cut of that run
	fuseUnderAt    time.Time                // first reading of a stretch under that limit
	overOffers     map[string]float64       // each charger's offer when that run began
	idleWh         float64                  // import since roomSince, for the ceiling's start test
	byImport       bool                     // the hour's import limit, not the fuse, left less room at readingAt
}

type evPool struct {
	offerW, wantW, minW, phases float64
	offerKnown                  bool
	offers                      map[string]float64 // per charger driver
}

// EVFuseBudget returns the managed EVs' common hard cap, which the loadpoint
// controller divides between cars. Callers hold the control lock, and the
// shared freshness gate must already allow dispatch. A fuse overage in total
// import cuts at once; a phase overage cuts once that phase's heat budget is
// spent, or at once above fuseInstantFactor. Otherwise the cap holds; it
// rises one step on a fresh reading with room for two, at least evSettle
// after the last cut. Room counts every
// offer sent above what the reading may show as flowing until its charger
// can have applied it; a cut never frees room before the meter shows it.
// A cut counts our orders both ways: a raise on its way adds to the overage,
// and a cut on its way covers the overage it was sent for until its charger
// can have applied it, within its run over the limit and while the phases
// stay at or below fuseHourFactor (unshownOrders).
// EVFusePhaseRoomA gives the same room per phase to the split between cars.
// An idle pool starts only after the meter has kept showing
// room for its minimum plus a step. Each reading acts once, so a silent
// meter can never raise or restart a charge.
func EVFuseBudget(store *telemetry.Store, state *State, points []loadpoint.State, configs []loadpoint.Config, fuseMaxW float64) (float64, bool) {
	if store == nil || state == nil || !finiteFuseValue(fuseMaxW) || fuseMaxW <= 0 {
		return 0, true
	}
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	if meter == nil {
		return 0, true
	}
	gridW := meter.RawW
	if !finiteFuseValue(gridW) {
		return 0, true
	}
	state.stepFuseHeat(meter)
	lim := &state.evLimit
	pool, plugged := evPoolFor(state, points, configs)
	if !plugged {
		*lim = evMeterLimit{}
		return 0, true
	}
	if pool.offerKnown {
		state.evOffers = state.evOffers.add(state.now(), pool.offerW)
	}
	state.recordEVOffers(store, points)
	if !lim.set || meter.UpdatedAt.After(lim.readingAt) {
		state.stepEVMeterLimit(meter, gridW, pool, fuseMaxW)
	}
	return lim.capW, true
}

func (s *State) stepEVMeterLimit(meter *telemetry.DerReading, gridW float64, pool evPool, fuseMaxW float64) {
	lim, now, at := &s.evLimit, s.now(), meter.UpdatedAt
	first, prevAt := !lim.set, lim.readingAt
	gap := first || at.Sub(prevAt) > evSettle
	lim.readingAt = at
	prev := math.Inf(1)
	if !first {
		prev = lim.capW
	}
	lim.set = true
	peakW := pool.offerW
	if _, high, _, ok := s.evOffers.span(now); ok {
		peakW = math.Max(peakW, high)
	}
	// Our own battery orders the meter may not show yet count beside the
	// chargers' offers, in total import and on each phase.
	pending := s.pendingBatteryCommandsW(now)
	r := s.evMeterRoom(meter, gridW, pool.phases, fuseMaxW, s.withBatteryOrders(s.pendingLoad(at), pending))
	_, fresh := qualifiedFuseObservation(meter, now)
	fresh = fresh && r.measured
	lim.phaseRoomA, lim.phaseRoomKnown = 0, r.phaseRoomKnown
	if fresh {
		lim.phaseRoomA = r.phaseRoomA
	}
	step := evPhaseMarginA * s.SiteFuseVoltage * pool.phases
	// The label follows whichever limit leaves less room.
	lim.byImport = math.Min(r.limitW, r.riseW) < r.fuseW
	// The fuse cuts on every reading over it, by its heat budget where the
	// meter reports phase currents. It takes off the latest offers only what
	// the cuts the reading may still show do not cover, plus the rises it may
	// not show yet. Over the fuse, the cap never rises. The hour's budget
	// absorbs a short overage of less than a step: it cuts once the overage
	// has lasted evSettle, so one reading over the ceiling changes nothing.
	// A run over the fuse's working limit ends once every phase has read
	// under it for evSettle; a short dip, as battery relief gives, does not.
	switch {
	case r.overLimit:
		lim.fuseUnderAt = time.Time{}
		if lim.fuseOverAt.IsZero() || gap {
			lim.fuseOverAt, lim.fuseCutAt = at, time.Time{}
		}
	case lim.fuseUnderAt.IsZero():
		lim.fuseUnderAt = at
	case at.Sub(lim.fuseUnderAt) >= evSettle:
		lim.fuseOverAt, lim.fuseCutAt = time.Time{}, time.Time{}
	}
	rise, cover, latestW := s.unshownOrders(at, r.perPhase)
	fuseCutW := r.fuseOverW + rise.w - cover.w
	if r.perPhase {
		// A single-phase order may land on any phase, so the cut takes the
		// most a rise adds to a phase and the least a cut takes off one.
		// Past the current a gG fuse carries for an hour, nothing on its
		// way holds back a cut.
		coverA := min(cover.phaseA[0], cover.phaseA[1], cover.phaseA[2])
		if s.fuseHeat.worstA() > fuseHourFactor*s.SiteFuseAmps {
			coverA = 0
		}
		fuseCutW = (r.fuseOverA + max(rise.phaseA[0], rise.phaseA[1], rise.phaseA[2]) - coverA) * s.SiteFuseVoltage * pool.phases
	}
	over, capW, byImport := r.fuseOverW > 0, math.Inf(1), false
	if over {
		capW = latestW - math.Max(0, fuseCutW)
	}
	if r.limitW >= 0 {
		lim.overSince = time.Time{}
	} else {
		if lim.overSince.IsZero() || gap {
			lim.overSince, lim.overOffers = at, pool.offers
		}
		if !r.hourly || gap || -r.limitW >= step || at.Sub(lim.overSince) >= evSettle {
			// The meter may still show any offer of the last evSettle.
			// Taking the overage from the highest cuts each measured watt once.
			hourW := peakW + r.limitW
			byImport, capW, over = !over || hourW < capW, math.Min(capW, hourW), true
		}
	}
	if over {
		lim.byImport = byImport
		lim.capW, lim.roomSince = math.Min(prev, math.Max(0, capW)), time.Time{}
		if lim.capW < prev {
			lim.cutAt = now
			if r.fuseOverW > 0 && lim.fuseCutAt.IsZero() {
				lim.fuseCutAt = now
			}
			slog.Info("EV hard limit: site meter over limit", "grid_w", gridW, "worst_phase_a", r.worstA,
				"fuse_heat_a", s.fuseHeat.worstA(), "import_limit_w", s.effectiveImportCeilingW(fuseMaxW),
				"overage_w", r.fuseOverW, "import_overage_w", math.Max(0, -r.limitW),
				"unshown_rise_w", rise.w, "unshown_cut_w", cover.w, "offer_w", latestW, "cap_w", lim.capW)
		}
		return
	}
	// An increase needs a fresh reading and room below the level increases
	// may reach, left after our own battery commands that the meter may not
	// show yet (in r). Between that level and the import limit the cap holds.
	room := 0.0
	if fresh {
		room = math.Max(0, math.Min(r.fuseW, r.riseW))
	}
	if pool.offerW <= 0 {
		s.stepEVIdleStart(r, gridW, pool, first, gap, fresh, at, prevAt, pending, step)
		return
	}
	// Keeping the current offers is never a cut. Headroom above them stays
	// within what this reading measured.
	lim.capW, lim.roomSince = math.Min(prev, peakW+room), time.Time{}
	// Right after a cut the meter may show battery relief that covers it.
	if lim.capW < pool.wantW && room >= 2*step && now.Sub(lim.cutAt) >= evSettle {
		// One step at most, and a step of room stays free.
		lim.capW = math.Max(lim.capW, math.Min(lim.capW+step, peakW+room-step))
	}
}

// stepEVIdleStart starts an idle pool once the meter has kept showing room
// for evRestartDwell: room on every reading for its minimum plus a step
// below the fuse. Under a measured hour, the ceiling's part is judged on the
// dwell's mean import, the house's load rather than one reading of it, and
// the minimum needs no step of margin there: a short overage waits.
func (s *State) stepEVIdleStart(r evMeterRoom, gridW float64, pool evPool, first, gap, fresh bool, at, prevAt time.Time, pending, step float64) {
	lim := &s.evLimit
	lim.capW = 0
	perReadingW := r.fuseW
	if !r.hourly {
		perReadingW = math.Min(perReadingW, r.riseW)
	} else {
		// The start test below holds an idle car to the ceiling itself,
		// whatever slack the hour has: name it by this reading.
		lim.byImport = r.startLevelW-gridW-pending < r.fuseW
	}
	room := 0.0
	if fresh {
		room = math.Max(0, perReadingW) // r holds our battery orders.
	}
	if room < pool.minW+step {
		lim.roomSince = time.Time{}
		return
	}
	switch {
	case first:
		// No stop of ours to wait out.
		lim.roomSince, lim.idleWh = at.Add(-evRestartDwell), math.Max(0, gridW)*evRestartDwell.Hours()
	case lim.roomSince.IsZero() || gap:
		lim.roomSince, lim.idleWh = at, 0
	default:
		lim.idleWh += math.Max(0, gridW) * at.Sub(prevAt).Hours()
	}
	dwell := at.Sub(lim.roomSince)
	if dwell < evRestartDwell {
		return
	}
	lim.capW = room - step
	if !r.hourly {
		return
	}
	ceilingRoomW := r.startLevelW - lim.idleWh/dwell.Hours() - pending
	if ceilingRoomW < pool.minW {
		lim.capW, lim.roomSince, lim.idleWh, lim.byImport = 0, at, 0, true // Judge the next dwell.
		return
	}
	lim.capW = math.Min(lim.capW, ceilingRoomW)
}

// EVLimitReason names the limit behind the managed EVs' cap for the
// loadpoints' reason: "import_limit" when the clock hour's import ceiling
// left less room than the fuse at the last site reading, else "fuse_limit".
func EVLimitReason(state *State) string {
	if state != nil && state.evLimit.set && state.evLimit.byImport {
		return "import_limit"
	}
	return "fuse_limit"
}

// evMeterRoom is the measured room in EV watts. The fuse's part holds on
// every reading. The import ceiling's parts are +Inf while it is off.
type evMeterRoom struct {
	fuseW          float64 // below the fuse less its margin and the worst phase, read or heat, and what we sent; rises and starts
	fuseOverW      float64 // the fuse's measured overage that cuts now, 0 under it; see fuseHeat
	fuseOverA      float64 // the same on the worst phase, with perPhase
	perPhase       bool    // the phase currents measure the fuse
	overLimit      bool    // a phase, or the total without phases, reads over the fuse's working limit
	limitW         float64 // below the hour's import limit; negative is an overage
	riseW          float64 // below the level an increase may reach, less what we sent
	startLevelW    float64 // import an idle start may reach: the rise level, at most the ceiling
	hourly         bool    // the hour's import is measured, so a short overage may wait
	measured       bool    // false when the meter reports phase currents that cannot be read
	worstA         float64
	phaseRoomA     float64 // below the working limit on the busiest phase, read or heat
	phaseRoomKnown bool    // the meter reports phase currents
}

func (s *State) evMeterRoom(meter *telemetry.DerReading, gridW, phases, fuseMaxW float64, sent pendingLoad) evMeterRoom {
	inf, totalW := math.Inf(1), math.Max(0, fuseMaxW-s.fuseSafetyMarginW())-gridW
	r := evMeterRoom{fuseW: totalW - sent.w, fuseOverW: math.Max(0, -totalW), overLimit: totalW < 0, limitW: inf, riseW: inf, startLevelW: inf}
	if limitW, riseW, ok := s.importHourLimitsW(); ok {
		r.limitW, r.riseW = limitW-gridW, riseW-gridW-sent.w
		// A car that could run only on one hour's slack would stop when
		// the next hour starts at the ceiling.
		r.startLevelW = math.Min(riseW, s.PeakImportCeilingW)
		r.hourly = s.importHourMeasured()
	}
	amps, present, valid := phaseCurrentsA(meter, s)
	if present && !valid {
		r.phaseRoomKnown = true // No room on phases it cannot read.
		return r
	}
	r.measured = true
	if present {
		limitA := s.SiteFuseAmps - evPhaseMarginA
		for _, a := range amps {
			r.worstA = math.Max(r.worstA, a)
		}
		r.fuseW = math.Min(r.fuseW, (limitA-s.fuseHeat.levelA(amps, sent.phaseA))*s.SiteFuseVoltage*phases)
		r.phaseRoomA, r.phaseRoomKnown = limitA-s.fuseHeat.levelA(amps, [3]float64{}), true
		// The phases measure the fuse itself; total import stands in only without them.
		r.fuseOverA = s.fuseHeat.overA(amps, limitA, fuseInstantFactor*s.SiteFuseAmps)
		r.fuseOverW, r.perPhase, r.overLimit = r.fuseOverA*s.SiteFuseVoltage*phases, true, r.worstA > limitA
	}
	return r
}

// worstPhaseA reads the site's configured phases. present is false when the
// meter reports no phase currents; valid is false when they cannot be used.
func worstPhaseA(meter *telemetry.DerReading, state *State) (worstA float64, present, valid bool) {
	amps, present, valid := phaseCurrentsA(meter, state)
	for _, a := range amps {
		worstA = math.Max(worstA, a)
	}
	return worstA, present, valid
}

// withBatteryOrders adds battery increase we ordered that the meter may not
// show yet, w, to the chargers' unshown offers: in total import, and as a
// three-phase battery puts it, the same current on each phase. A one-phase
// pool's room is one phase's, so the battery's whole power there would count
// it three times.
func (s *State) withBatteryOrders(p pendingLoad, w float64) pendingLoad {
	p.w += w
	if s.SiteFuseVoltage > 0 && s.SiteFusePhases > 0 {
		for i := range p.phaseA {
			p.phaseA[i] += w / (s.SiteFuseVoltage * float64(s.SiteFusePhases))
		}
	}
	return p
}

// pendingBatteryCommandsW is battery increase we ordered that the meter may
// not show yet. Signed, so a stopped discharge counts too.
func (s *State) pendingBatteryCommandsW(now time.Time) float64 {
	var pending float64
	for _, log := range s.batteryCommands {
		if low, _, latest, ok := log.span(now); ok {
			pending += math.Max(0, latest-low)
		}
	}
	return pending
}

// SetEVCommanded stores this tick's final charger offers, after every
// charger clamp, in total and per driver.
func (s *State) SetEVCommanded(points []loadpoint.State) {
	s.EVCommandedW = loadpoint.CommandedEVPowerW(points)
	s.evCommanded = loadpoint.CommandedEVPowerByDriver(points)
}

// pendingEVCutW is the EV reduction we sent during the current run over the
// hour's import limit: for each charger still plugged, its offer when the
// run began less its offer this tick. A car that leaves drops out, since no
// cut of ours removed it; a car that joins has no start offer. Our own
// commands only. A cut that has reached the meter ends the run, and no run
// waits longer than evSettle, so a car that draws less than its offer cannot
// hold an overage through a second cut.
func (s *State) pendingEVCutW() float64 {
	lim := &s.evLimit
	if lim.overSince.IsZero() || s.now().Sub(lim.overSince) >= evSettle {
		return 0
	}
	var cutW float64
	for driver, startW := range lim.overOffers {
		if nowW, plugged := s.evCommanded[driver]; plugged {
			cutW += math.Max(0, startW-math.Max(0, nowW))
		}
	}
	return cutW
}

// evPoolFor sums the plugged managed chargers. Before the first command after
// a restart their offer is unknown, and the pool counts as idle.
func evPoolFor(state *State, points []loadpoint.State, configs []loadpoint.Config) (evPool, bool) {
	byID := make(map[string]loadpoint.Config, len(configs))
	for _, cfg := range configs {
		byID[cfg.ID] = cfg
	}
	pool := evPool{phases: 3, wantW: math.Inf(1), offerKnown: true}
	plugged := false
	for _, point := range points {
		if !point.PluggedIn {
			continue
		}
		plugged = true
		cfg := byID[point.ID]
		// Empty is the declared three-phase default. The controller's own
		// gate covers runtime auto/manual phase changes.
		if cfg.PhaseMode != "" && cfg.PhaseMode != "3p" {
			pool.phases = 1
		}
		if cfg.MinChargeW > 0 && (pool.minW == 0 || cfg.MinChargeW < pool.minW) {
			pool.minW = cfg.MinChargeW
		}
		pool.offerKnown = pool.offerKnown && point.CommandedKnown
	}
	if !plugged {
		return pool, false
	}
	pool.offers = loadpoint.CommandedEVPowerByDriver(points)
	if pool.offerKnown {
		pool.offerW = loadpoint.CommandedEVPowerW(points)
	}
	if state.evFuseDemandsSet {
		pool.wantW = 0
		for _, demand := range state.evFuseDemands {
			pool.wantW += demand.powerW
		}
	}
	if state.SiteFusePhases >= 1 && float64(state.SiteFusePhases) < pool.phases {
		pool.phases = float64(state.SiteFusePhases)
	}
	return pool, true
}

func qualifiedFuseObservation(r *telemetry.DerReading, now time.Time) (telemetry.ControlObservation, bool) {
	if r == nil || !finiteFuseValue(r.RawW) || r.UpdatedAt.After(now) || now.Sub(r.UpdatedAt) > telemetry.ControlPowerMaxAge(r.Data) {
		return telemetry.ControlObservation{}, false
	}
	p, ok := telemetry.ControlPowerObservation(r.RawW, r.Data, r.UpdatedAt)
	return p, ok && !p.Seen().After(now) && now.Sub(p.Seen()) <= telemetry.ControlWindowDuration
}

func publishBatterylessEVFuseBudget(store *telemetry.Store, state *State, fuseMaxW float64) {
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	state.FuseEVMaxW, state.FuseSaturated, state.FuseSaturatedByImport = 0, false, false
	evInMeterW := state.evInMeterW()
	demand := math.Max(evInMeterW, scheduledEVPowerW(state))
	if meter == nil || demand <= 0 {
		return
	}
	ceilingW := state.phaseImportCeilingW(fuseMaxW, meter)
	cap := evHeadroomW(ceilingW, math.Max(meter.RawW, meter.SmoothedW), evInMeterW)
	state.FuseEVMaxW = math.Min(demand, cap)
	state.FuseSaturated = demand > cap
	state.FuseSaturatedByImport = state.FuseSaturated && state.importLimitSets(ceilingW)
}
