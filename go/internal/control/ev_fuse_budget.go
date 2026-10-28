package control

import (
	"log/slog"
	"math"
	"slices"
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

// evStepA is the EV rise step: one amp on each of the pool's phases. It is
// the size of the next move, never current held back below the fuse's
// working limit (fuseLineA).
const evStepA = 1.0

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
	// declared is true when the driver declares that time; settle is then
	// not evUnknownSettle's guess (evShownW).
	declared bool
	// found is the offer in force before the limit's first site reading,
	// after a restart: Core did not send it in this run. A rise counts the
	// charger as 0 before it; a cut takes it as shown, since the reading is
	// all Core knows of it.
	found sentCommand
	// driver is the charger's driver, whose power reading only the battery
	// side reads, and only before any offer of this run has surely landed
	// (evFoundDrawW). maxW is the loadpoint's max_charge_w; zero when not
	// configured.
	driver string
	maxW   float64
	// draws are the charger's samples while that window is open (noteDraw),
	// and watchFrom the receipt of the first reading noteDraw took there:
	// a sample already there then may have arrived at any earlier time.
	draws     []evDraw
	watchFrom time.Time
	// carPhases is how many phases the car draws on, as the loadpoint saw
	// it on the site meter: 1 or 3, or 0 when unknown. carPhase is the phase
	// a car on one phase draws on, 1-3.
	carPhases, carPhase int
	// shownA is the current on each phase of the charger's offers that the
	// site meter shows, as far as it can tell (seeOffers); unsure while it
	// cannot.
	shownA [3]float64
	unsure bool
	// riseBaseA is the site meter's phase currents when a raise went out
	// with no other raise still to show, and riseFromA the most of the
	// charger's offers that reading may have shown. riseAheadA measures the
	// rise still to come against them; riseSet is false while no raise is
	// on its way.
	riseBaseA [3]float64
	riseFromA float64
	riseSet   bool
}

// evMoveA is half a charger step. An order landing shows on the site meter
// as every phase moving the same way, by evMoveA or more on their mean and
// no more than a step apart, or as one phase alone moving a whole step
// while the others hold within evMoveA (a car on one phase). Meter noise
// rarely moves that far.
const evMoveA = evStepA / 2

// chargerMoveA is the move per phase from one reading to the next when it
// has the shape of an order landing (evMoveA), signed: alikeA when every
// phase moved alike, oneA and its phase when one phase moved alone.
func chargerMoveA(prev, now []float64) (alikeA, oneA float64, phase int) {
	var sum, held, moved, d1 float64
	lo, hi, p1 := math.Inf(1), math.Inf(-1), 0
	for i := range now {
		d := now[i] - prev[i]
		sum, lo, hi = sum+d, math.Min(lo, d), math.Max(hi, d)
		switch {
		case math.Abs(d) >= 2*evMoveA:
			moved, d1, p1 = moved+1, d, i
		case math.Abs(d) < evMoveA:
			held++
		}
	}
	n := float64(len(now))
	switch {
	case hi-lo <= 2*evMoveA && (lo > 0 && sum/n >= evMoveA || hi < 0 && sum/n <= -evMoveA):
		return sum / n, 0, 0
	case moved == 1 && held == n-1:
		return 0, d1, p1 + 1
	}
	return 0, 0, 0
}

// seeOffers reads a new site reading against the last one. With one charger
// in the pool and no change of phases in its log, a move shaped like an
// order landing (chargerMoveA) moves the current the meter shows of the
// charger on each phase (shownA), in a shape the offers can take there. A
// car the loadpoint has seen draw on one phase, or a one-phase order, is
// one level on every phase: every phase dropping alike moves it, and so
// does a move on the car's phase alone where that phase is known. A move on
// one phase while the car's phase is not known may be a house load, so it
// does not count. A three-phase order moves only on every phase alike:
// a phase that moves alone is a house load, or a car not yet seen that
// draws on one phase, and Core cannot tell which. The shown current moves
// only toward the latest offer, and stays within the offers the reading
// may show. While our own battery order moves, a move of every phase alike
// may be the battery's, so no reading counts; after a reading that did not
// count, the shown current is known again once only one offer may show. A
// house load that moves in a charger's shape reads as ours. It can only
// take cover off a cut. The rise a raise has still to show reads the meter
// differently (riseAheadA).
func (s *State) seeOffers(meter *telemetry.DerReading, gridW float64) {
	lim := &s.evLimit
	amps, _, valid := phaseCurrentsA(meter, s, gridW)
	var now [3]float64
	copy(now[:], amps)
	prev, read := lim.phasesA, valid && lim.phasesSeen && len(lim.offers) == 1 && len(amps) >= 2 && !s.batteryOrderMoves()
	lim.phasesA, lim.phasesSeen = now, valid
	var alikeA, oneA float64
	var phase int
	if read {
		alikeA, oneA, phase = chargerMoveA(prev[:len(amps)], now[:len(amps)])
	}
	for id, c := range lim.offers {
		if read && c.readable() {
			var moveA [3]float64
			switch {
			case c.carPhase > 0 || c.log[0].n == 1:
				// One car on one phase: one level, moved by a drop of
				// every phase alike or by its known phase alone.
				m := math.Min(0, alikeA)
				if c.carPhase == phase {
					m += oneA
				}
				moveA = [3]float64{m, m, m}
			default:
				// A three-phase order moves every phase alike. Read as the
				// car, a load cycling on the phase over the line took the
				// cut's cover off there on each drop and never gave it
				// back, and Core cut again and again.
				moveA = [3]float64{alikeA, alikeA, alikeA}
			}
			// The charger moves toward its latest offer, never past it.
			latestA := c.log[len(c.log)-1].a
			for i, m := range moveA {
				if m < 0 && c.shownA[i] > latestA {
					c.shownA[i] = math.Max(latestA, c.shownA[i]+m)
				} else if m > 0 && c.shownA[i] < latestA {
					c.shownA[i] = math.Min(latestA, c.shownA[i]+m)
				}
			}
		} else {
			c.unsure = true
		}
		low, high := c.offerRangeA(lim.readingAt)
		for i := range c.shownA {
			c.shownA[i] = math.Min(high, math.Max(low, c.shownA[i]))
		}
		c.unsure = c.unsure && low < high
		if ahead := s.riseAheadA(c); low >= high || ahead == [3]float64{} {
			c.riseSet = false
		}
		lim.offers[id] = c
	}
}

