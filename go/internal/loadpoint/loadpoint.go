// Package loadpoint models an EV charge point as a first-class entity
// the planner can reason about. A loadpoint couples a physical charger
// driver (Easee, Zap, …) with a specific vehicle and user intent
// (target SoC by target time).
//
// The package owns loadpoint configuration, schedules, live state and
// dispatch control. MPC consumes per-loadpoint planning specs and returns
// energy budgets; the controller translates those budgets into commands.
// Protocol details remain in drivers.
package loadpoint

import (
	"sort"
	"sync"
	"time"
)

// DeliveringW is the current_power_w threshold above which we treat a
// loadpoint as actively delivering power to a vehicle. Easee's minimum
// step is ~1380 W (1Φ 6 A); 100 W gives margin against settling noise
// on session start/stop without ever crossing into legitimate charging
// territory.
//
// Centralised so the MPC plumbing in main.go, the API decoration in
// internal/api, and any future consumers all gate the same way.
const DeliveringW = 100.0

// Config is the YAML-facing definition of one loadpoint. Wired into
// config.Config under "loadpoints". All electrical fields are
// optional with sensible defaults for a typical single-phase /
// three-phase residential EV charger.
type Config struct {
	ID         string `yaml:"id" json:"id"`                   // stable identifier ("garage", "street")
	DriverName string `yaml:"driver_name" json:"driver_name"` // which driver controls the charger

	// Elektriska gränser
	MinChargeW    float64   `yaml:"min_charge_w,omitempty" json:"min_charge_w,omitempty"`       // e.g. 1400 (1-phase 6 A)
	MaxChargeW    float64   `yaml:"max_charge_w,omitempty" json:"max_charge_w,omitempty"`       // e.g. 11000 (3-phase 16 A)
	AllowedStepsW []float64 `yaml:"allowed_steps_w,omitempty" json:"allowed_steps_w,omitempty"` // discrete Wh levels supported

	// Battery capacity in Wh (used to translate SoC% ↔ Wh and to
	// validate target-SoC feasibility given a deadline). 0 falls
	// back to a typical 60 kWh assumption.
	VehicleCapacityWh float64 `yaml:"vehicle_capacity_wh,omitempty" json:"vehicle_capacity_wh,omitempty"`

	// Assumed EV SoC % at plug-in. Chargers like Easee don't report
	// the vehicle's SoC directly — only cumulative session energy.
	// Current SoC is then estimated as `PluginSoCPct + delivered / cap`.
	// 0 defaults to 20 % (conservative). Operators who care can
	// override per-loadpoint or pre-plug-in.
	PluginSoCPct float64 `yaml:"plugin_soc_pct,omitempty" json:"plugin_soc_pct,omitempty"`

	// PhaseMode selects how the controller picks between 1Φ and 3Φ
	// delivery each tick. "3p" (default) and "1p" lock the install to
	// one mode and filter AllowedStepsW accordingly. "auto" lets the
	// controller switch based on the MPC budget, with a hysteresis
	// timer (MinPhaseHoldS) preventing flap. Empty == "3p" for
	// backward compatibility with pre-switching configs.
	PhaseMode string `yaml:"phase_mode,omitempty" json:"phase_mode,omitempty"`

	// PhaseSplitW is the wantW threshold below which "auto" picks 1Φ.
	// Zero defaults to 3680 W — the ceiling a 16 A fuse can sustain
	// on a single phase. Also used to classify AllowedStepsW entries:
	// steps ≤ split are 1Φ-eligible, > split are 3Φ-eligible.
	PhaseSplitW float64 `yaml:"phase_split_w,omitempty" json:"phase_split_w,omitempty"`

	// MinPhaseHoldS is the minimum dwell time before the controller
	// will flip phase again. Easee's cloud API + contactor transition
	// is not instantaneous (~5-10 s observed), and MPC slots can flap
	// across the split threshold on noisy wantW. Default 60 s.
	MinPhaseHoldS int `yaml:"min_phase_hold_s,omitempty" json:"min_phase_hold_s,omitempty"`

	// SurplusOnly forbids the loadpoint from drawing grid power: the EV
	// only charges from PV surplus (site-export). Enforced as a hard
	// constraint in the MPC DP (no action that turns site-export into
	// site-import is feasible) and as a live cap in the dispatch
	// controller (wantW clamped to the current site export). Implies
	// EV charging takes priority over battery surplus charging because
	// the deadline shortfall penalty outweighs the battery's terminal-
	// SoC credit when both compete for the same surplus.
	SurplusOnly bool `yaml:"surplus_only,omitempty" json:"surplus_only,omitempty"`
}

