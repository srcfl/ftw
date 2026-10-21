package cars

import (
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/units"
	"github.com/srcfl/ftw/go/protocol"
)

// The operations (docs/cars.md, Ops). Each changes the record under the lock,
// saves it, and then hands the loadpoints what changed.

// Update is car.update's arguments: a nil field is left alone.
type Update struct {
	// NewID re-keys a car the box minted to the id the owner's app holds.
	NewID       *string
	Name        *string
	Make        *string
	VIN         *string
	CapacityWh  *float64
	MaxW        *float64
	Phases      *int
	Identifiers *[]string
	Sources     *[]string
}

// Add makes a car with the id the owner's app minted. An id the box already
// has is an update of that car. A car the box minted with the same VIN takes
// the app's id. A VIN or identifier another car has is refused.
func (r *Runtime) Add(c protocol.Car) (protocol.Car, error) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	id := strings.TrimSpace(c.ID)
	if !validID(id) {
		return protocol.Car{}, fmt.Errorf("%w: id", ErrInvalid)
	}
	u := updateFrom(c)
	var goal *loadpoint.Schedule
	if c.Goal != nil {
		g, err := scheduleFrom(*c.Goal)
		if err != nil {
			return protocol.Car{}, err
		}
		goal = &g
	}
	r.mu.Lock()
	car := r.carLocked(id)
	// adopt is a car the box minted with this VIN, which takes the app's id
	// once the rest of the car checks out.
	var adopt *Car
	if car == nil {
		if vin := normVIN(c.VIN); vin != "" {
			for _, other := range r.rec.Cars {
				if other.BoxMinted && other.VIN == vin {
					adopt = other
					break
				}
			}
		}
	}
	base := car
	if base == nil {
		base = adopt
	}
	next := &Car{ID: id}
	if base != nil {
		cp := *base
		cp.ID = id
		cp.Identifiers, cp.Sources = slices.Clone(base.Identifiers), slices.Clone(base.Sources)
		next = &cp
	} else if strings.TrimSpace(c.Name) == "" {
		r.mu.Unlock()
		return protocol.Car{}, fmt.Errorf("%w: name", ErrInvalid)
	} else if len(r.rec.Cars) >= MaxCars {
		r.mu.Unlock()
		return protocol.Car{}, ErrTooMany
	}
	if err := applyUpdate(next, u); err != nil {
		r.mu.Unlock()
		return protocol.Car{}, err
	}
	if goal != nil {
		next.Goal = *goal
	}
	next.SurplusOnly = c.SurplusOnly
	if err := r.duplicateLocked(next, base); err != nil {
		r.mu.Unlock()
		return protocol.Car{}, err
	}
	switch {
	case car != nil:
		*car = *next
	case adopt != nil:
		r.rekeyLocked(adopt, id)
		next.BoxMinted = false
		*adopt = *next
		car = adopt
	default:
		car = next
		r.rec.Cars = append(r.rec.Cars, car)
	}
	r.rec.Removed = slices.DeleteFunc(r.rec.Removed, func(t Tombstone) bool { return t.ID == id })
	r.takeSourcesLocked(car)
	return r.commit("car_added", car)
}

// UpdateCar changes what the box knows about a car. new_id re-keys a car the
// box minted to the id the owner's app holds.
func (r *Runtime) UpdateCar(id string, u Update) (protocol.Car, error) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	r.mu.Lock()
	car := r.carLocked(id)
	if car == nil {
		r.mu.Unlock()
		return protocol.Car{}, ErrUnknownCar
	}
	rekey := u.NewID != nil && *u.NewID != id
	if rekey {
		var err error
		switch {
		case !car.BoxMinted:
			err = fmt.Errorf("%w: only a car the box minted takes a new id", ErrInvalid)
		case !validID(*u.NewID):
			err = fmt.Errorf("%w: new_id", ErrInvalid)
		case r.carLocked(*u.NewID) != nil:
			err = fmt.Errorf("%w: new_id is another car's", ErrInvalid)
		}
		if err != nil {
			r.mu.Unlock()
			return protocol.Car{}, err
		}
	}
	next := *car
	next.Identifiers = slices.Clone(car.Identifiers)
	next.Sources = slices.Clone(car.Sources)
	if err := applyUpdate(&next, u); err != nil {
		r.mu.Unlock()
		return protocol.Car{}, err
	}
	if err := r.duplicateLocked(&next, car); err != nil {
		r.mu.Unlock()
		return protocol.Car{}, err
	}
	if u.Sources != nil {
		// A driver the owner takes away stays away.
		for _, d := range car.Sources {
			if !slices.Contains(next.Sources, d) && !slices.Contains(r.rec.Unbound, d) {
				r.rec.Unbound = append(r.rec.Unbound, d)
			}
		}
	}
	*car = next
	if rekey {
		r.rekeyLocked(car, *u.NewID)
	}
	r.takeSourcesLocked(car)
	return r.commit("car_updated", car)
}

