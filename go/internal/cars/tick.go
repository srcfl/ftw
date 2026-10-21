package cars

import (
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/internal/units"
	"github.com/srcfl/ftw/go/protocol"
)

// Tick follows the loadpoints and the cars' sources. It runs once per control
// tick, after the loadpoints have observed their chargers.
func (r *Runtime) Tick() {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	now := r.deps.Now()
	lps := r.deps.Loadpoints
	states := lps.States()
	var obs []telemetry.VehicleObservation
	if r.deps.Vehicles != nil {
		obs = r.deps.Vehicles()
	}

	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return
	}
	for _, d := range r.vehicleDrivers {
		if r.carForSourceLocked(d) == nil {
			r.autoBindLocked(d)
		}
	}
	changed := r.followSessionsLocked(states, now)
	acts, levelChanged := r.ingestLocked(obs, states, now)
	if r.identifyLocked(states, now) {
		changed = true
	}
	asks := r.askLocked(now)
	r.intentMoved = false
	if len(asks) > 0 {
		// The chargers now say asking: listeners such as the plan push
		// should see it.
		changed = true
	}
	acts = append(acts, r.syncLocked(now)...)
	// A goal or surplus choice the sync hands a loadpoint, as when someone
	// set a visitor's goal through a charger route, is a plan input.
	intentMoved := r.intentMoved
	if r.dirty || (r.levelSave && now.Sub(r.lastSave) >= levelSaveEvery) {
		if err := r.saveLocked(); err != nil {
			slog.Warn("cars: could not save; kept in memory", "err", err)
		}
	}
	r.mu.Unlock()

	replan := changed || levelChanged || intentMoved
	if replan {
		r.applyAndReplan(acts, "car_changed")
	} else {
		run(acts)
	}
	for _, ev := range asks {
		r.deps.Bus.Publish(ev)
	}
	if replan {
		r.notify()
	}
}

// followSessionsLocked opens a session at each plug-in and closes it at
// unplug, crediting the car with what the session delivered. An unplug
// counts once the charger has said so for UnplugGrace, or names a new
// session: a cable that flaps for one reading keeps its session and level.
func (r *Runtime) followSessionsLocked(states []loadpoint.State, now time.Time) bool {
	changed := false
	seen := map[string]bool{}
	for _, st := range states {
		seen[st.ID] = true
		if !st.Observed {
			// After a restart the loadpoint knows nothing of its cable
			// until the charger reports. A session it had stays as it was.
			continue
		}
		s := r.rec.Sessions[st.ID]
		if !st.PluggedIn {
			if s != nil {
				if s.unpluggedAt.IsZero() {
					s.unpluggedAt = now
				}
				if now.Sub(s.unpluggedAt) >= UnplugGrace {
					r.endSessionLocked(st.ID, s, s.unpluggedAt)
					changed = true
				}
			}
			continue
		}
		if s != nil && !s.unpluggedAt.IsZero() {
			if s.Key != "" && st.SessionKey != "" && s.Key != st.SessionKey {
				r.endSessionLocked(st.ID, s, s.unpluggedAt)
				s = nil
			} else {
				r.resumeSessionLocked(st, s)
			}
		}
		if s != nil && !r.samePlugLocked(s, st) {
			r.endSessionLocked(st.ID, s, now)
			s = nil
		}
		if s == nil {
			s = r.openSessionLocked(st, now)
			changed = true
		}
		if s.Key == "" && st.SessionKey != "" {
			s.Key = st.SessionKey
			r.dirty = true
		}
		if st.DeliveredWhSession-s.LastWh >= 100 || st.DeliveredWhSession < s.LastWh {
			// Kept on disk at the level cadence, so an unplug the box did
			// not see credits close to what the car got.
			r.levelSave = true
		}
		s.LastWh = st.DeliveredWhSession
	}
	for lp, s := range r.rec.Sessions {
		if !seen[lp] {
			r.endSessionLocked(lp, s, now)
			changed = true
		}
	}
	return changed
}