// batteryOrderMoves reports whether one of our battery orders of the last
// evSettle moves the site's phases by evMoveA or more.
func (s *State) batteryOrderMoves() bool {
	for _, log := range s.batteryCommands {
		if low, high, _, ok := log.span(s.now()); ok && high-low >= evMoveA*s.SiteFuseVoltage*float64(s.SiteFusePhases) {
			return true
		}
	}
	return false
}

// readable reports whether the site meter can tell what it shows of the
// charger's offers: every offer the log holds lands on the same phases.
func (c chargerOffers) readable() bool {
	for _, o := range c.log {
		if o.n != c.log[0].n {
			return false
		}
	}
	return len(c.log) > 0
}

// offerRangeA is the lowest and highest current per phase among the
// offers a reading taken at `at` may show: the one in force settle before
// it and each one sent since. Before the first logged offer the charger
// counts as before.
func (c chargerOffers) offerRangeA(at time.Time) (low, high float64) {
	from := at.Add(-c.settle)
	low, high = c.found.a, c.found.a
	for _, o := range c.log {
		switch {
		case o.at.After(at):
		case !o.at.After(from):
			low, high = o.a, o.a
		default:
			low, high = math.Min(low, o.a), math.Max(high, o.a)
		}
	}
	return low, high
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

// evShownW is the EV load a reading taken at `at` surely shows: each
// plugged charger's latest offer less what it may not show yet. The battery
// side reads this. A raise stays unshown for the response time the
// charger's driver declares, so the battery waits for a slow charger's raise
// as long as the EV limit does. A charger that declares none gets evSettle,
// not evUnknownSettle: the battery would otherwise reserve a local charger's
// raise on top of the reading that already shows it, and stop charging for
// three minutes after each one. Easee Cloud, the slow charger we know,
// declares its 180 s.
func (s *State) evShownW(at time.Time) float64 {
	var w float64
	for _, c := range s.evLimit.offers {
		if n := len(c.log); n > 0 {
			unshown, _ := c.log.unshown(at, c.batterySettle())
			w += math.Max(0, c.log[n-1].w-unshown)
		}
	}
	return w
}

// evFoundDrawW is EV draw the battery side may count as shown beyond
// evShownW (shownW) while a charger has no order of this run that has
// surely landed: from the first offer Core logs for it with no fuse limit
// set (at Core's start, or at a plug-in once no other managed charger was
// plugged), until that offer is older than the charger's settle time. Core
// then does not know which offer the charger runs on, and its first offers
// (often 0 A) say nothing of what the meter shows.
//
// A car seen drawing in that window is in the site reading when the
// charger sampled that draw before the reading, and no sample since reads
// less. The charger stamps its samples with its own clock (OCPP
// MeterValues), which may run behind Core's by any amount, so Core judges
// a sample by when it first arrived on Core's clock (noteDraw): a sample
// arrives after it was measured. It counts as before the reading only
// when Core saw it arrive evMeterDelay before the reading did (arrivedBy),
// and no more than one control interval before that: a fast charger may
// have applied a 0 A offer, or the car paused, since an older sample. The
// draw is the lowest of that sample and every sample since (noteDraw, each
// the lowest of the sample, raw and smoothed), up to the charger's current
// reading. So a rise sampled after the reading never counts, whatever the
// charger's clock. A held reading after a failed poll says nothing of now.
// A charger that samples less often (OCPP MeterValues every 10 s or more) counts
// only on the ticks from evMeterDelay to one control interval more after a
// sample arrives, and holds its battery's discharge between them.
//
// Until the latest offer lands, the car may still rise from that draw to
// the offer in force. Core does not know that offer: one from before a
// restart, under an older max_charge_w, or the charger's own default. The
// reserve takes it as the larger of the loadpoint's max_charge_w and the
// site fuse's rating, and keeps the rise past the latest offer; a charger
// with no max_charge_w counts nothing. Each charger counts no more than its
// latest offer, and in all no more than the consumption the site meter
// shows (siteConsumptionW) less shownW. The car's own limit never reads
// this; only how much of our latest offers the battery reserves as a rise
// to come.
// gridW is the site reading, which arrived at meterAt.
func (s *State) evFoundDrawW(store *telemetry.Store, gridW float64, meterAt, at time.Time, shownW float64) float64 {
	if store == nil {
		return 0
	}
	consumptionW := siteConsumptionW(store, gridW)
	if !finiteFuseValue(consumptionW) {
		return 0
	}
	skew := time.Duration(s.ControlIntervalS) * time.Second
	if skew <= 0 {
		skew = 2 * time.Second // config's default control_interval_s
	}
	edge := meterAt.Add(-evMeterDelay)
	from := edge.Add(-skew)
	var w float64
	for _, c := range s.evLimit.offers {
		n := len(c.log)
		if n == 0 || !c.foundOpen(at) || c.driver == "" || !(c.maxW > 0) || !finiteFuseValue(c.maxW) {
			continue
		}
		now, ok := readEVDraw(store, c.driver, at)
		if !ok {
			continue
		}
		for _, d := range c.draws {
			if d.at.Equal(now.at) {
				d.merge(now)
				now = d
			}
		}
		if now.seen.Before(from) {
			continue
		}
		drawW, before := now.w, c.arrivedBy(now, edge)
		for _, d := range c.draws {
			if d.seen.Before(from) {
				continue
			}
			drawW, before = math.Min(drawW, d.w), before || c.arrivedBy(d, edge)
		}
		if !before {
			continue
		}
		unshown, _ := c.log.unshown(at, c.batterySettle())
		latest := c.log[n-1].w
		capW := math.Max(c.maxW, s.siteFuseMaxW())
		inLatestW := math.Min(latest, drawW-math.Max(0, capW-latest))
		w += math.Max(0, inLatestW-math.Max(0, latest-unshown))
	}
	return math.Max(0, math.Min(w, consumptionW-shownW))
}

// evMeterDelay is how much earlier than the site reading a charger's
// sample must arrive to count as in that reading (evFoundDrawW): the site
// meter's own delay from measurement to arrival.
const evMeterDelay = time.Second

// evDraw is one charger sample: its stamp on the charger's own clock,
// which only tells samples apart; when it first arrived on Core's clock;
// when the charger last vouched for it, on Core's clock (its arrival, or a
// later confirmed reading); and the lowest of the sample, raw and smoothed.
type evDraw struct {
	at, got, seen time.Time
	confirmed     bool
	w             float64
}

// merge folds a repeat of the same sample into e: the lowest draw, the
// first arrival, and a later receipt only where the charger confirms it.
func (e *evDraw) merge(d evDraw) {
	e.w = math.Min(e.w, d.w)
	if d.got.Before(e.got) {
		e.got = d.got
	}
	if d.confirmed && d.seen.After(e.seen) {
		e.seen, e.confirmed = d.seen, true
	}
}

// readEVDraw is the charger's current sample, while its telemetry is live,
// its last poll succeeded and the sample passes qualifiedFuseObservation,
// taken as arriving with the charger's current reading.
func readEVDraw(store *telemetry.Store, driver string, at time.Time) (evDraw, bool) {
	lowerW, live := store.OnlineLowerW(driver, telemetry.DerEV)
	if !live {
		return evDraw{}, false
	}
	r := store.Get(driver, telemetry.DerEV)
	p, ok := qualifiedFuseObservation(r, at)
	if !ok {
		return evDraw{}, false
	}
	w := math.Min(p.PowerW, lowerW)
	return evDraw{at: p.At, got: r.UpdatedAt, seen: r.UpdatedAt, confirmed: !p.ConfirmedAt.IsZero(), w: w}, finiteFuseValue(w)
}

// arrivedBy reports whether Core saw sample d arrive by edge. A sample
// already there when Core began to watch the charger arrived at a time
// Core does not know, so it never counts as arrived.
func (c chargerOffers) arrivedBy(d evDraw, edge time.Time) bool {
	return !c.watchFrom.IsZero() && d.got.After(c.watchFrom) && !d.got.After(edge)
}

// foundOpen is true while the charger has no order of this run that has
// surely landed (evFoundDrawW).
func (c chargerOffers) foundOpen(at time.Time) bool {
	return !c.found.at.IsZero() && at.Sub(c.found.at) < c.settle
}

// noteDraw keeps the charger's samples while foundOpen, for evFoundDrawW,
// over telemetry.ControlWindowDuration, each with its first arrival.
func (c *chargerOffers) noteDraw(store *telemetry.Store, at time.Time) {
	if !c.foundOpen(at) || c.driver == "" {
		c.draws, c.watchFrom = nil, time.Time{}
		return
	}
	d, ok := readEVDraw(store, c.driver, at)
	if !ok {
		return
	}
	if c.watchFrom.IsZero() {
		c.watchFrom = d.got
	}
	if n := len(c.draws); n > 0 && !d.at.After(c.draws[n-1].at) {
		if last := &c.draws[n-1]; d.at.Equal(last.at) {
			last.merge(d)
		}
		return
	}
	c.draws = append(slices.DeleteFunc(c.draws, func(e evDraw) bool {
		return at.Sub(e.seen) > telemetry.ControlWindowDuration
	}), d)
}

// siteConsumptionW is the consumption a site reading of gridW shows: grid
// less each live battery, PV and V2X reading, each at the higher of raw and
// smoothed. A smoothed value lags an output that just fell; the higher one
// keeps the estimate at or below what the meter shows.
func siteConsumptionW(store *telemetry.Store, gridW float64) float64 {
	return gridW - store.SumOnlineHigherW(telemetry.DerBattery, telemetry.DerPV, telemetry.DerV2X)
}

// batterySettle is how long the battery side counts a raise of this
// charger's as unshown: its declared response time, else evSettle
// (evShownW).
func (c chargerOffers) batterySettle() time.Duration {
	if c.declared {
		return c.settle
	}
	return evSettle
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
//
// Where the meter tells what it shows of the charger (seeOffers), a cut
// covers no more than the meter has still to show on each phase: a cut the
// meter shows covers no overage that comes after it. aheadA is then the
// rise on each phase the meter has still to show of the latest offers
// (riseAheadA), which a raise's own trigger reads (stepEVMeterLimit), and
// rise counts at least that much. Elsewhere aheadA is 0.
func (s *State) unshownOrders(at time.Time, perPhase bool) (rise, cover pendingLoad, aheadA [3]float64, latestW float64) {
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
		riseA := math.Max(0, a)
		phaseCoverA, phaseRiseA, ahead := [3]float64{coverA, coverA, coverA}, [3]float64{riseA, riseA, riseA}, [3]float64{}
		if perPhase && lim.phasesSeen && len(lim.offers) == 1 && s.SiteFusePhases >= 2 && c.readable() && !c.unsure {
			low, high := c.offerRangeA(at)
			ahead = s.riseAheadA(c)
			for i := range min(s.SiteFusePhases, len(ahead)) {
				toComeA := latest.a - math.Min(high, math.Max(low, c.shownA[i]))
				phaseCoverA[i] = math.Min(coverA, math.Max(0, -toComeA))
				phaseRiseA[i] = math.Max(riseA, ahead[i])
			}
		}
		rise.w += math.Max(0, w)
		cover.w += math.Max(0, math.Min(-w, -runW))
		for i := range rise.phaseA {
			rise.phaseA[i] += phaseRiseA[i]
			cover.phaseA[i] += phaseCoverA[i]
			aheadA[i] += ahead[i]
		}
		latestW += latest.w
	}
	return rise, cover, aheadA, latestW
}

// riseAheadA is the rise on each phase the site meter has still to show
// of the charger's latest offer: the raise since riseFromA less the meter's
// own rise since the raise went out, within the offers the reading may
// show. A car the loadpoint has seen draw on three phases takes the
// smallest rise across the phases: a house step on some phases does not
// show the raise on the rest. A car on one phase takes its own phase's
// rise. Any other car takes the largest rise: until the loadpoint has seen
// it, it may draw on one phase Core does not know. A landing counts in
// whatever shape it comes, a slow ramp or beside a house load that moves
// in the same reading. A house load that drops while the raise travels
// still reads as raise not shown yet, by as much as it dropped.
func (s *State) riseAheadA(c chargerOffers) (ahead [3]float64) {
	lim := &s.evLimit
	n := min(s.SiteFusePhases, len(ahead))
	if !c.riseSet || !lim.phasesSeen || n < 2 || len(c.log) == 0 {
		return ahead
	}
	latest := c.log[len(c.log)-1]
	rises := make([]float64, n)
	for i := range rises {
		rises[i] = lim.phasesA[i] - c.riseBaseA[i]
	}
	riseA, phases := slices.Min(rises), ahead[:n]
	switch {
	case c.carPhase > 0 && c.carPhase <= n:
		riseA, phases = rises[c.carPhase-1], ahead[c.carPhase-1:c.carPhase]
	case c.carPhases != 3 || latest.n == 1:
		riseA = slices.Max(rises)
	}
	low, high := c.offerRangeA(lim.readingAt)
	toComeA := math.Max(0, latest.a-math.Min(high, math.Max(low, c.riseFromA+riseA)))
	for i := range phases {
		phases[i] = toComeA
	}
	return ahead
}

// recordEVOffers keeps each plugged charger's offers, with the current each
// puts on the phases its command names, for the longer of evSettle and the
// power_max_age_s its driver declares, or evUnknownSettle when it declares
// none. Our own commands only: a charger's power reading never counts. The
// samples kept for the battery side (noteDraw) take no part in the limit.
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
		c.settle, c.declared, c.carPhases, c.carPhase, c.driver, c.maxW = evUnknownSettle, false, point.CarPhases, point.CarPhase, point.DriverName, point.MaxChargeW
		if r := store.Get(point.DriverName, telemetry.DerEV); r != nil {
			if declared := telemetry.DeclaredPowerMaxAge(r.Data); declared > 0 {
				c.settle, c.declared = max(evSettle, declared), true
			}
		}
		w := math.Max(0, point.CommandedW)
		a, phases := s.offerPhaseA(w, point.CommandedPhases)
		sent := sentCommand{at: now, w: w, a: a, n: phases}
		if n := len(c.log); n == 0 || c.log[n-1].w != sent.w || c.log[n-1].a != sent.a || c.log[n-1].n != sent.n {
			if n == 0 && !s.evLimit.set {
				c.found, c.shownA = sent, [3]float64{sent.a, sent.a, sent.a}
			} else if _, high := c.offerRangeA(now); sent.a > high && !c.riseSet && s.evLimit.phasesSeen {
				c.riseBaseA, c.riseFromA, c.riseSet = s.evLimit.phasesA, high, true
			}
			c.log = c.log.keep(sent, read.Add(-c.settle))
		}
		c.noteDraw(store, now)
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
// busiest phase: below the fuse's working limit (fuseLineA), the last reading's
// current or heat, and every charger's offers that reading may not show.
// For an idle pool the heat counts only over the working limit, as the
// start reads it. false when the site meter reports no phase currents.
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
	phaseRoomA     float64                  // below the working limit on the busiest phase at readingAt; an idle pool's as a start reads it
	phaseRoomKnown bool                     // the meter reports phase currents
	phasesA        [3]float64               // the phase currents at readingAt, when phasesSeen
	phasesSeen     bool
	cutForA        [3]float64 // each phase's highest reading that a cut still on its way was sent on, when cutForSet
	cutForSet      bool
	overSince      time.Time          // first reading of a run over the hour's import limit
	fuseOverAt     time.Time          // first reading of a run with a phase, or the total, over the fuse's working limit
	fuseCutAt      time.Time          // the first cut of that run
	fuseUnderAt    time.Time          // first reading of a stretch under that limit
	overOffers     map[string]float64 // each charger's offer when that run began
	idleWh         float64            // import since roomSince, for the ceiling's start test
	byImport       bool               // the hour's import limit, not the fuse, left less room at readingAt
	batteryYield   evBatteryYield
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
// rises one step, or the rest of the demand when that is less, on a fresh
// reading with room for that rise below the working limit, at least evSettle
// after the last cut. Room counts every
// offer sent above what the reading may show as flowing until its charger
// can have applied it; a cut never frees room before the meter shows it.
// A cut counts our orders both ways: a raise on its way adds to the overage,
// and a cut on its way covers the overage it was sent for until its charger
// can have applied it, within its run over the limit and while the phases
// stay at or below fuseHourFactor (unshownOrders), but never once the meter
// shows it. Our own rise on its way cuts before the meter shows it where it
// would read above fuseInstantFactor, or over the working limit on a phase
// whose heat budget is spent.
// EVFusePhaseRoomA gives the same room per phase to the split between cars.
// An idle pool starts only after the meter has kept showing
// room for its minimum below the working limit. Each reading acts once, so a silent
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
	state.stepFuseHeat(meter, gridW)
	lim := &state.evLimit
	pool, plugged := evPoolFor(state, points, configs)
	if !plugged {
		*lim = evMeterLimit{}
		return 0, true
	}
	state.recordEVOffers(store, points)
	if !lim.set || meter.UpdatedAt.After(lim.readingAt) {
		state.stepEVMeterLimit(store, meter, gridW, pool, fuseMaxW)
	}
	return lim.capW, true
}