// rekeyLocked gives a car the box minted the id the owner's app holds, and
// moves every reference to it.
func (r *Runtime) rekeyLocked(car *Car, id string) {
	old := car.ID
	car.ID, car.BoxMinted = id, false
	for _, s := range r.rec.Sessions {
		if s.CarID == old {
			s.CarID = id
		}
		if s.CorrectedFrom == old {
			s.CorrectedFrom = id
		}
	}
	for lp, c := range r.rec.LastCar {
		if c == old {
			r.rec.LastCar[lp] = id
		}
	}
	for p, c := range r.rec.Profiles {
		if c == old {
			r.rec.Profiles[p] = id
		}
	}
	// The app's id is a live car again, whatever it was before.
	r.rec.Removed = slices.DeleteFunc(r.rec.Removed, func(t Tombstone) bool { return t.ID == id })
	clear(r.applied)
	slog.Info("cars: a car the box minted took the app's id", "from", old, "to", id)
}

// duplicateLocked refuses a VIN or identifier another car has. self is the
// car being changed, which may still have its old id.
func (r *Runtime) duplicateLocked(car, self *Car) error {
	for _, other := range r.rec.Cars {
		if other.ID == car.ID || other == self {
			continue
		}
		if car.VIN != "" && other.VIN == car.VIN {
			return fmt.Errorf("%w: vin", ErrDuplicate)
		}
		for _, id := range car.Identifiers {
			for _, o := range other.Identifiers {
				if normIdentifier(id) == normIdentifier(o) {
					return fmt.Errorf("%w: identifier", ErrDuplicate)
				}
			}
		}
	}
	return nil
}

// takeSourcesLocked keeps each source on one car: the one just changed.
func (r *Runtime) takeSourcesLocked(car *Car) {
	for _, d := range car.Sources {
		r.rec.Unbound = slices.DeleteFunc(r.rec.Unbound, func(u string) bool { return u == d })
		for _, other := range r.rec.Cars {
			if other != car {
				other.Sources = slices.DeleteFunc(other.Sources, func(s string) bool { return s == d })
			}
		}
	}
}

// Remove takes a car off the box. A session on it becomes a visitor's.
func (r *Runtime) Remove(id string) error {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	r.mu.Lock()
	car := r.carLocked(id)
	if car == nil {
		r.mu.Unlock()
		return ErrUnknownCar
	}
	r.rec.Cars = slices.DeleteFunc(r.rec.Cars, func(c *Car) bool { return c.ID == id })
	r.rec.Removed = append(r.rec.Removed, Tombstone{ID: id, AtMs: r.deps.Now().UnixMilli()})
	if n := len(r.rec.Removed); n > maxTombstones {
		r.rec.Removed = slices.Delete(r.rec.Removed, 0, n-maxTombstones)
	}
	// Its drivers stay off, as when the owner takes one off a car: the box
	// does not hand them to the car that is left.
	for _, d := range car.Sources {
		if !slices.Contains(r.rec.Unbound, d) {
			r.rec.Unbound = append(r.rec.Unbound, d)
		}
	}
	for _, s := range r.rec.Sessions {
		if s.CarID == id {
			// The owner's word: a visitor's, which only the charger or the
			// car can correct, never the only car left.
			s.CarID, s.By, s.CorrectedFrom = "", protocol.CarIdentifiedByNone, ""
			s.seeded = false
			s.Asked, s.Tapped = true, true
		}
	}
	for lp, c := range r.rec.LastCar {
		if c == id {
			delete(r.rec.LastCar, lp)
		}
	}
	for p, c := range r.rec.Profiles {
		if c == id {
			// The profile stays seen: removing the car is the owner's word.
			r.rec.Profiles[p] = ""
		}
	}
	_, err := r.commit("car_removed", nil)
	return err
}