// SiteFuse describes the shared grid-boundary breaker in terms the
// loadpoint controller needs: max amps per phase (the rated trip
// current), nominal voltage, and number of phases at the service
// entrance. Zero MaxAmps disables the per-phase clamp — used by
// tests that don't care about the fuse and by sites without a
// configured fuse value.
type SiteFuse struct {
	MaxAmps  float64
	Voltage  float64
	PhaseCnt int // number of phases at the service entrance (1 or 3)
}

// PerPhaseMaxW is the maximum sustained power per phase under this
// fuse. 16 A @ 230 V = 3680 W. Multiply by phase count to get the
// total three-phase ceiling.
func (f SiteFuse) PerPhaseMaxW() float64 {
	v := f.Voltage
	if v <= 0 {
		v = 230
	}
	return f.MaxAmps * v
}

// Phases returns the total phase count at the service entrance,
// defaulting to 3 for backward compat with earlier callers that
// didn't pass the field explicitly.
func (f SiteFuse) Phases() int {
	if f.PhaseCnt <= 0 {
		return 3
	}
	return f.PhaseCnt
}

// State is the observable snapshot of one loadpoint at a point in time.
// Read-only for consumers — only the Manager or dispatch paths mutate
// it under lock.
type State struct {
	ID                 string    `json:"id"`
	DriverName         string    `json:"driver_name"`
	PluggedIn          bool      `json:"plugged_in"`
	CurrentSoCPct      float64   `json:"current_soc_pct"`       // observed or estimated
	CurrentPowerW      float64   `json:"current_power_w"`       // actual draw (site sign: + = charging)
	DeliveredWhSession float64   `json:"delivered_wh_session"`  // since plug-in
	TargetSoCPct       float64   `json:"target_soc_pct"`        // user intent
	TargetTime         time.Time `json:"target_time,omitempty"` // user intent
	UpdatedAtMs        int64     `json:"updated_at_ms"`

	// Vehicle-side telemetry, populated by the API layer from the most
	// recent DerVehicle reading whose charging_state indicates a likely
	// physical connection. Zero values when no online vehicle driver is
	// reporting. SoCSource is "vehicle" when CurrentSoCPct was overridden
	// from the car's BMS, "inferred" when it's the loadpoint manager's
	// pluginSoC + deliveredWh estimate, "" when not plugged in.
	VehicleSoCPct         float64 `json:"vehicle_soc_pct,omitempty"`
	VehicleChargeLimitPct float64 `json:"vehicle_charge_limit_pct,omitempty"`
	VehicleChargingState  string  `json:"vehicle_charging_state,omitempty"`
	VehicleDriver         string  `json:"vehicle_driver,omitempty"`
	VehicleStale          bool    `json:"vehicle_stale,omitempty"`
	SoCSource             string  `json:"soc_source,omitempty"`

	// MinChargeW / MaxChargeW / AllowedStepsW are repeated here so the
	// UI has everything for rendering in one fetch.
	MinChargeW    float64   `json:"min_charge_w"`
	MaxChargeW    float64   `json:"max_charge_w"`
	AllowedStepsW []float64 `json:"allowed_steps_w,omitempty"`

	// Phases / VoltageV let the UI convert between watts and amps for the
	// manual amp slider (A = W / (Phases × VoltageV)). Populated by the
	// API layer from the loadpoint's phase_mode and the site fuse voltage.
	Phases   int     `json:"phases,omitempty"`
	VoltageV float64 `json:"voltage_v,omitempty"`

	// ManualActive is true when an operator manual hold ("Start" / amp
	// slider) is pinned on this loadpoint, overriding surplus/plan.
	// ManualChargeW is the held setpoint in watts. Populated by the API
	// layer from the loadpoint controller.
	ManualActive  bool    `json:"manual_active"`
	ManualChargeW float64 `json:"manual_charge_w,omitempty"`

	// BatteryBoost is the explicit, bounded home-battery-to-EV permission
	// for this loadpoint. Populated by the API layer from Controller state.
	BatteryBoost BatteryBoostStatus `json:"battery_boost"`

	// SurplusOnly mirrors Config.SurplusOnly with any runtime override
	// (set via POST /api/loadpoints/{id}/target). Always emitted (no
	// omitempty) so a polling client can distinguish "explicitly off"
	// from "field absent because the server is too old to know".
	SurplusOnly bool `json:"surplus_only"`

	// Schedule is the operator's recurring intent. Empty when no
	// schedule is configured. Always emitted (object, possibly with
	// zero fields) so the UI can rely on a stable shape — clients
	// detect "no schedule" via Schedule.Empty() / soc_pct === 0.
	Schedule Schedule `json:"schedule"`
}