// openSessionLocked starts the session of a plug-in, with the charger's
// word if it came first.
func (r *Runtime) openSessionLocked(st loadpoint.State, now time.Time) *session {
	plugged := st.PluggedAt
	if plugged.IsZero() {
		plugged = now
	}
	s := &session{ID: newSessionID(), By: protocol.CarIdentifiedByNone, Key: st.SessionKey,
		PluggedMs: plugged.UnixMilli(), BoundMs: now.UnixMilli(), connMs: plugged.UnixMilli(),
		uncertain: plugged.Sub(r.startedAt) < bootWindow}
	if p, ok := r.pendingIdent[st.ID]; ok && absDuration(p.at.Sub(plugged)) <= IdentifyWindow {
		s.chargerIdent, s.chargerIdentAt = p.ident, p.at
	}
	delete(r.pendingIdent, st.ID)
	r.rec.Sessions[st.ID] = s
	r.dirty = true
	return s
}

// resumeSessionLocked keeps a session whose cable came back within
// UnplugGrace. The loadpoint may count from zero again: what flowed before
// is the car's, and the car's level starts the loadpoint once more.
func (r *Runtime) resumeSessionLocked(st loadpoint.State, s *session) {
	s.unpluggedAt = time.Time{}
	s.connMs = st.PluggedAt.UnixMilli()
	if car := r.carLocked(s.CarID); car != nil && car.Level.LP == st.ID {
		car.Level.BaseWh += s.LastWh - car.Level.SessionWh
		car.Level.SessionWh = st.DeliveredWhSession
	}
	s.LastWh = st.DeliveredWhSession
	s.seeded = false
	delete(r.applied, st.ID)
	r.dirty = true
	slog.Info("cars: the cable came back within the grace; the session goes on", "lp", st.ID)
}

// bootWindow is how soon after the box starts a plug-in counts as one it
// found already plugged in.
const bootWindow = 30 * time.Second

// UnplugGrace is how long a charger must say the cable is out before the
// session ends.
const UnplugGrace = 30 * time.Second

// PrePlugMaxAge is how much older than the plug-in a level may be and still
// start the session. An older one may predate a drive.
const PrePlugMaxAge = time.Hour

// tooOldFor reports whether a level read at atMs is too old to start the
// session s.
func tooOldFor(atMs int64, s *session) bool {
	return atMs < s.PluggedMs-PrePlugMaxAge.Milliseconds()
}

// samePlugLocked tells one plug-in from the next. A session read back after
// a restart is the same one when the charger's own session id says so, or,
// without one, when its energy counter has not started again.
func (r *Runtime) samePlugLocked(s *session, st loadpoint.State) bool {
	if s.restored {
		s.restored = false
		var same bool
		if s.Key != "" && st.SessionKey != "" {
			same = s.Key == st.SessionKey
		} else {
			same = st.DeliveredWhSession >= s.LastWh
		}
		if same {
			// PluggedMs stays the plug-in itself; the loadpoint's own
			// connection time starts again at the restart.
			s.connMs = st.PluggedAt.UnixMilli()
			// The loadpoint restores its own level when it can. When it
			// could not, the car's level starts the session again.
			s.seeded = st.SoCSource != "assumed"
		}
		return same
	}
	if s.Key != "" && st.SessionKey != "" {
		return s.Key == st.SessionKey
	}
	return s.connMs == st.PluggedAt.UnixMilli()
}

func (r *Runtime) endSessionLocked(lpID string, s *session, at time.Time) {
	if car := r.carLocked(s.CarID); car != nil {
		r.creditLocked(car, lpID, s.LastWh)
		car.UnpluggedMs = at.UnixMilli()
		r.rec.LastCar[lpID] = car.ID
	}
	delete(r.rec.Sessions, lpID)
	r.dirty = true
}

// creditLocked moves the session's energy since the car's reading into the
// car's level, as the session ends.
func (r *Runtime) creditLocked(car *Car, lpID string, sessionWh float64) {
	if car.Level.Source == "" {
		return
	}
	from := 0.0
	if car.Level.LP == lpID {
		from = car.Level.SessionWh
	}
	// Negative when a V2X session took energy out.
	car.Level.BaseWh += sessionWh - from
	car.Level.LP, car.Level.SessionWh = "", 0
}