// SetGoal is car.goal.set. A nil goal with clear set removes the goal; a nil
// surplusOnly leaves the choice alone.
func (r *Runtime) SetGoal(id string, goal *protocol.CarGoal, clear bool, surplusOnly *bool) (protocol.Car, error) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	var next loadpoint.Schedule
	if goal != nil {
		var err error
		if next, err = scheduleFrom(*goal); err != nil {
			return protocol.Car{}, err
		}
	}
	r.mu.Lock()
	car := r.carLocked(id)
	if car == nil {
		r.mu.Unlock()
		return protocol.Car{}, ErrUnknownCar
	}
	if goal != nil || clear {
		car.Goal = next
	}
	if surplusOnly != nil {
		car.SurplusOnly = *surplusOnly
	}
	return r.commit("car_goal_changed", car)
}

// SetLevel is car.soc.set: a level someone set, now, as a fraction. A car on
// a charger re-anchors that session.
func (r *Runtime) SetLevel(id string, soc float64) (protocol.Car, error) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	if math.IsNaN(soc) || soc < 0 || soc > 1 {
		return protocol.Car{}, fmt.Errorf("%w: soc", ErrInvalid)
	}
	r.mu.Lock()
	car := r.carLocked(id)
	if car == nil {
		r.mu.Unlock()
		return protocol.Car{}, ErrUnknownCar
	}
	lpID, s := r.sessionOfLocked(id)
	if s == nil {
		car.Level = Level{SoC: soc, Source: protocol.CarSoCManual, AtMs: r.deps.Now().UnixMilli(), ChargeLimit: car.Level.ChargeLimit}
		return r.commit("car_level_set", car)
	}
	r.mu.Unlock()
	// The loadpoint's level observer records it on the car (LevelSet).
	if !r.deps.Loadpoints.SetCurrentSoC(lpID, soc) {
		return protocol.Car{}, ErrUnplugged
	}
	r.replan("car_level_set")
	r.notify()
	return r.Car(id)
}

// SetSessionGoal is car.goal.set with charger_id: the goal and surplus
// choice of the car on the charger, or of the session itself while its car
// is unknown, through the charger's own routes, which never touch a car the
// session does not have (docs/cars.md).
func (r *Runtime) SetSessionGoal(chargerID string, goal *protocol.CarGoal, clear bool, surplusOnly *bool) (protocol.ChargerCar, error) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	st, err := r.pluggedSession(chargerID)
	if err != nil {
		return protocol.ChargerCar{}, err
	}
	lps := r.deps.Loadpoints
	if goal != nil || clear {
		next := loadpoint.Schedule{}
		if goal != nil {
			g, err := scheduleFrom(*goal)
			if err != nil {
				return protocol.ChargerCar{}, err
			}
			next = g
		}
		next.SurplusUnlockBatSoC = st.Schedule.SurplusUnlockBatSoC
		if _, err := lps.SetScheduleChecked(chargerID, next); err != nil {
			return protocol.ChargerCar{}, err
		}
		if st.CarID == "" && surplusOnly == nil {
			// A visitor's goal ends its surplus-only charging (GoalSaved);
			// the loadpoint hears it now, before the replan.
			lps.ApplySurplusOnly(chargerID, !next.HasTarget())
		}
	}
	if surplusOnly != nil {
		if _, _, err := lps.SetSurplusOnlyChecked(chargerID, *surplusOnly); err != nil {
			return protocol.ChargerCar{}, err
		}
	}
	r.replan("session_goal_set")
	return r.ChargerCar(chargerID), nil
}

// SetSessionLevel is car.soc.set with charger_id: the level of the car on the
// charger, or of the session itself while its car is unknown.
func (r *Runtime) SetSessionLevel(chargerID string, soc float64) (protocol.ChargerCar, error) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	if math.IsNaN(soc) || soc < 0 || soc > 1 {
		return protocol.ChargerCar{}, fmt.Errorf("%w: soc", ErrInvalid)
	}
	if _, err := r.pluggedSession(chargerID); err != nil {
		return protocol.ChargerCar{}, err
	}
	if !r.deps.Loadpoints.SetCurrentSoC(chargerID, soc) {
		return protocol.ChargerCar{}, ErrUnplugged
	}
	r.replan("session_level_set")
	r.notify()
	return r.ChargerCar(chargerID), nil
}

func (r *Runtime) pluggedSession(chargerID string) (loadpoint.State, error) {
	st, ok := r.deps.Loadpoints.State(chargerID)
	if !ok {
		return st, ErrUnknownCharger
	}
	if !st.PluggedIn {
		return st, ErrUnplugged
	}
	return st, nil
}