// Manager holds the running set of loadpoints. Thread-safe.
type Manager struct {
	mu    sync.RWMutex
	byID  map[string]*loadpointRuntime
	order []string // insertion-preserving id list for deterministic listing

	// scheduleSaver, if non-nil, is invoked synchronously whenever a
	// schedule is set or cleared. Wired by main.go to persist via
	// state.SaveConfig. Left nil in tests / sites without storage.
	scheduleSaver func(id string, s Schedule)

	// surplusOnlySaver, if non-nil, persists the runtime surplus_only
	// flag whenever an operator toggles it. Without this the flag
	// reverts to whatever's in YAML on every restart — operators were
	// finding that frustrating since the toggle lives in the dashboard
	// EV modal, not the YAML they'd think to edit. Same pattern as
	// scheduleSaver.
	surplusOnlySaver func(id string, v bool)

	// nowFn is the clock the manager uses for time-sensitive logic
	// (session-completion timer in particular). Defaults to time.Now;
	// tests inject a deterministic clock via SetNowFn.
	nowFn func() time.Time
}

// SessionCompletionTimeout is how long a vehicle must stay connected
// but explicitly not requesting current before the loadpoint treats
// the session as vehicle-side-complete. Tuned to swallow short bursts
// of retry-flap that some EVSEs emit while the vehicle holds steady
// at refusing (observed cycles in the ~10 s–90 s range) without
// snapping on a transient hiccup. Once tripped, the snap persists
// until the cable is unplugged.
const SessionCompletionTimeout = 90 * time.Second

// loadpointRuntime is the in-memory representation. Its fields are the
// union of configured parameters and observed state. Lives behind
// Manager so consumers access it via the public State snapshot.
type loadpointRuntime struct {
	Config

	pluggedIn          bool
	currentSoCPct      float64
	currentPowerW      float64
	deliveredWhSession float64
	targetSoCPct       float64
	targetTime         time.Time
	updatedAtMs        int64

	// Plug-in anchor: the SoC we believe the vehicle was at when
	// this session began. Persisted across Observe() calls so SoC
	// inference (pluginSoC + deliveredWh/capacity) stays stable
	// even as session_wh grows. Reset to Config.PluginSoCPct on
	// every plug-in transition (prev !pluggedIn → now pluggedIn).
	sessionPluginSoCPct float64

	// schedule carries the operator's persistent intent. Empty when
	// none is set. Survives config hot-reload because Load() copies
	// it across from the previous runtime row.
	schedule Schedule

	// lastRolledFor is the targetTime value that the most recent
	// RollSchedules promotion produced. Used to keep the roll
	// idempotent within a tick window — without this, a recurring
	// schedule re-promotes its own freshly-set targetTime back to
	// the day after that, racing the clock by 24 h every tick.
	lastRolledFor time.Time

	// notRequestingSince marks when the loadpoint first observed
	// "connected + vehicle not requesting current" on the current
	// session. Zero when the vehicle is requesting or unplugged.
	// Drives session-completion (see Observe).
	notRequestingSince time.Time

	// sessionComplete latches once the vehicle has held "not
	// requesting" past SessionCompletionTimeout for this session.
	// While set, the inferred SoC is pinned to targetSoCPct so the
	// MPC stops allocating PV surplus to a sink the vehicle has
	// already declined. Cleared on plug-out.
	sessionComplete bool

	// socSource, when non-empty, overrides the API layer's
	// vehicle-driver attribution in the State snapshot. Set to
	// "completed" when sessionComplete is latched so operators can
	// see why the inferred SoC pinned at target.
	socSource string

	// surplusWithheld is set by the controller each tick: true when WE
	// are intentionally withholding power from this loadpoint (a
	// surplus_only pause below the 3-phase floor). While set, a vehicle
	// reporting "not requesting current" is responding to our own pause,
	// not declining charge — so Observe must not count it toward session
	// completion. Without this a cloudy spell below the floor latches the
	// session done and the planner stops offering PV surplus for the rest
	// of the day. Transient per-tick; the controller refreshes it.
	surplusWithheld bool
}

