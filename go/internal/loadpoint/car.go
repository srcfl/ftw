package loadpoint

import (
	"time"

	"github.com/srcfl/ftw/go/internal/units"
)

// The car on a loadpoint (docs/cars.md). internal/cars owns the cars and
// decides which one a session belongs to; the loadpoint keeps the session's
// energy and turns the car's level into the estimate the planner and the
// controller read. These are the doors between the two.

// CarBinding is what the session's car brings to a loadpoint. A zero ID is a
// visitor while plugged in, and no car at all while unplugged.
type CarBinding struct {
	ID   string
	Name string
	// Session is the plug-in's id, IdentifiedBy how the box knows the car,
	// and Corrected/CorrectedFrom a better rule's correction. Choose is the
	// cars a tap may pick while the box asks, comma-separated so the
	// binding stays comparable.
	Session       string
	IdentifiedBy  string
	Corrected     bool
	CorrectedFrom string
	Choose        string
	// CapacityWh replaces the configured vehicle capacity while the binding
	// holds. Zero is config.yaml's vehicle_capacity_wh, or the default when
	// that is unset: a visitor's capacity is not known either.
	CapacityWh float64
	// MaxW is the most the car's own charger takes, zero when unknown.
	MaxW float64
	// Phases is the car's AC phase count, independently of the site wiring.
	Phases int
	// ChargeLimit is the car's own charge limit as the plan and the reserve
	// read it: only while the level is the car's own exact reading. Zero is
	// none.
	ChargeLimit float64
}

// LevelSeed starts a session's level from the car's.
type LevelSeed struct {
	// Known is false for a visitor: nobody knows its level, and the box
	// does not guess one.
	Known bool
	// SoC is the car's level when the session had delivered SessionWh.
	SoC       float64
	SessionWh float64
	// Confirmed is a level from the car or from someone who set it, not a
	// plug-in assumption. ConfirmedAt is when, for VehicleSoCSupersedes.
	Confirmed   bool
	ConfirmedAt time.Time
	// FromCar is a reading the car itself made.
	FromCar bool
}

// SetCarBinding names the session's car. The level is kept: a new car's
// level comes through SeedSessionLevel. False for an unknown loadpoint.
func (m *Manager) SetCarBinding(id string, b CarBinding) bool {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.mu.Lock()
	lp := m.byID[id]
	if lp == nil {
		m.mu.Unlock()
		return false
	}
	if lp.car.Phases != b.Phases {
		// The old count cannot fund a rise on the newly declared phases.
		lp.commandedPhases = 0
	}
	lp.car = b
	capacity := b.CapacityWh
	if capacity <= 0 {
		capacity = lp.configCapacityWh
	}
	persist := false
	switch {
	case lp.capacityFromCar:
		// The car measured its own capacity this session, which outranks
		// what the owner wrote down. Plug-out restores the car's figure.
		lp.baseCapacityWh = capacity
	case lp.VehicleCapacityWh != capacity:
		persist = setCapacityLocked(lp, capacity)
	}
	lp.updatedAtMs = m.now().UnixMilli()
	m.mu.Unlock()
	if persist {
		m.persistSession(id)
	}
	return true
}

// setCapacityLocked changes how delivered energy turns into a level, and
// keeps the level the session shows now, as a config reload does.
func setCapacityLocked(lp *loadpointRuntime, capacityWh float64) bool {
	lp.VehicleCapacityWh = capacityWh
	if !lp.pluggedIn {
		return false
	}
	delivered := 0.0
	if capacityWh > 0 {
		delivered = lp.deliveredWhSession * DefaultChargeEfficiency / capacityWh
	}
	lp.sessionPluginSoC = lp.currentSoC - delivered
	lp.currentSoC = estimateSoC(lp.sessionPluginSoC, lp.deliveredWhSession, capacityWh)
	return lp.socConfirmed
}

// SeedSessionLevel starts the session's level from the car's. False when the
// loadpoint is unknown or unplugged.
func (m *Manager) SeedSessionLevel(id string, seed LevelSeed) bool {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.mu.Lock()
	lp := m.byID[id]
	if lp == nil || !lp.pluggedIn || !finite(seed.SoC) || !finite(seed.SessionWh) {
		m.mu.Unlock()
		return false
	}
	if !seed.Known {
		// Counted from empty: the planner then sizes a goal for a full
		// battery, and the car stops charging when it is full.
		lp.sessionPluginSoC = 0
		lp.socConfirmed = false
		lp.socConfirmedAt = time.Time{}
		lp.socUnknown = true
		lp.vehicleAnchored = false
	} else {
		delivered := 0.0
		if lp.VehicleCapacityWh > 0 {
			delivered = seed.SessionWh * DefaultChargeEfficiency / lp.VehicleCapacityWh
		}
		lp.sessionPluginSoC = units.ClampFraction(seed.SoC) - delivered
		lp.socConfirmed = seed.Confirmed
		lp.socConfirmedAt = time.Time{}
		if seed.Confirmed {
			lp.socConfirmedAt = seed.ConfirmedAt
		}
		lp.socUnknown = false
		lp.vehicleAnchored = seed.FromCar
	}
	lp.currentSoC = estimateSoC(lp.sessionPluginSoC, lp.deliveredWhSession, lp.VehicleCapacityWh)
	lp.updatedAtMs = m.now().UnixMilli()
	confirmed := lp.socConfirmed
	m.mu.Unlock()
	if confirmed {
		m.persistSession(id)
	}
	return true
}

// DeliveredAt is the session's energy at t, as a car reading made then is
// anchored. False when the loadpoint is unplugged or the session cannot
// place t.
func (m *Manager) DeliveredAt(id string, t time.Time) (float64, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lp := m.byID[id]
	if lp == nil || !lp.pluggedIn {
		return 0, false
	}
	return lp.deliveredAt(t)
}

// ApplySchedule sets the goal the car brings, as SetScheduleChecked does but
// without the schedule saver: the car's own record already holds it.
func (m *Manager) ApplySchedule(id string, s Schedule) bool {
	ok, _ := m.setSchedule(id, s, false)
	return ok
}

// ApplyCarGoal sets the goal the car brings and keeps the site's battery
// rule the loadpoint holds now, read under the same lock that a schedule
// save takes, so a save in between is never undone. A goal the loadpoint
// already runs is left alone. same compares the owner's choices.
func (m *Manager) ApplyCarGoal(id string, goal Schedule, same func(a, b Schedule) bool) bool {
	ok, _ := m.setScheduleWith(id, func(cur Schedule) (Schedule, bool) {
		if same(cur, goal) {
			return cur, false
		}
		goal.SurplusUnlockBatSoC = cur.SurplusUnlockBatSoC
		return goal, true
	})
	return ok
}

// ApplySurplusOnly sets the car's surplus-only choice without the saver.
func (m *Manager) ApplySurplusOnly(id string, v bool) bool {
	m.intentMu.Lock()
	defer m.intentMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	lp := m.byID[id]
	if lp == nil {
		return false
	}
	lp.Config.SurplusOnly = v
	return true
}

// SetLevelObserver hears every level someone sets by hand, so the session's
// car keeps it. Called outside the manager's locks.
func (m *Manager) SetLevelObserver(fn func(id string, soc float64, at time.Time)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.levelObserver = fn
}