// SetSessionCar is session.car.set, the one tap. sessionID names the plug-in
// the tap answers; a tap for one that has ended is refused. An empty carID
// makes the session a visitor's. A tap over an answer the box found by itself
// is final.
func (r *Runtime) SetSessionCar(lpID, sessionID, carID string) (protocol.ChargerCar, error) {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	st, err := r.pluggedSession(lpID)
	if err != nil {
		return protocol.ChargerCar{}, err
	}
	r.mu.Lock()
	s := r.rec.Sessions[lpID]
	if s == nil {
		// Plugged in since the last tick: open the session now.
		s = r.openSessionLocked(st, r.deps.Now())
	}
	if sessionID != "" && sessionID != s.ID {
		r.mu.Unlock()
		return protocol.ChargerCar{}, ErrSessionEnded
	}
	if carID != "" && r.carLocked(carID) == nil {
		r.mu.Unlock()
		return protocol.ChargerCar{}, ErrUnknownCar
	}
	if carID != "" && r.onOtherChargerLocked(carID, lpID) {
		// One car is on one charger. The other charger's session lets go
		// of it at its unplug, or when someone names its car.
		r.mu.Unlock()
		return protocol.ChargerCar{}, ErrElsewhere
	}
	// The box's own answer, a visitor's included, makes the tap final: a
	// rule that answered once would otherwise undo it at the next tick.
	automatic := s.By == protocol.CarIdentifiedByCharger || s.By == protocol.CarIdentifiedByCar ||
		s.By == protocol.CarIdentifiedByOnly || (s.By == protocol.CarIdentifiedByNone && s.Asked && !s.Tapped)
	by := protocol.CarIdentifiedByTap
	if carID == "" {
		by = protocol.CarIdentifiedByNone
	}
	if carID != s.CarID || s.By != by {
		r.bindLocked(lpID, s, carID, by, r.deps.Now())
	}
	s.Tapped, s.TapFinal, s.Asked = true, s.TapFinal || automatic, true
	if _, err := r.commit("session_car_set", nil); err != nil {
		return protocol.ChargerCar{}, err
	}
	return r.ChargerCar(lpID), nil
}

// commit saves the record, hands the loadpoints what changed and tells the
// listeners. It is called with the lock held and releases it.
func (r *Runtime) commit(reason string, car *Car) (protocol.Car, error) {
	r.dirty = true
	err := r.saveLocked()
	if err != nil {
		r.mu.Unlock()
		return protocol.Car{}, err
	}
	now := r.deps.Now()
	acts := r.syncLocked(now)
	var out protocol.Car
	if car != nil {
		out = r.viewLocked(car, statesByID(r.deps.Loadpoints.States()))
	}
	r.mu.Unlock()
	r.applyAndReplan(acts, reason)
	r.notify()
	return out, nil
}

// ChargerIdentified is the charger naming the car on it: an OCPP idTag, MAC
// address or eMAID, at the start of each transaction. It often arrives before
// the loadpoint has seen the plug-in, and then waits for it. An identifier no
// car lists makes the session a visitor's when some car lists identifiers;
// while none does, it names nobody.
func (r *Runtime) ChargerIdentified(lpID, identifier string) {
	now := r.deps.Now()
	r.mu.Lock()
	s := r.rec.Sessions[lpID]
	if s == nil {
		r.pendingIdent[lpID] = pendingIdent{ident: identifier, at: now}
	} else if s.chargerIdent != identifier {
		s.chargerIdent, s.chargerIdentAt = identifier, now
	}
	unknown := r.carWithIdentifierLocked(identifier) == nil && r.anyIdentifiersLocked()
	r.mu.Unlock()
	if unknown {
		// The identifier itself stays out of the log: it names a person's
		// card or car.
		slog.Info("cars: the charger named a car no car lists; the session is a visitor's", "lp", lpID)
	}
}

// ChargerReportedLevel is a level the car sent through the charger (OCPP
// 2.0.1 NotifyEVChargingNeeds): a reading from the car. The loadpoint has
// already anchored it.
func (r *Runtime) ChargerReportedLevel(lpID string, soc float64) {
	if math.IsNaN(soc) || soc < 0 || soc > 1 {
		return
	}
	now := r.deps.Now()
	r.mu.Lock()
	s := r.rec.Sessions[lpID]
	car := (*Car)(nil)
	if s != nil {
		car = r.carLocked(s.CarID)
	}
	if car == nil {
		r.mu.Unlock()
		return
	}
	wh := 0.0
	if st, ok := r.deps.Loadpoints.State(lpID); ok {
		wh = st.DeliveredWhSession
	}
	car.Level = Level{SoC: soc, Source: protocol.CarSoCVehicle, AtMs: now.UnixMilli(), LP: lpID, SessionWh: wh,
		ChargeLimit: car.Level.ChargeLimit}
	r.levelSave = true
	r.mu.Unlock()
	r.notify()
}