// NewManager returns an empty manager. Configure with Load().
func NewManager() *Manager {
	return &Manager{byID: map[string]*loadpointRuntime{}}
}

// Load replaces the configured set. Idempotent: existing state is
// carried across when the ID is kept; removed IDs are dropped.
func (m *Manager) Load(cfgs []Config) {
	m.mu.Lock()
	defer m.mu.Unlock()

	newByID := make(map[string]*loadpointRuntime, len(cfgs))
	newOrder := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		if c.ID == "" {
			continue
		}
		lp := &loadpointRuntime{Config: c}
		if existing, ok := m.byID[c.ID]; ok {
			// Preserve observed state across reload. The session
			// plug-in anchor is carried too — otherwise a config
			// hot-reload during a charging session would drop our
			// SoC reference and reset the estimate back to
			// PluginSoCPct even though delivered_wh has grown.
			lp.pluggedIn = existing.pluggedIn
			lp.currentSoCPct = existing.currentSoCPct
			lp.currentPowerW = existing.currentPowerW
			lp.deliveredWhSession = existing.deliveredWhSession
			lp.targetSoCPct = existing.targetSoCPct
			lp.targetTime = existing.targetTime
			lp.updatedAtMs = existing.updatedAtMs
			lp.sessionPluginSoCPct = existing.sessionPluginSoCPct
			lp.schedule = existing.schedule
			lp.lastRolledFor = existing.lastRolledFor
			lp.notRequestingSince = existing.notRequestingSince
			lp.sessionComplete = existing.sessionComplete
			lp.socSource = existing.socSource
		}
		newByID[c.ID] = lp
		newOrder = append(newOrder, c.ID)
	}
	m.byID = newByID
	m.order = newOrder
}

// IDs returns configured loadpoint IDs in insertion order.
func (m *Manager) IDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.order))
	copy(out, m.order)
	return out
}

// State returns an immutable snapshot. Returns (State{}, false) when ID
// is unknown.
func (m *Manager) State(id string) (State, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lp, ok := m.byID[id]
	if !ok {
		return State{}, false
	}
	return lp.snapshot(), true
}

// States returns snapshots of every configured loadpoint, sorted by
// the configured ID order. Useful for GET /api/loadpoints.
func (m *Manager) States() []State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]State, 0, len(m.order))
	for _, id := range m.order {
		if lp, ok := m.byID[id]; ok {
			out = append(out, lp.snapshot())
		}
	}
	return out
}

// Configs returns a snapshot of the currently-configured loadpoints
// in insertion order. Used by Controller.Tick to drive dispatch
// without needing a second copy of the YAML source of truth — the
// manager is already the authoritative in-memory view after Load().
func (m *Manager) Configs() []Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Config, 0, len(m.order))
	for _, id := range m.order {
		if lp, ok := m.byID[id]; ok {
			out = append(out, lp.Config)
		}
	}
	return out
}