func (s *State) stepEVMeterLimit(store *telemetry.Store, meter *telemetry.DerReading, gridW float64, pool evPool, fuseMaxW float64) {
	lim, now, at := &s.evLimit, s.now(), meter.UpdatedAt
	first, prevAt := !lim.set, lim.readingAt
	gap := first || at.Sub(prevAt) > evSettle
	lim.readingAt = at
	s.seeOffers(meter, gridW)
	prev := math.Inf(1)
	if !first {
		prev = lim.capW
	}
	lim.set = true
	peakW := pool.offerW
	var logPeakW float64
	for _, c := range s.evLimit.offers {
		if _, high, _, ok := c.log.span(now); ok {
			logPeakW += high
		}
	}
	peakW = math.Max(peakW, logPeakW)
	// Our own battery orders the meter may not show yet count beside the
	// chargers' offers, in total import and on each phase.
	pending := s.pendingBatteryCommandsW(now)
	r := s.evMeterRoom(meter, gridW, pool.phases, fuseMaxW, s.withBatteryOrders(s.pendingLoad(at), pending))
	_, fresh := qualifiedFuseObservation(meter, now)
	fresh = fresh && r.measured
	lim.batteryYield.active = false
	if fresh && !r.overLimit && r.limitW >= 0 {
		lim.batteryYield = evBatteryYield{}
	}
	lim.phaseRoomA, lim.phaseRoomKnown = 0, r.phaseRoomKnown
	if fresh {
		lim.phaseRoomA = r.phaseRoomA
		if pool.offerW <= 0 {
			lim.phaseRoomA = r.startRoomA // The split between idle cars reads the start's room.
		}
	}
	step := evStepA * s.SiteFuseVoltage * pool.phases
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
	rise, cover, aheadA, latestW := s.unshownOrders(at, r.perPhase)
	fuseCutW, over := r.fuseOverW+rise.w-cover.w, r.fuseOverW > 0
	if r.perPhase && lim.phasesSeen {
		// A single-phase order may land on any phase, so the cut takes the
		// most a rise adds to a phase. A cut covers each phase by what it
		// takes off there, the same on every phase unless the meter tells
		// the phases apart (unshownOrders). Past the current a gG fuse
		// carries for an hour, nothing on its way holds back a cut.
		coverA := cover.phaseA
		if s.fuseHeat.worstA() > fuseHourFactor*s.SiteFuseAmps {
			coverA = [3]float64{}
		}
		if slices.Max(coverA[:s.SiteFusePhases]) <= 0 {
			lim.cutForSet = false
		}
		riseA := max(rise.phaseA[0], rise.phaseA[1], rise.phaseA[2])
		lineA, instantA, cutA := s.fuseLineA(), fuseInstantFactor*s.SiteFuseAmps, 0.0
		for i, a := range lim.phasesA[:s.SiteFusePhases] {
			// Cuts on their way cover a phase up to the reading they were
			// sent for. A phase that has risen since by more than noise
			// carries an overage that came later: they cover only the
			// part of it that phase had then.
			phaseCoverA, aheadA := coverA[i], aheadA[i]
			if lim.cutForSet && a > lim.cutForA[i]+evMoveA {
				phaseCoverA = math.Min(phaseCoverA, math.Max(0, lim.cutForA[i]-lineA))
			}
			// Where the heat budget is spent, the overage the meter shows
			// comes off with our rise on its way. Elsewhere our own rise on
			// its way is judged as the reading it will make: above
			// fuseInstantFactor, or over the line on a phase whose heat
			// budget is spent, it comes off now, as far as it takes the
			// phase over the line. Under that, the heat budget judges it
			// once the meter shows it, as any other reading.
			hot, needA := s.fuseHeat.sq[i] > lineA*lineA, 0.0
			if landA := a + aheadA - phaseCoverA; landA > instantA || hot && landA > lineA {
				needA = math.Min(aheadA-phaseCoverA, landA-lineA)
			}
			if a > lineA && (hot || a > instantA) {
				needA = math.Max(needA, a+riseA-phaseCoverA-lineA)
			}
			cutA = math.Max(cutA, needA)
		}
		fuseCutW, over = cutA*s.SiteFuseVoltage*pool.phases, over || cutA > 0
	}
	capW, byImport := math.Inf(1), false
	if over {
		capW = latestW
		if fuseCutW > 0 {
			capW -= fuseCutW
			if r.perPhase {
				// A charger rounds to the nearest amp, so a cut that leaves
				// half an amp would round back up and stay over the line.
				// Only a new cut rounds. When cuts on their way cover the
				// overage, the offer stays as it is, whole amps or not (a
				// step table, a PV surplus); rounding it would lower it
				// again on every reading.
				capW = wholeStepsW(capW, step)
			}
		}
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
		requiredW := math.Max(0, latestW-capW)
		if !byImport && pool.phases > 0 {
			requiredW *= float64(s.SiteFusePhases) / pool.phases
		}
		if s.yieldBatteryBeforeEVCut(store, meter, r, pool, requiredW, gridW, pending, rise, fresh) {
			capW = latestW // Keep the existing car order while the battery yields.
		}
		lim.byImport = byImport
		lim.capW, lim.roomSince = math.Min(prev, math.Max(0, capW)), time.Time{}
		if lim.capW < prev {
			lim.cutAt = now
			if lim.phasesSeen {
				for i, a := range lim.phasesA {
					if !lim.cutForSet || a > lim.cutForA[i] {
						lim.cutForA[i] = a
					}
				}
				lim.cutForSet = true
			}
			if r.fuseOverW > 0 && lim.fuseCutAt.IsZero() {
				lim.fuseCutAt = now
			}
			slog.Info("EV hard limit: site meter over limit", "grid_w", gridW, "worst_phase_a", r.worstA,
				"fuse_heat_a", s.fuseHeat.worstA(), "import_limit_w", s.effectiveImportCeilingW(fuseMaxW),
				"overage_w", r.fuseOverW, "import_overage_w", math.Max(0, -r.limitW),
				"unshown_rise_w", rise.w, "unshown_cut_w", cover.w, "rise_on_its_way_a", aheadA, "offer_w", latestW, "cap_w", lim.capW)
		}
		return
	}
	// An increase needs a fresh reading and room below the level increases
	// may reach, left after our own battery commands that the meter may not
	// show yet (in r). Between that level and the import limit the cap holds.
	// Battery discharge the fuse guard added ends once the site fits without
	// it, so it is no room: a car that took it would keep the battery
	// feeding the car at the line.
	if reliefW := s.evFuseReliefW(store); reliefW > 0 {
		r = s.evMeterRoom(meter, gridW, pool.phases, fuseMaxW, s.withBatteryOrders(s.pendingLoad(at), pending+reliefW))
	}
	fuseRoom, importRoom := 0.0, 0.0
	if fresh {
		fuseRoom, importRoom = math.Max(0, r.fuseW), math.Max(0, r.riseW)
	}
	room := math.Min(fuseRoom, importRoom)
	if pool.offerW <= 0 {
		s.stepEVIdleStart(r, gridW, pool, first, gap, fresh, at, prevAt, pending, step)
		return
	}
	// Keeping the current offers is never a cut. Headroom above them stays
	// within what this reading measured.
	lim.capW, lim.roomSince = math.Min(prev, peakW+room), time.Time{}
	// Right after a cut the meter may show battery relief that covers it.
	remaining := pool.wantW - peakW
	if lim.capW < pool.wantW && remaining > 0 && now.Sub(lim.cutAt) >= evSettle {
		// The fuse: a rise moves one step, or the rest of the demand when
		// that is less, once that move fits under the working limit. No
		// room is held back below the line; evSettle after a cut and the
		// heat the rise reads keep it from undoing a cut.
		fits, riseW := fuseRoom+1e-6 >= math.Min(step, remaining), math.Min(lim.capW+step, peakW+fuseRoom)
		// The hour's import limit keeps its own margin: a rise needs room
		// for two steps, and one stays free.
		if !math.IsInf(importRoom, 1) {
			fits, riseW = fits && importRoom >= 2*step, math.Min(riseW, peakW+importRoom-step)
		}
		if fits {
			lim.capW = math.Max(lim.capW, riseW)
		}
	}
	// The import limit's rise keeps a step free, but the battery leaves room
	// for the car's demand, not a further unused step. Let the last step use
	// that measured room, or concurrent battery charge can keep the car
	// short forever. Under the fuse alone the rise above already does this.
	if s.hasBatteryCharge() && remaining > 0 && remaining <= step && room+1e-6 >= remaining && now.Sub(lim.cutAt) >= evSettle {
		lim.capW = math.Max(lim.capW, pool.wantW)
	}
}