// LevelSet is the loadpoint's level observer: a level set by hand on a
// charger becomes its car's. A visitor's stays with the session.
func (r *Runtime) LevelSet(lpID string, soc float64, at time.Time) {
	r.mu.Lock()
	s := r.rec.Sessions[lpID]
	if s == nil {
		r.mu.Unlock()
		return
	}
	car := r.carLocked(s.CarID)
	if car == nil {
		r.mu.Unlock()
		return
	}
	wh := 0.0
	if st, ok := r.deps.Loadpoints.State(lpID); ok {
		wh = st.DeliveredWhSession
	}
	car.Level = Level{SoC: soc, Source: protocol.CarSoCManual, AtMs: at.UnixMilli(), LP: lpID, SessionWh: wh,
		ChargeLimit: car.Level.ChargeLimit}
	r.dirty = true
	if err := r.saveLocked(); err != nil {
		slog.Warn("cars: could not save a level someone set; kept in memory", "err", err)
	}
	r.mu.Unlock()
	r.notify()
}

// GoalSaved is the loadpoint schedule saver's: a goal set on a charger
// becomes the goal of the car the charger shows. It returns the schedule to
// write to the loadpoint's own row, nil for none. A visitor's goal stays in
// memory with its session, and the row keeps the car's.
func (r *Runtime) GoalSaved(lpID string, s loadpoint.Schedule) (*loadpoint.Schedule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return &s, nil
	}
	r.sessionForRouteLocked(lpID)
	car, visitor := r.viewCarLocked(lpID)
	switch {
	case visitor:
		goal := goalOnly(s)
		sess := r.rec.Sessions[lpID]
		sess.goal = &goal
		// Someone who sets a visitor's goal wants it met: the session no
		// longer charges from surplus alone, and plans from empty. With the
		// goal gone it charges from surplus again.
		sess.surplus = nil
		if goal.HasTarget() {
			off := false
			sess.surplus = &off
		}
		row := r.rowFor(lpID, s)
		return row, nil
	case car == nil:
		return &s, nil
	}
	car.Goal = goalOnly(s)
	r.dirty = true
	if err := r.saveLocked(); err != nil {
		return nil, err
	}
	return &s, nil
}

// rowFor is what a loadpoint's own row holds while a visitor sets a goal:
// the goal of the car it last had, and the site rule as just set.
func (r *Runtime) rowFor(lpID string, s loadpoint.Schedule) *loadpoint.Schedule {
	row := loadpoint.Schedule{}
	if c := r.carLocked(r.rec.LastCar[lpID]); c != nil {
		row = c.Goal
	} else if len(r.rec.Cars) == 1 {
		row = r.rec.Cars[0].Goal
	}
	row.SurplusUnlockBatSoC = s.SurplusUnlockBatSoC
	return &row
}

// SurplusSaved is the loadpoint surplus saver's: the choice is the car's. It
// reports whether the loadpoint's own row should be written too.
func (r *Runtime) SurplusSaved(lpID string, v bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return true, nil
	}
	r.sessionForRouteLocked(lpID)
	car, visitor := r.viewCarLocked(lpID)
	switch {
	case visitor:
		r.rec.Sessions[lpID].surplus = &v
		return false, nil
	case car == nil:
		return true, nil
	}
	car.SurplusOnly = v
	r.dirty = true
	if err := r.saveLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func updateFrom(c protocol.Car) Update {
	u := Update{Name: &c.Name}
	if c.Make != "" {
		u.Make = &c.Make
	}
	if c.VIN != "" {
		u.VIN = &c.VIN
	}
	if c.CapacityWh > 0 {
		capacity := float64(c.CapacityWh)
		u.CapacityWh = &capacity
	}
	if c.MaxW > 0 {
		maxW := float64(c.MaxW)
		u.MaxW = &maxW
	}
	if c.Phases > 0 {
		phases := int(c.Phases)
		u.Phases = &phases
	}
	if c.Identifiers != nil {
		u.Identifiers = &c.Identifiers
	}
	if c.Sources != nil {
		u.Sources = &c.Sources
	}
	return u
}