// Observe updates the measurement side of a loadpoint from raw driver
// telemetry. The manager derives current SoC internally from the
// session's plug-in anchor + delivered energy (chargers like Easee
// don't report the vehicle's actual SoC).
//
// Plug-in transitions (prev !pluggedIn → now pluggedIn) reset the
// session anchor to Config.PluginSoCPct (default 20 %) so the
// inference is stable across plug cycles even if the underlying
// charger's session counter wraps or resets.
//
// requestActive expresses whether the vehicle is (or could imminently
// be) drawing current. Drivers that can distinguish "we throttled to 0"
// from "the vehicle has explicitly stopped requesting current" pass
// false on the latter; drivers without that distinction always pass
// true and pre-existing behaviour is preserved. After
// SessionCompletionTimeout of sustained !requestActive on a connected
// session, the inferred SoC is pinned to targetSoCPct so the MPC stops
// allocating PV surplus to a sink the vehicle has already declined.
//
// No-op for unknown IDs — a misconfigured driver shouldn't crash the
// manager.
// SetSurplusWithheld records whether the controller is intentionally
// withholding power from this loadpoint this tick (a surplus_only pause below
// the 3-phase floor). When true, the next Observe treats a "not requesting
// current" report as self-induced and does not advance the session-completion
// timer. No-op for an unknown id.
func (m *Manager) SetSurplusWithheld(id string, withheld bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lp, ok := m.byID[id]; ok {
		lp.surplusWithheld = withheld
	}
}

func (m *Manager) Observe(id string, pluggedIn bool, powerW, deliveredWh float64, requestActive bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok {
		return
	}
	now := m.now()
	if pluggedIn && !lp.pluggedIn {
		// Plug-in transition: seed the session anchor and clear any
		// session-completion latched from a prior session.
		anchor := lp.PluginSoCPct
		if anchor <= 0 {
			anchor = 20 // conservative default
		}
		lp.sessionPluginSoCPct = anchor
		lp.notRequestingSince = time.Time{}
		lp.sessionComplete = false
		lp.socSource = ""
	}
	if !pluggedIn {
		// Plug-out: drop any pending completion timer / latch.
		lp.notRequestingSince = time.Time{}
		lp.sessionComplete = false
		lp.socSource = ""
	}
	lp.pluggedIn = pluggedIn
	lp.currentPowerW = powerW
	lp.deliveredWhSession = deliveredWh

	if pluggedIn && !requestActive && lp.surplusWithheld {
		// Self-induced "not requesting": we paused this surplus_only
		// loadpoint below its floor, so the vehicle dropping current is
		// our doing, not a vehicle-side decline. Do not start/advance the
		// completion timer — otherwise a sub-floor spell would latch the
		// session done and the planner would stop offering surplus all
		// day. Reset the clock so a genuine refusal (once we resume
		// offering power) is timed from a clean start.
		lp.notRequestingSince = time.Time{}
	} else if pluggedIn && !requestActive {
		// Vehicle has explicitly stopped requesting current while we ARE
		// offering power. Start (or continue) the completion timer; latch
		// once it elapses.
		if lp.notRequestingSince.IsZero() {
			lp.notRequestingSince = now
		}
		if !lp.sessionComplete && lp.targetSoCPct > 0 &&
			!lp.notRequestingSince.IsZero() &&
			now.Sub(lp.notRequestingSince) >= SessionCompletionTimeout {
			lp.sessionComplete = true
			lp.socSource = "completed"
		}
	} else if pluggedIn && requestActive {
		// Vehicle is back to requesting. Reset the timer, but keep
		// sessionComplete latched — once a vehicle has declined this
		// session, treating it as "still hungry" the moment an EVSE
		// retry briefly succeeds would reopen the export hole the
		// completion latch exists to close. Plug-cycle to reset.
		lp.notRequestingSince = time.Time{}
	}

	if pluggedIn {
		if lp.sessionComplete && lp.targetSoCPct > 0 {
			// Snap the inferred SoC to target; the planner reads
			// currentSoCPct as the MPC LoadpointSpec.InitialSoCPct,
			// so InitialSoCPct == TargetSoCPct → DP allocates 0 W.
			lp.currentSoCPct = lp.targetSoCPct
		} else {
			lp.currentSoCPct = estimateSoCPct(lp.sessionPluginSoCPct,
				deliveredWh, lp.VehicleCapacityWh)
		}
	} else {
		lp.currentSoCPct = 0
	}
	lp.updatedAtMs = now.UnixMilli()
}

// now returns the manager's clock, defaulting to time.Now when nowFn
// is unset. Tests inject a deterministic clock via SetNowFn.
func (m *Manager) now() time.Time {
	if m.nowFn != nil {
		return m.nowFn()
	}
	return time.Now()
}

