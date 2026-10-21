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

// evMinPhaseA is the least current a car charges at on each phase
// (IEC 61851).
const evMinPhaseA = 6.0

// evPhaseMarginA is the working margin below the breaker on each phase. One
// amp on the pool's phases is also the rise step and the gap between the cut
// and rise thresholds.
const evPhaseMarginA = 1.0

// sentCommand is one of our own commands, never a device reading.
type sentCommand struct {
	at    time.Time
	w     float64
	a     float64 // EV offers: current on each phase it may land on
	three bool    // EV offers: on all three phases
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
// settle before the reading, less the lowest offer the reading may show.
// An offer set before that window is in the reading. A cut needs nothing:
// the reading shows the draw until the cut lands. Before our first offer
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
		highW, highA = math.Max(highW, c.w), math.Max(highA, c.a)
		if !c.at.After(at) {
			lowW, lowA = math.Min(lowW, c.w), math.Min(lowA, c.a)
		}
	}
	lowW, lowA = math.Min(lowW, settledW), math.Min(lowA, settledA)
	return math.Max(0, highW-lowW), math.Max(0, highA-lowA)
}

// shownLow is the lowest offer a reading taken at `at` may show: the one in
// force settle before it, or one sent since. Before our first offer the
// charger counts as 0.
func (l commandLog) shownLow(at time.Time, settle time.Duration) float64 {
	from := at.Add(-settle)
	low, settled := math.Inf(1), 0.0
	for _, c := range l {
		if !c.at.After(from) {
			settled = c.w
			continue
		}
		if !c.at.After(at) {
			low = math.Min(low, c.w)
		}
	}
	return math.Min(low, settled)
}

// unshownCut is how much less a charger may draw than a reading taken at
// `at` shows once latestW lands: the highest offer in force since a run over
// a limit began, less latestW. three is false when any of those offers may
// sit on one phase.
func (l commandLog) unshownCut(since, at time.Time, latestW float64) (w float64, three bool) {
	high, three := 0.0, true
	for _, c := range l {
		if c.at.After(at) {
			break
		}
		if c.at.Before(since) {
			high, three = c.w, c.three // In force when the run began.
			continue
		}
		high, three = math.Max(high, c.w), three && c.three
	}
	return math.Max(0, high-latestW), three
}

// chargerOffers is one charger's offers, kept for the time its driver
// declares its power takes to show.
type chargerOffers struct {
	driver string
	log    commandLog
	settle time.Duration
}

// pendingLoad is load we ordered that a site reading may not show yet:
// each EV offer sent above what the reading may show, until its charger
// can have applied it. A single-phase offer may land on any phase. Rises
// and starts count it on top of the meter; a cut never waits for it.
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
		c.driver, c.settle = point.DriverName, evUnknownSettle
		if r := store.Get(point.DriverName, telemetry.DerEV); r != nil {
			if declared := telemetry.DeclaredPowerMaxAge(r.Data); declared > 0 {
				c.settle = max(evSettle, declared)
			}
		}
		w := math.Max(0, point.CommandedW)
		sent := sentCommand{at: now, w: w, a: s.offerPhaseA(w, point.CommandedPhases), three: point.CommandedPhases == 3 && s.SiteFusePhases == 3}
		if n := len(c.log); n == 0 || c.log[n-1].w != sent.w || c.log[n-1].a != sent.a {
			c.log = c.log.keep(sent, read.Add(-c.settle))
		}
		offers[point.ID] = c
	}
	s.evLimit.offers = offers
}

// offerPhaseA is the current an offer of w puts on each phase it may land
// on. Unknown phases count as one.
func (s *State) offerPhaseA(w float64, phases int) float64 {
	if s.SiteFuseVoltage <= 0 {
		return 0
	}
	n := 1
	if phases == 3 && s.SiteFusePhases > 1 {
		n = s.SiteFusePhases
	}
	return w / (s.SiteFuseVoltage * float64(n))
}

// EVFusePhaseRoomA is the current the chargers may still add on the
// busiest phase: fusePhases.roomA at the last site reading, less every
// charger's offers that reading may not show. false before a reading.
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
	phaseRoomKnown bool                     // a fuse is configured and the reading has power
	overSince      time.Time                // first reading of a run over the hour's import limit
	fuseOverSince  time.Time                // first reading of a run with a phase over the fuse
	idleWh         float64                  // import since roomSince, for the ceiling's start test
	byImport       bool                     // the hour's import limit, not the fuse, left less room at readingAt
}

type evPool struct {
	offerW, wantW, minW, phases float64
	offerKnown                  bool
}