// applyUpdate checks each value as the HTTP capacity route does.
func applyUpdate(car *Car, u Update) error {
	if u.Name != nil {
		name := strings.TrimSpace(*u.Name)
		if name == "" || len(name) > 64 {
			return fmt.Errorf("%w: name", ErrInvalid)
		}
		car.Name = name
	}
	if u.Make != nil {
		car.Make = strings.TrimSpace(*u.Make)
	}
	if u.VIN != nil {
		vin := normVIN(*u.VIN)
		if vin == "" && strings.TrimSpace(*u.VIN) != "" {
			return fmt.Errorf("%w: vin", ErrInvalid)
		}
		car.VIN = vin
	}
	if u.CapacityWh != nil {
		v := *u.CapacityWh
		if math.IsNaN(v) || v < 1000 || v > 300000 {
			return fmt.Errorf("%w: capacityWh", ErrInvalid)
		}
		car.CapacityWh = v
	}
	if u.MaxW != nil {
		v := *u.MaxW
		if math.IsNaN(v) || v < 0 || v > 400000 {
			return fmt.Errorf("%w: maxW", ErrInvalid)
		}
		car.MaxW = v
	}
	if u.Phases != nil {
		if p := *u.Phases; p < 0 || p > 3 {
			return fmt.Errorf("%w: phases", ErrInvalid)
		} else {
			car.Phases = p
		}
	}
	if u.Identifiers != nil {
		ids, err := textList(*u.Identifiers, "identifiers")
		if err != nil {
			return err
		}
		car.Identifiers = ids
	}
	if u.Sources != nil {
		sources, err := textList(*u.Sources, "sources")
		if err != nil {
			return err
		}
		car.Sources = sources
	}
	return nil
}

// textList trims and de-duplicates identifiers or sources, at most
// maxListLen of at most maxTextLen characters each.
func textList(in []string, what string) ([]string, error) {
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || slices.Contains(out, v) {
			continue
		}
		if len(v) > maxTextLen || len(out) == maxListLen {
			return nil, fmt.Errorf("%w: %s", ErrInvalid, what)
		}
		out = append(out, v)
	}
	return out, nil
}

// A car's identifiers and sources are few and short.
const (
	maxListLen = 8
	maxTextLen = 64
)

// scheduleFrom turns a wire goal into the schedule a loadpoint runs.
func scheduleFrom(g protocol.CarGoal) (loadpoint.Schedule, error) {
	if g.TargetPermille < 0 || g.TargetPermille > 1000 || g.ReadyByMin < 0 || g.ReadyByMin >= 24*60 {
		return loadpoint.Schedule{}, fmt.Errorf("%w: goal", ErrInvalid)
	}
	days := g.Days & 0x7F
	recurring := days != 0
	if days == 0x7F {
		days = 0
	}
	return loadpoint.NewSchedule(float64(g.TargetPermille)/1000, int(g.ReadyByMin), recurring, days, g.FinishAtLimit), nil
}

// carGoal is a schedule as the wire carries it.
func carGoal(s loadpoint.Schedule) *protocol.CarGoal {
	if emptyGoal(s) {
		return nil
	}
	clock, ok := s.TimeOfDayMin()
	if !ok {
		clock = s.TimeOfDayMinUTC
	}
	g := &protocol.CarGoal{
		TargetPermille: units.PermilleFromFraction(s.SoC),
		ReadyByMin:     int64(clock),
		FinishAtLimit:  s.FinishAtVehicleLimit,
	}
	if s.Recurring {
		g.Days = s.Days
		if g.Days == 0 {
			g.Days = 0x7F
		}
	}
	return g
}

func normIdentifier(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// SetViewCarCapacity is POST /api/loadpoints/{id}/vehicle for one release: the
// battery size of the car the charger shows. A visitor's session has no car
// to change, and the charger keeps the default.
func (r *Runtime) SetViewCarCapacity(lpID string, capacityWh float64) error {
	r.syncMu.Lock()
	defer r.syncMu.Unlock()
	if _, ok := r.deps.Loadpoints.State(lpID); !ok {
		return nil
	}
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return nil
	}
	r.sessionForRouteLocked(lpID)
	car, _ := r.viewCarLocked(lpID)
	if car == nil {
		r.mu.Unlock()
		return nil
	}
	if err := applyUpdate(car, Update{CapacityWh: &capacityWh}); err != nil {
		r.mu.Unlock()
		return err
	}
	_, err := r.commit("car_capacity_changed", car)
	return err
}