// SetNowFn overrides the manager's clock. Tests use this to drive the
// session-completion timer deterministically. Pass nil to revert to
// time.Now.
func (m *Manager) SetNowFn(fn func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nowFn = fn
}

// estimateSoCPct returns the vehicle SoC % inferred from the session
// anchor + energy delivered. Chargers like Easee don't expose the
// car's BMS; this is the best-effort estimate the MPC uses.
//
// Clamps to [0, 100]. Falls back to the anchor when capacity is
// unknown (can't translate Wh → %).
func estimateSoCPct(pluginSoCPct, deliveredWh, capacityWh float64) float64 {
	if capacityWh <= 0 {
		return pluginSoCPct
	}
	soc := pluginSoCPct + deliveredWh/capacityWh*100.0
	if soc < 0 {
		return 0
	}
	if soc > 100 {
		return 100
	}
	return soc
}

// SetTarget updates the user-intent fields for an existing loadpoint.
// targetTime zero = no deadline. Returns false for unknown IDs.
func (m *Manager) SetTarget(id string, socPct float64, targetTime time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok {
		return false
	}
	if socPct < 0 {
		socPct = 0
	}
	if socPct > 100 {
		socPct = 100
	}
	lp.targetSoCPct = socPct
	lp.targetTime = targetTime
	return true
}

// SetSurplusOnly toggles the runtime surplus_only flag for a loadpoint.
// Mutates Config.SurplusOnly so subsequent Configs() calls reflect the
// new value (both the MPC LoadpointSpec builder in main.go and the
// dispatch controller read from there). Returns (previous, ok) so a
// caller can detect the transition direction — disabling surplus_only
// is a regime change for the planner (terminal credit flips back to
// the arbitrage default, the grid-charge ban lifts) and the API
// handler forces a tagged replan in that case.
func (m *Manager) SetSurplusOnly(id string, v bool) (prev bool, ok bool) {
	m.mu.Lock()
	lp, ok := m.byID[id]
	if !ok {
		m.mu.Unlock()
		return false, false
	}
	prev = lp.Config.SurplusOnly
	lp.Config.SurplusOnly = v
	saver := m.surplusOnlySaver
	m.mu.Unlock()
	if saver != nil && prev != v {
		saver(id, v)
	}
	return prev, true
}