// EVFuseBudget returns the managed EVs' common hard cap, which the loadpoint
// controller divides between cars. Callers hold the control lock, and the
// shared freshness gate must already allow dispatch. The fuse is
// fusePhases: a phase overage cuts once that phase's heat budget is spent,
// or at once above fuseInstantFactor. Otherwise the cap holds; it rises one
// step on a fresh reading with room for two, at least evSettle after the
// last cut. Room counts every offer sent above what the reading may show as
// flowing until its charger can have applied it; cuts read the meter alone.
// EVFusePhaseRoomA gives the same room per phase to the split between cars.
// An idle pool starts once the meter has kept showing room for its minimum
// for evRestartDwell. Each reading acts once, so a silent meter can never
// raise or restart a charge.
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
	// Only a reading after a stale meter starts afresh; a slow meter's
	// cadence changes nothing else.
	gap := first || at.Sub(prevAt) > telemetry.ControlPowerMaxAge(meter.Data)
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
	r := s.evMeterRoom(meter, gridW, pool.phases, s.pendingLoad(at))
	_, fresh := qualifiedFuseObservation(meter, now)
	fresh = fresh && r.measured
	lim.phaseRoomA, lim.phaseRoomKnown = 0, r.phaseRoomKnown
	if fresh {
		lim.phaseRoomA = r.phaseRoomA
	}
	step := evPhaseMarginA * s.SiteFuseVoltage * pool.phases
	// The label follows whichever limit leaves less room.
	lim.byImport = math.Min(r.limitW, r.riseW) < r.fuseW
	// The fuse cuts on every reading, by each phase's heat budget. The
	// hour's budget absorbs a short overage of
	// less than a step: it cuts once the overage has lasted evSettle, so one
	// reading over the ceiling changes nothing.
	cutW := r.fuseCutW
	switch {
	case cutW >= 0:
		lim.fuseOverSince = time.Time{}
	case lim.fuseOverSince.IsZero() || gap:
		lim.fuseOverSince = at
	}
	if r.limitW >= 0 {
		lim.overSince = time.Time{}
	} else {
		if lim.overSince.IsZero() || gap {
			lim.overSince = at
		}
		if !r.hourly || gap || -r.limitW >= step || at.Sub(lim.overSince) >= evSettle {
			cutW = math.Min(cutW, r.limitW)
		}
	}
	if cutW < 0 {
		lim.byImport = cutW < r.fuseCutW
		// The meter may still show any offer of the last evSettle. Taking
		// the overage from the highest cuts each measured watt once.
		lim.capW, lim.roomSince = math.Min(prev, math.Max(0, peakW+cutW)), time.Time{}
		if lim.capW < prev {
			lim.cutAt = now
			slog.Info("EV hard limit: site meter over limit", "grid_w", gridW, "worst_phase_a", r.worstA,
				"fuse_heat_a", s.fuseHeat.worstA(), "import_limit_w", s.effectiveImportCeilingW(fuseMaxW),
				"overage_w", -cutW, "offer_w", peakW, "cap_w", lim.capW)
		}
		return
	}
	// An increase needs a fresh reading and room below the level increases
	// may reach, left after our own battery commands that the meter may not
	// show yet. Between that level and the import limit the cap holds.
	pending := s.pendingBatteryCommandsW(now)
	room := 0.0
	if fresh {
		room = math.Max(0, math.Min(r.fuseW, r.riseW)-pending)
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
// for evRestartDwell: room on every reading for its minimum below the fuse,
// and for its minimum plus a step below an unmeasured hour's ceiling. It
// starts a step below the room where the minimum still fits. Under a
// measured hour, the ceiling's part is judged on the dwell's mean import,
// the house's load rather than one reading of it: a short overage waits.
func (s *State) stepEVIdleStart(r evMeterRoom, gridW float64, pool evPool, first, gap, fresh bool, at, prevAt time.Time, pending, step float64) {
	lim := &s.evLimit
	lim.capW = 0
	fuseRoom, ceilingRoom := 0.0, math.Inf(1)
	if r.hourly {
		// The start test below holds an idle car to the ceiling itself,
		// whatever slack the hour has: name it by this reading.
		lim.byImport = r.startLevelW-gridW < r.fuseW
	}
	if fresh {
		fuseRoom = r.fuseW - pending
		if !r.hourly {
			ceilingRoom = r.riseW - pending - step
		}
	}
	if fuseRoom < pool.minW || ceilingRoom < pool.minW {
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
	lim.capW = math.Min(math.Max(pool.minW, fuseRoom-step), ceilingRoom)
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
	fuseW          float64 // fusePhases.roomA after what we sent; rises and starts
	fuseCutW       float64 // what the fuse needs cut now, negative; see fusePhases
	limitW         float64 // below the hour's import limit; negative is an overage
	riseW          float64 // below the level an increase may reach, less what we sent
	startLevelW    float64 // import an idle start may reach: the rise level, at most the ceiling
	hourly         bool    // the hour's import is measured, so a short overage may wait
	measured       bool    // false when the meter's phase currents cannot be read
	worstA         float64
	phaseRoomA     float64 // fusePhases.roomA
	phaseRoomKnown bool
}

func (s *State) evMeterRoom(meter *telemetry.DerReading, gridW, phases float64, sent pendingLoad) evMeterRoom {
	inf := math.Inf(1)
	r := evMeterRoom{limitW: inf, riseW: inf, startLevelW: inf}
	if limitW, riseW, ok := s.importHourLimitsW(); ok {
		r.limitW, r.riseW = limitW-gridW, riseW-gridW-sent.w
		// A car that could run only on one hour's slack would stop when
		// the next hour starts at the ceiling.
		r.startLevelW = math.Min(riseW, s.PeakImportCeilingW)
		r.hourly = s.importHourMeasured()
	}
	f, ok := s.fusePhasesAt(meter)
	if !ok {
		return r // No room and no cut: the fuse is not described.
	}
	r.measured, r.phaseRoomKnown, r.worstA = !f.blind, true, f.highestA()
	r.fuseW = f.watts(f.roomA(sent.phaseA)) / float64(f.n) * phases
	r.phaseRoomA = f.roomA([3]float64{})
	r.fuseCutW = -f.watts(f.importOverA([3]float64{})) / float64(f.n) * phases
	return r
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
// charger clamp, in total and per driver, and logs each plugged charger's
// offer at the time it was sent.
func (s *State) SetEVCommanded(points []loadpoint.State) {
	s.EVCommandedW = loadpoint.CommandedEVPowerW(points)
	s.evCommanded = loadpoint.CommandedEVPowerByDriver(points)
	now := s.now()
	for _, point := range points {
		if !point.PluggedIn || !point.CommandedKnown || !finiteFuseValue(point.CommandedW) {
			continue
		}
		c := s.evLimit.offers[point.ID]
		c.driver = point.DriverName
		if c.settle == 0 {
			c.settle = evUnknownSettle
		}
		w := math.Max(0, point.CommandedW)
		sent := sentCommand{at: now, w: w, a: s.offerPhaseA(w, point.CommandedPhases), three: point.CommandedPhases == 3 && s.SiteFusePhases == 3}
		if n := len(c.log); n == 0 || c.log[n-1].w != sent.w || c.log[n-1].a != sent.a {
			c.log = c.log.keep(sent, now.Add(-c.settle))
		}
		if s.evLimit.offers == nil {
			s.evLimit.offers = map[string]chargerOffers{}
		}
		s.evLimit.offers[point.ID] = c
	}
}

// evPoolFor sums the plugged managed chargers. A charger's offer is unknown
// before its first command after a restart or a plug-in; it adds nothing,
// and the pool counts as idle only while no offer is known.
func evPoolFor(state *State, points []loadpoint.State, configs []loadpoint.Config) (evPool, bool) {
	byID := make(map[string]loadpoint.Config, len(configs))
	for _, cfg := range configs {
		byID[cfg.ID] = cfg
	}
	pool := evPool{phases: 3, wantW: math.Inf(1)}
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
		minW := cfg.MinChargeW
		if cfg.PhaseMode == "3p" && state.SiteFuseVoltage > 0 {
			minW = math.Max(minW, evMinPhaseA*state.SiteFuseVoltage*3)
		}
		if minW > 0 && (pool.minW == 0 || minW < pool.minW) {
			pool.minW = minW
		}
		pool.offerKnown = pool.offerKnown || point.CommandedKnown
	}
	if !plugged {
		return pool, false
	}
	pool.offerW = loadpoint.CommandedEVPowerW(points)
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

// publishBatterylessEVFuseBudget marks a site without batteries saturated
// when the EVs want more than the site's room.
func publishBatterylessEVFuseBudget(store *telemetry.Store, state *State, fuseMaxW float64) {
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	state.FuseSaturated, state.FuseSaturatedByImport = false, false
	evInMeterW := state.evInMeterW()
	demand := math.Max(evInMeterW, scheduledEVPowerW(state))
	if meter == nil || demand <= 0 {
		return
	}
	gridW := math.Max(meter.RawW, meter.SmoothedW)
	ceilingW, byImport := state.siteImportCeilingW(meter, gridW)
	if math.IsInf(ceilingW, 1) {
		return
	}
	state.FuseSaturated = demand > evHeadroomW(ceilingW, gridW, evInMeterW)
	state.FuseSaturatedByImport = state.FuseSaturated && byImport
}