// ingestLocked reads each car's sources. A newer reading becomes the car's
// level, and the loadpoint the car is on is anchored to it. It returns the
// anchors to run once the lock is released, and whether a car's level
// source changed.
func (r *Runtime) ingestLocked(obs []telemetry.VehicleObservation, states []loadpoint.State, now time.Time) ([]func(), bool) {
	lps := r.deps.Loadpoints
	byID := statesByID(states)
	var acts []func()
	changed := false
	for _, o := range obs {
		ss := r.sources[o.Driver]
		if ss == nil {
			ss = &sourceState{}
			r.sources[o.Driver] = ss
		}
		plugged := pluggedEvidence(o)
		switch {
		case !plugged:
			ss.pluggedSince = time.Time{}
		case o.Plugged != nil && !o.PluggedSince.IsZero():
			// The car's own clock says when its cable went in, which a
			// source that reports minutes apart, or a box that just
			// started, cannot see for itself.
			ss.pluggedSince = o.PluggedSince
			if ss.pluggedSince.After(now) {
				ss.pluggedSince = now
			}
		case !ss.plugged && ss.seen && o.At.Sub(ss.obs.At) <= IdentifyWindow:
			// The box saw the change between two readings less than a
			// minute apart. Further apart, the cable could have gone in at
			// any time between them, and the time stays unknown.
			ss.pluggedSince = o.ReportedAt
			if ss.pluggedSince.IsZero() || ss.pluggedSince.After(now) {
				ss.pluggedSince = now
			}
		}
		ss.plugged, ss.seen, ss.obs = plugged, true, o

		car := r.carForSourceLocked(o.Driver)
		if car == nil {
			car = r.autoBindLocked(o.Driver)
		}
		if car == nil || !newerReading(car, o.At) {
			continue
		}
		lpID, s := r.sessionOfLocked(car.ID)
		if s != nil && tooOldFor(o.At.UnixMilli(), s) {
			// Read more than PrePlugMaxAge before this plug-in: the car may
			// have driven since, and the session keeps its own start.
			continue
		}
		lvl := Level{SoC: o.SoC, Source: protocol.CarSoCVehicle, AtMs: o.At.UnixMilli(), ChargeLimit: o.ChargeLimit}
		if s != nil {
			lvl.LP = lpID
			if lvl.AtMs >= s.PluggedMs {
				wh, ok := lps.DeliveredAt(lpID, o.At)
				if !ok {
					wh = byID[lpID].DeliveredWhSession
				}
				lvl.SessionWh = wh
			}
		}
		if car.Level.Source != protocol.CarSoCVehicle {
			changed = true
		}
		car.Level = lvl
		r.levelSave = true
		if s == nil || !s.seeded {
			continue
		}
		soc, at := o.SoC, o.At
		// A reading from before a plug-in the box saw happen is the level
		// the session started from. One from before a session the box
		// found plugged in at start may be from after the plug-in: the
		// loadpoint adopts it without energy it cannot account for.
		beforePlugIn := lvl.AtMs < s.PluggedMs && !s.uncertain
		acts = append(acts, func() {
			if !lps.VehicleSoCSupersedes(lpID, at) {
				return
			}
			if beforePlugIn {
				// Read before this plug-in: that is the level the session
				// started from, and all its energy went into the car since.
				lps.SeedSessionLevel(lpID, loadpoint.LevelSeed{Known: true, SoC: soc,
					Confirmed: true, ConfirmedAt: at, FromCar: true})
				return
			}
			lps.AnchorVehicleSoCAt(lpID, soc, at)
		})
	}
	return acts, changed
}

// newerReading reports whether a reading made at at replaces the car's level:
// one newer than it, or for a plug-in assumption, which is no reading at all,
// any made since the car last left a charger.
func newerReading(car *Car, at time.Time) bool {
	if car.Level.Source == protocol.CarSoCAssumed {
		return at.UnixMilli() > car.UnpluggedMs
	}
	return at.UnixMilli() > car.Level.AtMs
}

// pluggedEvidence is the car's own word that a cable is in: its plugged
// flag, or else a charging state that needs one.
func pluggedEvidence(o telemetry.VehicleObservation) bool {
	if o.Plugged != nil {
		return *o.Plugged
	}
	return telemetry.VehicleConnectedRank(o.ChargingState) >= 1
}