// evFuseReliefW is the fuse guard's battery relief the meter may show now:
// each battery's recorded relief, at most the discharge it reads. A battery
// with no live reading (watchdog-offline, or none yet) counts its whole
// relief: Core can no longer hold that discharge, and the car must not
// build on it. expireOfflineFuseRelief ends it evSettle after it went offline.
func (s *State) evFuseReliefW(store *telemetry.Store) float64 {
	var w float64
	for driver, relief := range s.batteryFuseRelief {
		if raw, _, ok := store.OnlineRawW(driver, telemetry.DerBattery); ok {
			relief = math.Min(relief, math.Max(0, -raw))
		}
		w += relief
	}
	return w
}

// expireOfflineFuseRelief ends each offline battery's recorded relief
// evSettle after dispatch first finds that battery offline. Core sent it its
// default mode then, and that command reaches the meter within evSettle, as
// ours do. Until then the car must not build on a discharge that may still
// stand; after it, holding the car below the line for relief Core no longer
// orders would be a hidden margin. It runs on every dispatch, whatever the
// other batteries do, so a dispatch that sends no command still ends it. A
// battery back online drops out of the clock: the next time it goes offline
// its relief counts whole again.
func (s *State) expireOfflineFuseRelief(online []batteryInfo) {
	now := s.now()
	live := make(map[string]bool, len(online))
	for _, b := range online {
		live[b.driver] = true
	}
	var relief map[string]float64
	var since map[string]time.Time
	for driver, w := range s.batteryFuseRelief {
		if !live[driver] {
			at, ok := s.batteriesOfflineSince[driver]
			if !ok {
				at = now
			}
			if now.Sub(at) >= evSettle {
				continue
			}
			if since == nil {
				since = make(map[string]time.Time)
			}
			since[driver] = at
		}
		if relief == nil {
			relief = make(map[string]float64)
		}
		relief[driver] = w
	}
	s.batteryFuseRelief, s.batteriesOfflineSince = relief, since
}

