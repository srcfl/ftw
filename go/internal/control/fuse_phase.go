package control

import (
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// fusePhases is the main fuse as every charger and battery decision reads
// it: the site meter's current on each configured phase, import positive
// (phaseCurrentsA), under one working limit. Our own commands that a reading
// may not show yet are the callers' to add. A meter without phase currents
// stands in its total, shared evenly over the phases.
//
// A raise needs room on its phases: the limit less each phase's level and
// what we sent that it may not show yet. A cut waits for the phase's heat
// budget (fuseHeat), or not at all above fuseInstantFactor, and then takes
// the overage off the lever that lowers that phase's current.
type fusePhases struct {
	n        int
	voltage  float64
	limitA   float64
	amps     [3]float64 // the reading, import positive
	level    [3]float64 // what a raise starts from: an importing phase at the higher of its reading and heat
	over     [3]float64 // above the limit with the heat budget spent: positive import, negative export
	measured bool       // the meter reports phase currents
	blind    bool       // it reports phase currents that cannot be read: nothing may rise
}

// fuseMarginA is the one working margin below the fuse on every phase: the
// configured safety margin, at least evPhaseMarginA.
func (s *State) fuseMarginA() float64 { return math.Max(evPhaseMarginA, s.SiteFuseSafetyA) }

// fusePhasesAt reads one site reading and folds it into the heat model.
// false when no fuse is configured or the reading has no usable power.
func (s *State) fusePhasesAt(meter *telemetry.DerReading) (fusePhases, bool) {
	if meter == nil || !finiteFuseValue(s.SiteFuseAmps) || !finiteFuseValue(s.SiteFuseVoltage) ||
		s.SiteFuseAmps <= 0 || s.SiteFuseVoltage <= 0 || s.SiteFusePhases < 1 || s.SiteFusePhases > 3 {
		return fusePhases{}, false
	}
	f := fusePhases{n: s.SiteFusePhases, voltage: s.SiteFuseVoltage, limitA: s.SiteFuseAmps - s.fuseMarginA()}
	amps, present, valid := phaseCurrentsA(meter, s)
	f.measured, f.blind = present && valid, present && !valid
	if !f.measured {
		totalW := meter.RawW
		if !finiteFuseValue(totalW) {
			return fusePhases{}, false
		}
		amps = make([]float64, f.n)
		for i := range amps {
			amps[i] = totalW / (f.voltage * float64(f.n))
		}
	}
	s.stepFuseHeat(meter, amps)
	copy(f.amps[:], amps)
	instantA := fuseInstantFactor * s.SiteFuseAmps
	for i, a := range amps {
		f.level[i] = a
		if a >= 0 {
			f.level[i] = math.Max(a, math.Sqrt(s.fuseHeat.sq[i]))
		}
		if s.fuseHeat.sq[i] > f.limitA*f.limitA || math.Abs(a) > instantA {
			switch {
			case a > f.limitA:
				f.over[i] = a - f.limitA
			case a < -f.limitA:
				f.over[i] = a + f.limitA
			}
		}
	}
	return f, true
}

// roomA is the current a raise may still add on its busiest phase, after
// pendingA, from each phase's level: a charger's slow rise must not spend
// heat the last minute already used. Negative is a shortfall. Never
// positive when the phases cannot be read.
func (f fusePhases) roomA(pendingA [3]float64) float64 { return f.room(f.level, pendingA) }

// readingRoomA is roomA from the reading alone, for a battery: it answers
// within a tick, so a cut follows any overage at once and the heat's lag
// would only make it hunt.
func (f fusePhases) readingRoomA() float64 { return f.room(f.amps, [3]float64{}) }

func (f fusePhases) room(level, pendingA [3]float64) float64 {
	room := math.Inf(1)
	for i := range f.n {
		room = math.Min(room, f.limitA-level[i]-pendingA[i])
	}
	if f.blind {
		room = math.Min(room, 0)
	}
	return room
}

// exportRoomA is the export a discharge may still add on its busiest
// exporting phase.
func (f fusePhases) exportRoomA() float64 {
	room := math.Inf(1)
	for i := range f.n {
		room = math.Min(room, f.limitA+f.amps[i])
	}
	if f.blind {
		room = math.Min(room, 0)
	}
	return room
}

// importOverA is the most any phase's import stays above the limit after
// creditA, the cuts already sent that the reading may not show.
func (f fusePhases) importOverA(creditA [3]float64) float64 {
	over := 0.0
	for i := range f.n {
		over = math.Max(over, f.over[i]-creditA[i])
	}
	return over
}

// exportOverA is the most any phase's export is above the limit.
func (f fusePhases) exportOverA() float64 {
	over := 0.0
	for i := range f.n {
		over = math.Max(over, -f.over[i])
	}
	return over
}

// watts is a current on every phase as total watts: a three-phase battery
// or charger spreads its power evenly.
func (f fusePhases) watts(a float64) float64 { return a * f.voltage * float64(f.n) }

// highestA is the reading's highest import, for logs.
func (f fusePhases) highestA() float64 {
	high := math.Inf(-1)
	for i := range f.n {
		high = math.Max(high, f.amps[i])
	}
	return high
}

// siteImportCeilingW is the import the batteries and EVs may bring the site
// to from gridW: the hour's import limit, or the import at which the busiest
// phase meets the fuse's limit on the reading with the rest spread evenly,
// whichever is lower. byImport is true when it is the hour's. +Inf when
// neither is set.
func (s *State) siteImportCeilingW(meter *telemetry.DerReading, gridW float64) (ceilingW float64, byImport bool) {
	ceilingW = math.Inf(1)
	if f, ok := s.fusePhasesAt(meter); ok {
		ceilingW = gridW + f.watts(f.readingRoomA())
	}
	if limitW, _, ok := s.importHourLimitsW(); ok && limitW <= ceilingW {
		return limitW, true
	}
	return ceilingW, false
}

// evCutsSent is the EV reduction we sent during a run over a limit that
// began at since, which a reading taken at `at` may not show yet: for each
// charger still plugged, its highest offer since the run began less this
// tick's, in total watts and in current on every phase. No run waits longer
// than evSettle, the shortest a charger takes, so a cut that does not land
// leaves the overage to the next lever. A charger that leaves drops out: no
// cut of ours removed it. A one-phase cut may have landed on any phase, so
// it relieves none for certain.
func (s *State) evCutsSent(since, at time.Time) (w float64, phaseA [3]float64) {
	if since.IsZero() || at.Sub(since) >= evSettle {
		return 0, phaseA
	}
	for _, c := range s.evLimit.offers {
		latestW, plugged := s.evCommanded[c.driver]
		if !plugged {
			continue
		}
		cutW, three := c.log.unshownCut(since, at, math.Max(0, latestW))
		w += cutW
		if three && s.SiteFuseVoltage > 0 {
			for i := range phaseA {
				phaseA[i] += cutW / (s.SiteFuseVoltage * 3)
			}
		}
	}
	return w, phaseA
}

// evNeedW is the EV demand a reading taken at `at` may not show, before any
// optional battery charge: the chargers' demand, or the plan's while no
// charger has spoken (scheduledEVPowerW), less each charger's lowest offer
// the reading may show. phases is 1 when a demand may land on one phase.
func (s *State) evNeedW(at time.Time) (w, phases float64) {
	w, phases = scheduledEVPowerW(s), 3
	for _, demand := range s.evFuseDemands {
		if demand.phaseMode != "3p" {
			phases = 1
		}
	}
	for _, c := range s.evLimit.offers {
		w -= c.log.shownLow(at, c.settle)
	}
	return math.Max(0, w), phases
}

// fitBatteryRisesToFuse keeps each battery rise inside the room the meter
// shows: new import (charge, or less discharge) over our latest order fits
// the phases' room after what the EVs still need, EV first, and the hour's
// rise level; new export fits the phases' export room. Holding an order is
// never a rise. Optional charge yields to any shortfall at once, EV first,
// so the car keeps its offer; an overage beyond that waits for the fuse's
// heat budget in forceFuseDischarge, except an export overage, which takes
// discharge back here. Only the site meter and our own commands count.
// FuseSaturated reports that the EVs' need or a battery's charge did not
// fit; main.go replans on it and stops a battery boost.
func fitBatteryRisesToFuse(targets []DispatchTarget, store *telemetry.Store, state *State) []DispatchTarget {
	if store == nil || state == nil || len(targets) == 0 {
		return targets
	}
	_, hourRiseW, hourly := state.importHourLimitsW()
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	f, fused := state.fusePhasesAt(meter)
	if !fused && !hourly && (meter != nil || state.siteFuseMaxW() <= 0) {
		return targets
	}
	var gridW float64
	if meter != nil {
		gridW = meter.RawW
	}
	if meter == nil || !finiteFuseValue(gridW) {
		return floorPositiveEVBatteryTargets(targets) // A limit binds that nothing measures.
	}
	now := state.now()
	latest := make(map[string]float64, len(targets))
	for _, target := range targets {
		if _, seen := latest[target.Driver]; seen {
			return floorPositiveEVBatteryTargets(targets)
		}
		_, _, last, sent := state.batteryCommands[target.Driver].span(now)
		switch r := store.Get(target.Driver, telemetry.DerBattery); {
		case sent:
			latest[target.Driver] = last
		case r != nil:
			// Without a recent order the meter shows what it does now.
			latest[target.Driver] = r.SmoothedW
		default:
			// Unknown: it may be discharging and stop.
			latest[target.Driver] = -state.DriverLimits[target.Driver].dischargeCap()
		}
	}
	needW, needPhases := state.evNeedW(meter.UpdatedAt)
	fuseSpare, hourSpare := math.Inf(1), math.Inf(1)
	if fused {
		fuseSpare = f.watts(f.readingRoomA()) - needW*float64(f.n)/needPhases
	}
	if hourly {
		hourSpare = hourRiseW - gridW - needW
	}
	spare := math.Min(fuseSpare, hourSpare)
	keep, keepCharge := make(map[string]float64, len(targets)), make(map[string]float64, len(targets))
	for _, t := range targets {
		if r := store.Get(t.Driver, telemetry.DerBattery); r != nil {
			lower, upper := fuseTargetBounds(r, state.DriverLimits[t.Driver])
			keep[t.Driver], keepCharge[t.Driver] = math.Max(0, math.Min(0, t.TargetW)-lower), math.Max(0, upper-math.Max(0, t.TargetW))
		}
	}
	out := capBatteryCharge(targets, latest, state.pendingBatteryCommandsW(now), spare, keep)
	saturated := spare < 0
	for i := range out {
		saturated = saturated || out[i].TargetW < targets[i].TargetW
	}
	if saturated {
		state.FuseSaturated, state.FuseSaturatedByImport = true, hourSpare < fuseSpare
	}
	if !fused {
		return out
	}
	// Export: the same rule with the signs turned. An export overage the
	// heat budget no longer covers takes discharge back.
	exportSpare := math.Max(0, f.watts(f.exportRoomA()))
	if over := f.exportOverA(); over > 0 {
		exportSpare = -f.watts(over)
	}
	turned := make([]DispatchTarget, len(out))
	turnedLatest := make(map[string]float64, len(latest))
	for i, t := range out {
		turned[i] = DispatchTarget{Driver: t.Driver, TargetW: -t.TargetW, Clamped: t.Clamped}
	}
	for driver, w := range latest {
		turnedLatest[driver] = -w
	}
	turned = capBatteryCharge(turned, turnedLatest, state.pendingBatteryExportW(now), exportSpare, keepCharge)
	for i := range out {
		out[i].TargetW, out[i].Clamped = -turned[i].TargetW, turned[i].Clamped
	}
	return out
}

// capBatteryCharge keeps each battery's latest order: holding it is never an
// increase. A shortfall of measured room takes positive charge back; new
// charge needs room left after our increases the meter may not show yet. A
// reduction this tick funds nothing, as the meter does not show it. A
// discharge we end raises import too: where taking charge back leaves the
// room short, a battery keeps as much of it as the shortfall needs, up to
// keepW, what its floor still allows.
func capBatteryCharge(targets []DispatchTarget, latest map[string]float64, pendingW, spare float64, keepW map[string]float64) []DispatchTarget {
	if !finiteFuseValue(spare) || !finiteFuseValue(pendingW) {
		return floorPositiveEVBatteryTargets(targets)
	}
	out := append([]DispatchTarget(nil), targets...)
	holds, lost := make([]float64, len(out)), make([]float64, len(out))
	var holdW, increaseW, lostW float64
	for i, target := range out {
		last := latest[target.Driver]
		lost[i] = math.Max(0, math.Min(0, target.TargetW)-math.Min(0, last))
		lostW, spare = lostW+lost[i], spare-lost[i]
		if target.TargetW > 0 {
			holds[i] = math.Min(target.TargetW, math.Max(0, last))
			holdW += holds[i]
			increaseW += target.TargetW - holds[i]
		}
	}
	holdScale, increaseScale := 1.0, 0.0
	if spare < 0 && holdW > 0 {
		holdScale = math.Max(0, 1+spare/holdW)
	}
	if room := spare - pendingW; room > 0 && increaseW > 0 {
		increaseScale = math.Min(1, room/increaseW)
	}
	for i := range out {
		if out[i].TargetW <= 0 {
			continue
		}
		capped := holds[i]*holdScale + (out[i].TargetW-holds[i])*increaseScale
		if capped < out[i].TargetW {
			out[i].TargetW, out[i].Clamped = capped, true
		}
	}
	if short := -spare - holdW; short > 0 && lostW > 0 {
		k := math.Min(1, short/lostW)
		for i := range out {
			if keep := math.Min(lost[i]*k, keepW[out[i].Driver]); keep > 0 {
				out[i].TargetW, out[i].Clamped = math.Min(0, out[i].TargetW)-keep, true
			}
		}
	}
	return out
}

func floorPositiveEVBatteryTargets(targets []DispatchTarget) []DispatchTarget {
	out := append([]DispatchTarget(nil), targets...)
	for i := range out {
		if out[i].TargetW > 0 {
			out[i].TargetW, out[i].Clamped = 0, true
		}
	}
	return out
}

// pendingBatteryExportW is battery export we ordered that the meter may not
// show yet: a cut charge or a new discharge.
func (s *State) pendingBatteryExportW(now time.Time) float64 {
	var pending float64
	for _, log := range s.batteryCommands {
		if _, high, latest, ok := log.span(now); ok {
			pending += math.Max(0, high-latest)
		}
	}
	return pending
}

// PhaseImbalanceW is how far the busiest phase sits above the mean phase
// current, as total watts: import the planner leaves unused so that phase
// stays inside the fuse. Zero when the meter reports no phase currents or
// its reading is older than maxAge. Caller holds the control mutex.
func PhaseImbalanceW(store *telemetry.Store, state *State, maxAge time.Duration, now time.Time) float64 {
	if store == nil || state == nil || state.SiteMeterDriver == "" || state.SiteFusePhases <= 1 {
		return 0
	}
	r := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	if r == nil || now.Sub(r.UpdatedAt) > maxAge {
		return 0
	}
	amps, _, valid := phaseCurrentsA(r, state)
	if !valid {
		return 0
	}
	high, sum := math.Inf(-1), 0.0
	for _, a := range amps {
		high, sum = math.Max(high, a), sum+a
	}
	return math.Max(0, high-sum/float64(len(amps))) * state.SiteFuseVoltage * float64(len(amps))
}