// SetSurplusOnlySaver wires the persistence callback. Pass nil to
// disable. Mirrors SetScheduleSaver — the saver runs on every change
// (after the mutex is released, so the storage I/O isn't on the hot
// path).
func (m *Manager) SetSurplusOnlySaver(saver func(id string, v bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.surplusOnlySaver = saver
}

// HydrateSurplusOnly seeds the in-memory surplus_only flag from a
// per-LP loader at boot. Called once after Load; loader returns
// (value, true) when a persisted override exists and should win over
// the YAML default, (zero, false) otherwise. Matches the pattern used
// by HydrateSchedules.
func (m *Manager) HydrateSurplusOnly(load func(id string) (bool, bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, lp := range m.byID {
		if v, ok := load(id); ok {
			lp.Config.SurplusOnly = v
		}
	}
}

// SetCurrentSoC lets an operator correct the inferred vehicle SoC
// mid-session. Chargers like Easee don't report the vehicle's actual
// BMS state, so the manager defaults to
// `plugin_soc_pct + session_wh / capacity` — which drifts if the
// plug-in anchor was wrong. This resets the session anchor so the
// CURRENT estimate equals `socPct` and future observations accumulate
// from there. Only applies while plugged in; no-op otherwise.
//
// Returns false for unknown IDs or when the loadpoint is unplugged.
func (m *Manager) SetCurrentSoC(id string, socPct float64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok {
		return false
	}
	if !lp.pluggedIn {
		return false
	}
	reanchorSoCLocked(lp, socPct)
	return true
}

// AnchorVehicleSoC re-anchors the inferred SoC to a trusted vehicle BMS
// reading. It is the automatic counterpart to the operator's manual
// SetCurrentSoC: the control loop calls it every tick with the SoC from
// the vehicle driver paired to this loadpoint (e.g. Tesla via
// TeslaBLEProxy), so the dashboard's current_soc and the planner's
// InitialSoCPct both reflect BMS ground truth instead of the
// delivered-Wh estimate, which is blind to the real pack (Easee and
// other chargers can't read the car).
//
// Caller is responsible for the trust gate — only call with a reading
// that is online, fresh, and matched to this loadpoint
// (telemetry.PickBestVehicleForLoadpoint enforces this). Re-anchoring
// every tick keeps current_soc locked to the latest BMS value; between
// refreshes the inference advances from the last anchor on delivered Wh,
// and if the vehicle goes BLE-silent (caller stops anchoring) the
// estimate continues from the last known BMS truth rather than snapping
// back to the plug-in guess.
//
// Returns false for unknown IDs or when the loadpoint is unplugged.
func (m *Manager) AnchorVehicleSoC(id string, socPct float64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok {
		return false
	}
	if !lp.pluggedIn {
		return false
	}
	reanchorSoCLocked(lp, socPct)
	return true
}

// reanchorSoCLocked re-bases the session anchor so the CURRENT estimate
// equals socPct, then recomputes current_soc from it. Caller must hold
// m.mu and have verified the loadpoint is plugged in. Shared by the
// manual (SetCurrentSoC) and automatic (AnchorVehicleSoC) correction
// paths so they stay arithmetically identical.
func reanchorSoCLocked(lp *loadpointRuntime, socPct float64) {
	if socPct < 0 {
		socPct = 0
	}
	if socPct > 100 {
		socPct = 100
	}
	// Re-anchor: new_anchor + delivered/capacity*100 == socPct.
	// → new_anchor = socPct − delivered/capacity*100
	deliveredPct := 0.0
	if lp.VehicleCapacityWh > 0 {
		deliveredPct = lp.deliveredWhSession / lp.VehicleCapacityWh * 100.0
	}
	anchor := socPct - deliveredPct
	if anchor < 0 {
		anchor = 0
	}
	if anchor > 100 {
		anchor = 100
	}
	lp.sessionPluginSoCPct = anchor
	lp.currentSoCPct = estimateSoCPct(anchor, lp.deliveredWhSession, lp.VehicleCapacityWh)
	lp.updatedAtMs = time.Now().UnixMilli()
}

func (lp *loadpointRuntime) snapshot() State {
	steps := make([]float64, len(lp.AllowedStepsW))
	copy(steps, lp.AllowedStepsW)
	sort.Float64s(steps)
	return State{
		ID:                 lp.ID,
		DriverName:         lp.DriverName,
		PluggedIn:          lp.pluggedIn,
		CurrentSoCPct:      lp.currentSoCPct,
		CurrentPowerW:      lp.currentPowerW,
		DeliveredWhSession: lp.deliveredWhSession,
		TargetSoCPct:       lp.targetSoCPct,
		TargetTime:         lp.targetTime,
		UpdatedAtMs:        lp.updatedAtMs,
		MinChargeW:         lp.MinChargeW,
		MaxChargeW:         lp.MaxChargeW,
		AllowedStepsW:      steps,
		SurplusOnly:        lp.Config.SurplusOnly,
		Schedule:           lp.schedule,
		SoCSource:          lp.socSource,
	}
}

// SetScheduleSaver wires the persistence callback. Pass nil to disable.
// Safe to call before or after Load().
func (m *Manager) SetScheduleSaver(saver func(id string, s Schedule)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scheduleSaver = saver
}

// SetSchedule stores the operator's intent for a loadpoint. Empty
// schedules clear (equivalent to ClearSchedule). Returns false for
// unknown IDs. The persistence callback (if wired) is invoked outside
// the lock so a slow disk doesn't block other readers.
func (m *Manager) SetSchedule(id string, s Schedule) bool {
	m.mu.Lock()
	lp, ok := m.byID[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	if s.SoCPct < 0 {
		s.SoCPct = 0
	}
	if s.SoCPct > 100 {
		s.SoCPct = 100
	}
	if s.SurplusUnlockBatSoCPct < 0 {
		s.SurplusUnlockBatSoCPct = 0
	}
	if s.SurplusUnlockBatSoCPct > 100 {
		s.SurplusUnlockBatSoCPct = 100
	}
	lp.schedule = s
	// Force RollSchedules to re-evaluate on next call — operator just
	// changed the contract so any previous idempotence cache is stale.
	lp.lastRolledFor = time.Time{}
	// Clear the one-shot targetTime too. RollSchedules deliberately
	// preserves a future targetTime (so daily-rolled deadlines stay
	// stable across ticks), but that same guard makes a freshly-saved
	// schedule a no-op when a stale future targetTime is sitting in
	// state.db — the operator presses Save and nothing changes. The
	// contract for SetSchedule is "this replaces the user's intent",
	// so we wipe the derived field and let the next RollSchedules
	// seed it from the new schedule. Applies to both recurring and
	// non-recurring saves.
	lp.targetTime = time.Time{}
	lp.targetSoCPct = 0
	saver := m.scheduleSaver
	m.mu.Unlock()
	if saver != nil {
		saver(id, s)
	}
	return true
}

// GetSchedule returns the current schedule and a found flag. The flag
// is true only when an Empty()=false schedule is set for the ID — an
// empty schedule is reported as "not configured".
func (m *Manager) GetSchedule(id string) (Schedule, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lp, ok := m.byID[id]
	if !ok {
		return Schedule{}, false
	}
	if lp.schedule.Empty() {
		return Schedule{}, false
	}
	return lp.schedule, true
}

// ClearSchedule removes the operator's intent. Persists Empty so a
// reload doesn't resurrect the old schedule from disk. Returns false
// for unknown IDs.
func (m *Manager) ClearSchedule(id string) bool {
	m.mu.Lock()
	lp, ok := m.byID[id]
	if !ok {
		m.mu.Unlock()
		return false
	}
	lp.schedule = Schedule{}
	lp.lastRolledFor = time.Time{}
	saver := m.scheduleSaver
	m.mu.Unlock()
	if saver != nil {
		saver(id, Schedule{})
	}
	return true
}

// HydrateSchedules loads persisted schedules at boot. `loader(id)`
// returns the stored schedule for each configured loadpoint; missing
// entries return (Schedule{}, false). Unknown IDs in storage are
// silently ignored — the operator may have renamed a loadpoint, and
// resurrecting a schedule under the new ID would be surprising.
//
// Does NOT invoke the saver — this is a load path. Does NOT call
// RollSchedules either; the controller's first tick will handle that.
func (m *Manager) HydrateSchedules(loader func(id string) (Schedule, bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.order {
		lp, ok := m.byID[id]
		if !ok {
			continue
		}
		s, found := loader(id)
		if !found || s.Empty() {
			continue
		}
		lp.schedule = s
	}
}

// RollSchedules brings each loadpoint's one-shot target_soc / target_time
// into line with its persisted schedule. Two cases:
//
//   - Recurring=true: refresh target_time forward each time the prior
//     deadline passes, so the deadline penalty in MPC never goes stale.
//   - Recurring=false: seed the one-shot target ONCE on the first roll
//     after the schedule was saved (SetSchedule clears lastRolledFor as
//     its sentinel). After the deadline passes, leave target_time in the
//     past — MPC treats that as "no deadline" and the schedule expires
//     quietly. The schedule itself stays for the operator to inspect or
//     clear via the API.
//
// Idempotent on subsequent ticks. Cheap to call every dispatch cycle.
func (m *Manager) RollSchedules(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, lp := range m.byID {
		s := lp.schedule
		if s.Empty() {
			continue
		}
		next := NextDailyUTC(now, s.TimeOfDayMinUTC)
		if s.Recurring {
			if !lp.targetTime.IsZero() && lp.targetTime.After(now) {
				continue
			}
			lp.targetTime = next
			lp.targetSoCPct = s.SoCPct
			lp.lastRolledFor = next
			continue
		}
		// Non-recurring: seed exactly once per SetSchedule. The Empty
		// SetSchedule path (clear) also resets lastRolledFor, so a
		// re-save with a non-recurring schedule re-seeds.
		if lp.lastRolledFor.IsZero() {
			lp.targetTime = next
			lp.targetSoCPct = s.SoCPct
			lp.lastRolledFor = next
		}
	}
}