// wholeStepsW rounds w down to whole charger amps on the pool's phases, one
// step each. Chargers round an offer to the nearest amp, so a cap at the
// line's exact room could land half an amp over it. This is step
// quantisation, not a margin: a cap that is already whole amps stays.
func wholeStepsW(w, step float64) float64 {
	if !(step > 0) || math.IsInf(w, 0) {
		return w
	}
	return math.Floor(w/step+1e-9) * step
}

// stepEVIdleStart starts an idle pool once the meter has kept showing room
// for evRestartDwell: room on every reading for its minimum below the
// fuse's working limit. Under a measured hour, the ceiling's part is judged on the
// dwell's mean import, the house's load rather than one reading of it, and
// the minimum needs no step of margin there: a short overage waits.
func (s *State) stepEVIdleStart(r evMeterRoom, gridW float64, pool evPool, first, gap, fresh bool, at, prevAt time.Time, pending, step float64) {
	lim := &s.evLimit
	lim.capW = 0
	perReadingW := r.startW
	if !r.hourly {
		perReadingW = math.Min(perReadingW, r.riseW)
		lim.byImport = r.riseW < r.startW
	} else {
		// The start test below holds an idle car to the start level:
		// name it by this reading.
		lim.byImport = r.startLevelW-gridW-pending < r.startW
	}
	room := 0.0
	if fresh {
		room = math.Max(0, perReadingW) // r holds our battery orders.
	}
	// The fuse holds back nothing below its working limit: the charger
	// rounds the cap down to a step it can draw. The hour's import limit
	// keeps its separate rise margin.
	keepW := 0.0
	if !r.hourly && r.riseW < r.startW {
		keepW = step
	}
	if room+1e-6 < pool.minW+keepW {
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
	lim.capW = math.Max(pool.minW, wholeStepsW(room-keepW, step))
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
	fuseW          float64 // below the working limit (fuseLineA) and the worst phase, read or heat, and what we sent; rises
	fuseOverW      float64 // the fuse's measured overage that cuts now, 0 under it; see fuseHeat
	fuseOverA      float64 // the same on the worst phase, with perPhase
	perPhase       bool    // the phase currents measure the fuse
	overLimit      bool    // a phase, or the total without phases, reads over the fuse's working limit
	limitW         float64 // below the hour's import limit; negative is an overage
	riseW          float64 // below the level an increase may reach, less what we sent
	startLevelW    float64 // import an idle start may reach: the rise level, in the hour's last quarter at most the ceiling
	hourly         bool    // the hour's import is measured, so a short overage may wait
	measured       bool    // false when the meter reports phase currents that cannot be read
	worstA         float64
	phaseRoomA     float64 // below the working limit on the busiest phase, read or heat
	phaseRoomKnown bool    // the meter reports phase currents
	startW         float64 // fuseW for an idle start: the worst phase read, its heat only over the working limit (startLevelA)
	startRoomA     float64 // phaseRoomA the same way
}

func (s *State) evMeterRoom(meter *telemetry.DerReading, gridW, phases, fuseMaxW float64, sent pendingLoad) evMeterRoom {
	inf, totalW := math.Inf(1), math.Max(0, fuseMaxW-s.fuseSafetyMarginW())-gridW
	r := evMeterRoom{fuseW: totalW - sent.w, startW: totalW - sent.w, fuseOverW: math.Max(0, -totalW), overLimit: totalW < 0, limitW: inf, riseW: inf, startLevelW: inf}
	if limitW, riseW, ok := s.importHourLimitsW(); ok {
		r.limitW, r.riseW = limitW-gridW, riseW-gridW-sent.w
		// The ceiling caps the hour's mean, so an idle car may start on the
		// hour's slack, as a rise may. In the hour's last quarter it may
		// not: the next hour starts again at the ceiling and would stop it.
		r.startLevelW = riseW
		if now := s.now(); s.importHourStart(now).Add(time.Hour).Sub(now) < importHourRiseMinLeft {
			r.startLevelW = math.Min(riseW, s.PeakImportCeilingW)
		}
		r.hourly = s.importHourMeasured()
	}
	amps, present, valid := phaseCurrentsA(meter, s, gridW)
	if present && !valid {
		r.phaseRoomKnown = true // No room on phases it cannot read.
		return r
	}
	r.measured = true
	if present {
		limitA := s.fuseLineA()
		for _, a := range amps {
			r.worstA = math.Max(r.worstA, a)
		}
		r.fuseW = math.Min(r.fuseW, (limitA-s.fuseHeat.levelA(amps, sent.phaseA))*s.SiteFuseVoltage*phases)
		r.phaseRoomA, r.phaseRoomKnown = limitA-s.fuseHeat.levelA(amps, [3]float64{}), true
		r.startW = math.Min(r.startW, (limitA-s.fuseHeat.startLevelA(amps, sent.phaseA, limitA))*s.SiteFuseVoltage*phases)
		r.startRoomA = limitA - s.fuseHeat.startLevelA(amps, [3]float64{}, limitA)
		// The phases measure the fuse itself; total import stands in only without them.
		r.fuseOverA = s.fuseHeat.overA(amps, limitA, fuseInstantFactor*s.SiteFuseAmps)
		r.fuseOverW, r.perPhase, r.overLimit = r.fuseOverA*s.SiteFuseVoltage*phases, true, r.worstA > limitA
	}
	return r
}

// worstPhaseA is the highest import on the site's configured phases, with
// direction (phaseCurrentsA), and 0 when every phase exports. present is
// false when the meter reports no phase currents; valid is false when they
// cannot be used.
func worstPhaseA(meter *telemetry.DerReading, state *State, totalW float64) (worstA float64, present, valid bool) {
	amps, present, valid := phaseCurrentsA(meter, state, totalW)
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