// autoBindLocked binds a vehicle driver to the car with its VIN, or to the
// only car. A driver the owner unbound stays unbound.
func (r *Runtime) autoBindLocked(driver string) *Car {
	if slices.Contains(r.rec.Unbound, driver) {
		return nil
	}
	var make, serial string
	if r.deps.Identity != nil {
		make, serial = r.deps.Identity(driver)
	}
	vin := normVIN(serial)
	var car *Car
	if vin != "" {
		for _, c := range r.rec.Cars {
			if c.VIN == vin {
				car = c
				break
			}
		}
	}
	if car == nil && len(r.rec.Cars) == 1 {
		if c := r.rec.Cars[0]; c.VIN == "" || vin == "" || c.VIN == vin {
			car = c
		}
	}
	if car == nil {
		return nil
	}
	car.Sources = append(car.Sources, driver)
	if car.VIN == "" {
		car.VIN = vin
	}
	if car.Make == "" {
		car.Make = make
	}
	r.dirty = true
	slog.Info("cars: bound a vehicle driver to a car", "driver", driver, "car", car.ID, "by_vin", vin != "" && car.VIN == vin)
	return car
}

// identifyLocked asks, for each open session, which car it is.
func (r *Runtime) identifyLocked(states []loadpoint.State, now time.Time) bool {
	changed := false
	for _, st := range states {
		s := r.rec.Sessions[st.ID]
		if s == nil || !st.PluggedIn {
			continue
		}
		if r.applyAnswerLocked(st.ID, s, r.answerLocked(st, s, states, now), now) {
			changed = true
		}
	}
	return changed
}

// answer is what the identification order says about a session.
type answer struct {
	car string
	by  string
	// rank is the rule that answered, as identifiedRank orders them.
	rank int
	at   time.Time
	// decisive is set when a rule answered "not one of the cars": the
	// charger named a car the box does not know, or the only car said it is
	// somewhere else. Without it an empty car means nobody answered.
	decisive bool
}

// answerLocked runs the identification order (docs/cars.md): the charger,
// the car, the only car. A tap is not an answer here; it is held until a
// rule may correct it.
func (r *Runtime) answerLocked(st loadpoint.State, s *session, states []loadpoint.State, now time.Time) answer {
	none := answer{by: protocol.CarIdentifiedByNone, rank: identifiedRank[protocol.CarIdentifiedByNone], at: now}
	if s.chargerIdent != "" {
		if c := r.carWithIdentifierLocked(s.chargerIdent); c != nil {
			return answer{car: c.ID, by: protocol.CarIdentifiedByCharger, at: s.chargerIdentAt}
		}
		if r.anyIdentifiersLocked() {
			// The charger named another car: a visitor.
			return answer{by: protocol.CarIdentifiedByNone, at: s.chargerIdentAt, decisive: true}
		}
		// No car lists identifiers, so the charger's names nobody: an OCPP
		// charger sends an idTag at every start, often the same for every
		// car. The other rules answer.
	}
	if c, evidenceAt := r.carEvidenceLocked(st, s, states, now); c != "" {
		return answer{car: c, by: protocol.CarIdentifiedByCar, rank: identifiedRank[protocol.CarIdentifiedByCar], at: evidenceAt}
	}
	candidates, awayAt := r.candidatesLocked(st.ID, s)
	if len(candidates) == 1 {
		return answer{car: candidates[0].ID, by: protocol.CarIdentifiedByOnly, rank: identifiedRank[protocol.CarIdentifiedByOnly],
			at: time.UnixMilli(s.PluggedMs)}
	}
	if len(candidates) == 0 && !awayAt.IsZero() {
		// Every car that could be here said it is not: a visitor.
		return answer{by: protocol.CarIdentifiedByNone, rank: identifiedRank[protocol.CarIdentifiedByOnly], at: awayAt, decisive: true}
	}
	if usual := r.usualCarLocked(st.ID, candidates); usual != nil {
		return answer{car: usual.ID, by: protocol.CarIdentifiedByOnly, rank: identifiedRank[protocol.CarIdentifiedByOnly],
			at: time.UnixMilli(s.PluggedMs)}
	}
	return none
}

// usualCarLocked is the car whose usual charger this is, when no other car
// that could be here claims it: each of them has a usual charger elsewhere. A
// car with none, as one an owner added since, could be on any charger, and
// the box asks.
func (r *Runtime) usualCarLocked(lpID string, candidates []*Car) *Car {
	var found *Car
	for _, c := range candidates {
		switch c.UsualLP {
		case lpID:
			if found != nil {
				return nil
			}
			found = c
		case "":
			return nil
		}
	}
	return found
}

// onOtherChargerLocked reports whether a car is the car of another plugged-in
// charger's session: one car is on one charger at a time.
func (r *Runtime) onOtherChargerLocked(carID, lpID string) bool {
	other, _ := r.sessionOfLocked(carID)
	return other != "" && other != lpID
}

// candidatesLocked are the cars that could be on a charger's session: every
// car not on another charger whose source has not read it elsewhere, or with
// its cable out, since the plug-in. Rule 3 answers when there is exactly one.
// awayAt is the latest such reading, zero when no car was ruled out.
func (r *Runtime) candidatesLocked(lpID string, s *session) (out []*Car, awayAt time.Time) {
	for _, c := range r.rec.Cars {
		if r.onOtherChargerLocked(c.ID, lpID) {
			continue
		}
		if at, away := r.notHereLocked(c, s); away {
			if at.After(awayAt) {
				awayAt = at
			}
			continue
		}
		out = append(out, c)
	}
	return out, awayAt
}

// notHereLocked reports whether one of the car's sources read the car within
// IdentifyWindow after the plug-in and found it not here: somewhere else, or
// with its cable out. The reading's own time counts, not when it arrived: a
// source that replays a reading from before the plug-in, or reads the car
// minutes apart, has not seen this plug-in, and its old "cable out" would
// make the only car a visitor.
func (r *Runtime) notHereLocked(car *Car, s *session) (time.Time, bool) {
	plugAt := time.UnixMilli(s.PluggedMs)
	for _, d := range car.Sources {
		ss := r.sources[d]
		if ss == nil || !ss.seen {
			continue
		}
		o := ss.obs
		at := o.At
		if at.Before(plugAt) || at.Sub(plugAt) > IdentifyWindow {
			continue
		}
		if (o.AtSite != nil && !*o.AtSite) || (o.Plugged != nil && !*o.Plugged) {
			return at, true
		}
	}
	return time.Time{}, false
}

// carEvidenceLocked is rule 2: a car's own source says it was plugged in
// within IdentifyWindow of this plug-in, or says it is charging at this site
// while this charger alone delivers, and none of the car's sources says it is
// somewhere else. Two cars saying so is no answer. Nor is a cable time when
// another charger saw a plug-in within the same window: a car whose cable
// went in then could be on either.
func (r *Runtime) carEvidenceLocked(st loadpoint.State, s *session, states []loadpoint.State, now time.Time) (string, time.Time) {
	plugAt := time.UnixMilli(s.PluggedMs)
	delivering := st.CurrentPowerW >= loadpoint.DeliveringW
	byCable := true
	for _, other := range states {
		if other.ID == st.ID || !other.PluggedIn {
			continue
		}
		if other.CurrentPowerW >= loadpoint.DeliveringW {
			delivering = false
		}
		if os := r.rec.Sessions[other.ID]; os != nil &&
			absDuration(time.UnixMilli(os.PluggedMs).Sub(plugAt)) <= IdentifyWindow {
			byCable = false
		}
	}
	found, foundAt := "", time.Time{}
	for _, c := range r.rec.Cars {
		if r.onOtherChargerLocked(c.ID, st.ID) {
			continue
		}
		claims, away, at := false, false, time.Time{}
		for _, d := range c.Sources {
			ss := r.sources[d]
			if ss == nil || !ss.seen {
				continue
			}
			o := ss.obs
			if o.AtSite != nil && !*o.AtSite {
				away = true
				break
			}
			switch {
			case byCable && ss.plugged && !ss.pluggedSince.IsZero() && absDuration(ss.pluggedSince.Sub(plugAt)) <= IdentifyWindow:
				claims, at = true, ss.pluggedSince
			case delivering && o.AtSite != nil && *o.AtSite && o.Live(now) &&
				telemetry.VehicleConnectedRank(o.ChargingState) == 3:
				claims, at = true, now
			}
		}
		if !claims || away {
			continue
		}
		if found != "" {
			return "", time.Time{}
		}
		found, foundAt = c.ID, at
	}
	return found, foundAt
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func (r *Runtime) carWithIdentifierLocked(ident string) *Car {
	want := normIdentifier(ident)
	if want == "" {
		return nil
	}
	for _, c := range r.rec.Cars {
		for _, id := range c.Identifiers {
			if normIdentifier(id) == want {
				return c
			}
		}
	}
	return nil
}

// applyAnswerLocked binds the session to the answer, or keeps what it has.
//
// An answer the box found by itself may be corrected by the charger or the
// car, never by a worse rule, and only with evidence newer than the choice;
// the only car may be corrected by any of them. A tap may be corrected the
// same way, unless it overrode an answer the box had found: then it is
// final. "Nobody answered" never unbinds a car.
func (r *Runtime) applyAnswerLocked(lpID string, s *session, a answer, now time.Time) bool {
	cur := s.By
	if a.car == s.CarID {
		switch {
		case a.car == "" && a.decisive && !s.Tapped && cur != protocol.CarIdentifiedByNone:
			// asking, and then the charger or the only car said it is
			// another car: a visitor, and nothing to ask.
			s.By, s.Asked = protocol.CarIdentifiedByNone, true
			r.dirty = true
			return true
		case a.car == "" && a.decisive && !s.Asked:
			s.Asked = true
			r.dirty = true
			return false
		case a.car != "" && a.by != cur && !s.Tapped && identifiedRank[a.by] < identifiedRank[cur]:
			s.By = a.by
			r.dirty = true
			return true
		}
		return false
	}
	if a.car == "" && !a.decisive {
		return false
	}
	corrects := a.by == protocol.CarIdentifiedByCharger || a.by == protocol.CarIdentifiedByCar || a.decisive
	newer := a.at.After(time.UnixMilli(s.BoundMs))
	switch {
	case s.Tapped:
		if s.TapFinal || !corrects || !newer || a.by == protocol.CarIdentifiedByOnly {
			return false
		}
	case cur == protocol.CarIdentifiedByOnly:
		if !corrects {
			return false
		}
	case cur == protocol.CarIdentifiedByCharger || cur == protocol.CarIdentifiedByCar:
		if !corrects || a.rank > identifiedRank[cur] || !newer {
			return false
		}
	}
	by := a.by
	if a.car == "" {
		by = protocol.CarIdentifiedByNone
		s.Asked = true
	}
	r.bindLocked(lpID, s, a.car, by, now)
	s.Tapped, s.TapFinal = false, false
	return true
}

// bindLocked names the session's car. A session that had another car marks
// the change as a correction; the car it had gets its level from before
// the session back, unless its own source read it since.
func (r *Runtime) bindLocked(lpID string, s *session, carID, by string, now time.Time) {
	old := s.CarID
	if oldCar := r.carLocked(old); oldCar != nil && oldCar.Level.LP == lpID {
		if oldCar.Level.Source != protocol.CarSoCVehicle && s.Prev != nil {
			oldCar.Level = *s.Prev
		} else {
			oldCar.Level.LP, oldCar.Level.SessionWh = "", 0
		}
	}
	s.Prev = nil
	if old != "" && old != carID {
		s.CorrectedFrom = old
		slog.Info("cars: the session's car changed", "lp", lpID, "from", old, "to", carID, "by", by)
	}
	s.CarID, s.By = carID, by
	s.BoundMs = now.UnixMilli()
	s.seeded = false
	s.goal, s.surplus = nil, nil
	if carID != "" {
		r.rec.LastCar[lpID] = carID
	}
	r.dirty = true
}

// askLocked raises car.which once per plug-in, when a box with several cars
// has had no answer within IdentifyWindow, and the session says asking.
func (r *Runtime) askLocked(now time.Time) []events.Event {
	if len(r.rec.Cars) < 2 || r.deps.Bus == nil {
		return nil
	}
	var out []events.Event
	for lp, s := range r.rec.Sessions {
		if s.By != protocol.CarIdentifiedByNone || s.Asked || s.Tapped || !s.unpluggedAt.IsZero() ||
			now.Sub(time.UnixMilli(s.PluggedMs)) < IdentifyWindow {
			continue
		}
		s.Asked = true
		s.By = protocol.CarIdentifiedByAsking
		r.dirty = true
		choose := r.chooseLocked(lp)
		out = append(out, events.CarWhich{LoadpointID: lp, Session: s.ID, CarIDs: choose, At: now})
		slog.Info("cars: asking which car is plugged in", "lp", lp, "offered", len(choose))
	}
	return out
}

// chooseLocked is the cars a tap may pick on a charger: those not on another
// charger, at most four. With more, it offers none and the phone opens FTW.
func (r *Runtime) chooseLocked(lpID string) []string {
	var ids []string
	for _, c := range r.rec.Cars {
		if other, _ := r.sessionOfLocked(c.ID); other != "" && other != lpID {
			continue
		}
		ids = append(ids, c.ID)
	}
	if len(ids) > maxChoose {
		return nil
	}
	return ids
}

// maxChoose is how many cars a notification can offer as buttons.
const maxChoose = 4

// syncLocked hands each loadpoint the car it now has: capacity and limits,
// goal and surplus choice, and at a new binding the car's level. It returns
// the calls to make once the lock is released, because the loadpoint's
// savers call back into this runtime.
func (r *Runtime) syncLocked(now time.Time) []func() {
	lps := r.deps.Loadpoints
	var acts []func()
	states := lps.States()
	byID := statesByID(states)
	for _, st := range states {
		id := st.ID
		s := r.rec.Sessions[id]
		var (
			b       loadpoint.CarBinding
			car     *Car
			goal    loadpoint.Schedule
			surplus bool
			intent  = true
		)
		if s != nil && st.PluggedIn {
			car = r.carLocked(s.CarID)
			if car != nil {
				b, goal, surplus = bindingFor(car), car.Goal, car.SurplusOnly
				b.ChargeLimit = r.planningLimitLocked(car, byID)
			} else {
				// A visitor charges from surplus only until someone sets a
				// goal for the session.
				surplus = true
				if s.goal != nil {
					goal = *s.goal
				}
				if s.surplus != nil {
					surplus = *s.surplus
				}
			}
			b.Session, b.IdentifiedBy, b.CorrectedFrom = s.ID, s.By, s.CorrectedFrom
			b.Corrected = s.CorrectedFrom != ""
			if s.By == protocol.CarIdentifiedByAsking {
				b.Choose = strings.Join(r.chooseLocked(id), ",")
			}
		} else if car, _ = r.viewCarLocked(id); car != nil {
			b, goal, surplus = bindingFor(car), car.Goal, car.SurplusOnly
		} else {
			intent = false
		}
		if prev, ok := r.applied[id]; !ok || prev != b {
			r.applied[id] = b
			acts = append(acts, func() { lps.SetCarBinding(id, b) })
		}
		if intent {
			if !sameGoal(st.Schedule, goal) {
				// The loadpoint keeps the site rule it holds when the goal
				// lands, not the one read here.
				next := goalOnly(goal)
				acts = append(acts, func() { lps.ApplyCarGoal(id, next, sameGoal) })
				r.intentMoved = true
			}
			if st.SurplusOnly != surplus {
				acts = append(acts, func() { lps.ApplySurplusOnly(id, surplus) })
				r.intentMoved = true
			}
		}
		if s != nil && st.PluggedIn && !s.seeded {
			s.seeded = true
			seed := r.seedLocked(car, s, st)
			acts = append(acts, func() { lps.SeedSessionLevel(id, seed) })
			if car != nil && car.ProfileTarget > 0 {
				target := car.ProfileTarget
				acts = append(acts, func() { lps.SetTarget(id, target, time.Time{}) })
			}
		}
	}
	return acts
}

func bindingFor(car *Car) loadpoint.CarBinding {
	return loadpoint.CarBinding{ID: car.ID, Name: car.Name, CapacityWh: car.CapacityWh, MaxW: car.MaxW, Phases: car.Phases}
}

// planningLimitLocked is the car's charge limit as the plan and the reserve
// read it: only while its level is its own exact reading.
func (r *Runtime) planningLimitLocked(car *Car, byID map[string]loadpoint.State) float64 {
	if car.Level.Source != protocol.CarSoCVehicle {
		return 0
	}
	if _, energy := r.levelNowLocked(car, byID); estimate(car, energy) {
		return 0
	}
	return car.Level.ChargeLimit
}

// seedLocked is the level a session starts from. A car's level carries into
// a session only when the car has not left a charger since it was read; a
// car that may have driven starts from its plug-in assumption, as before
// cars. A visitor starts from nothing.
func (r *Runtime) seedLocked(car *Car, s *session, st loadpoint.State) loadpoint.LevelSeed {
	if car == nil {
		return loadpoint.LevelSeed{}
	}
	if s.Prev == nil {
		// The level before this session, which a switch to another car
		// puts back. A session resumed after a flap keeps the first.
		prev := car.Level
		s.Prev = &prev
	}
	lvl := &car.Level
	carried := lvl.Source != "" && (car.UnpluggedMs == 0 || lvl.AtMs > car.UnpluggedMs) && !tooOldFor(lvl.AtMs, s)
	readHere := carried && lvl.AtMs >= s.PluggedMs
	if s.CorrectedFrom == "" && !readHere && !st.SoCUnknown && st.SoCSource != "assumed" {
		// The session's first car, and the loadpoint already holds a level
		// it trusts: restored after a restart, or set while the box asked.
		// That level is this car's; a seed from an older one would undo it.
		*lvl = r.adoptedLevelLocked(st, s, lvl.ChargeLimit)
		r.dirty = true
		return loadpoint.LevelSeed{Known: true, SoC: st.CurrentSoC, SessionWh: st.DeliveredWhSession,
			Confirmed: true, ConfirmedAt: st.SoCConfirmedAt, FromCar: st.LevelFromCar}
	}
	if !carried {
		soc := car.PluginSoC
		if soc <= 0 {
			soc = units.DefaultPluginSoC
		}
		*lvl = Level{SoC: soc, Source: protocol.CarSoCAssumed, AtMs: s.PluggedMs, LP: st.ID}
		r.dirty = true
		return loadpoint.LevelSeed{Known: true, SoC: soc}
	}
	seed := loadpoint.LevelSeed{Known: true, SoC: lvl.SoC, Confirmed: lvl.Source != protocol.CarSoCAssumed,
		ConfirmedAt: lvl.at(), FromCar: lvl.Source == protocol.CarSoCVehicle}
	switch {
	case lvl.LP == st.ID:
		// Placed in this session. After a flap, what flowed before it is
		// in BaseWh.
		seed.SessionWh = lvl.SessionWh
		if lvl.BaseWh != 0 && car.CapacityWh > 0 {
			seed.SoC = units.ClampFraction(lvl.SoC + lvl.BaseWh*loadpoint.DefaultChargeEfficiency/car.CapacityWh)
			seed.FromCar = false
		}
	case lvl.AtMs >= s.PluggedMs:
		// Read in this session, before the box knew which car it was.
		wh, ok := r.deps.Loadpoints.DeliveredAt(st.ID, lvl.at())
		if !ok {
			wh = st.DeliveredWhSession
		}
		lvl.LP, lvl.SessionWh = st.ID, wh
		seed.SessionWh = wh
		r.dirty = true
	case s.uncertain:
		// Found plugged in at start: the reading may be from after the
		// plug-in, so it is the level now, without energy the box cannot
		// account for.
		lvl.LP, lvl.SessionWh = st.ID, st.DeliveredWhSession
		seed.SessionWh = st.DeliveredWhSession
		r.dirty = true
	default:
		// Read before the plug-in: the level then, with what ended
		// sessions delivered since.
		if lvl.BaseWh > 0 && car.CapacityWh > 0 {
			seed.SoC = units.ClampFraction(lvl.SoC + lvl.BaseWh*loadpoint.DefaultChargeEfficiency/car.CapacityWh)
			seed.FromCar = false
		}
	}
	return seed
}

// adoptedLevelLocked is the level a loadpoint holds, as the car's: the
// reading it was confirmed with, at that reading's time and counter, so the
// car shows an estimate once the energy since says so. Without that time it
// is the level at the plug-in with the whole session since.
func (r *Runtime) adoptedLevelLocked(st loadpoint.State, s *session, chargeLimit float64) Level {
	source := protocol.CarSoCManual
	if st.LevelFromCar {
		source = protocol.CarSoCVehicle
	}
	atMs, wh := s.PluggedMs, 0.0
	if !st.SoCConfirmedAt.IsZero() {
		if d, ok := r.deps.Loadpoints.DeliveredAt(st.ID, st.SoCConfirmedAt); ok {
			atMs, wh = st.SoCConfirmedAt.UnixMilli(), d
		}
	}
	soc := st.CurrentSoC
	if st.VehicleCapacityWh > 0 {
		soc -= (st.DeliveredWhSession - wh) * loadpoint.DefaultChargeEfficiency / st.VehicleCapacityWh
	}
	return Level{SoC: units.ClampFraction(soc), Source: source, AtMs: atMs, LP: st.ID, SessionWh: wh,
		ChargeLimit: chargeLimit}
}

func statesByID(states []loadpoint.State) map[string]loadpoint.State {
	out := make(map[string]loadpoint.State, len(states))
	for _, st := range states {
		out[st.ID] = st
	}
	return out
}
