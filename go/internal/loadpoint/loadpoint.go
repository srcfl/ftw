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
	"crypto/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/units"
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

	// Assumed EV SoC at plug-in (0–1). Chargers like Easee don't report
	// the vehicle's SoC directly — only cumulative session energy.
	// Current SoC is then estimated as PluginSoC + deliveredWh*DefaultChargeEfficiency/capacityWh.
	// 0 defaults to units.DefaultPluginSoC (0.20). Operators who care can
	// override per-loadpoint or pre-plug-in.
	PluginSoC float64 `yaml:"plugin_soc,omitempty" json:"plugin_soc,omitempty"`

	// PhaseMode selects how the controller picks between 1Φ and 3Φ
	// delivery each tick. "3p" (default) and "1p" lock the install to
	// one mode and filter AllowedStepsW accordingly. "auto" charges on
	// three phases; only surplus charging below the 3Φ minimum switches
	// to one, with a hysteresis timer (MinPhaseHoldS) preventing flap.
	// Empty == "3p" for backward compatibility with pre-switching configs.
	PhaseMode string `yaml:"phase_mode,omitempty" json:"phase_mode,omitempty"`

	// PhaseSplitW is the wantW threshold below which "auto" picks 1Φ.
	// Zero defaults to 3680 W — the ceiling a 16 A fuse can sustain
	// on a single phase. It does not classify AllowedStepsW: planned and
	// surplus charging count a step as 3Φ from 6 A on each phase.
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
	ManualRestoreUnconfirmed bool    `json:"manual_restore_unconfirmed"`
	ManualSaveError          bool    `json:"manual_save_error"`
	ManualSavePending        bool    `json:"manual_save_pending,omitempty"`
	VehicleCapacityWh        float64 `json:"vehicle_capacity_wh"`
	CapacitySource           string  `json:"capacity_source"`
	// ChargingDeclined is a sustained vehicle-side refusal, not a battery level.
	ChargingDeclined bool `json:"charging_declined"`
	// SoCRetention reports whether the confirmed estimate can survive restart.
	EnergySource         string    `json:"energy_source,omitempty"`
	EnergyUpdatedAtMs    int64     `json:"energy_updated_at_ms,omitempty"`
	PowerUpdatedAtMs     int64     `json:"power_updated_at_ms,omitempty"`
	PowerUnavailable     bool      `json:"power_unavailable,omitempty"`
	SoCRetention         string    `json:"soc_retention,omitempty"`
	ID                   string    `json:"id"`
	DriverName           string    `json:"driver_name"`
	PluggedIn            bool      `json:"plugged_in"`
	CurrentSoC           float64   `json:"current_soc"`          // observed or estimated
	CurrentPowerW        float64   `json:"current_power_w"`      // actual draw (site sign: + = charging)
	DeliveredWhSession   float64   `json:"delivered_wh_session"` // since plug-in
	FinishAtVehicleLimit bool      `json:"finish_at_vehicle_limit,omitempty"`
	GoalComplete         bool      `json:"goal_complete,omitempty"`
	GoalRetention        string    `json:"goal_retention,omitempty"`
	TargetSoC            float64   `json:"target_soc"`            // user intent
	TargetTime           time.Time `json:"target_time,omitempty"` // user intent
	UpdatedAtMs          int64     `json:"updated_at_ms"`

	// Vehicle-side telemetry, populated by the API layer from the most
	// recent DerVehicle reading whose charging_state indicates a likely
	// physical connection. Zero values when no online vehicle driver is
	// reporting. SoCSource is "vehicle" when CurrentSoC was overridden
	// from the car's BMS, "inferred" when it's the loadpoint manager's
	// confirmed anchor + deliveredWh estimate, "assumed" before the user
	// or a matched vehicle confirms a level, "" when not plugged in.
	VehicleSoC           float64 `json:"vehicle_soc,omitempty"`
	VehicleChargeLimit   float64 `json:"vehicle_charge_limit,omitempty"`
	VehicleChargingState string  `json:"vehicle_charging_state,omitempty"`
	VehicleDriver        string  `json:"vehicle_driver,omitempty"`
	VehicleStale         bool    `json:"vehicle_stale,omitempty"`
	VehicleSoCAgeS       int64   `json:"vehicle_soc_age_s,omitempty"`
	SoCSource            string  `json:"soc_source,omitempty"`
	// PlanningSoC is the state of charge a new plan starts from. The API
	// sets it from PlanningStartSoC so the charging view matches the planner.
	// PlanningFromVehicle is true when that value came from a car reading.
	PlanningSoC         *float64 `json:"planning_soc,omitempty"`
	PlanningFromVehicle bool     `json:"planning_from_vehicle,omitempty"`
	// VehicleName is the vehicle profile the session identified (via the
	// charging transaction's idTag/idToken), empty when none matched. With
	// cars it is the session's car's name.
	VehicleName string `json:"vehicle_name,omitempty"`
	// The car on the charger and how the box knows it (docs/cars.md). Empty
	// while nothing is plugged in. The registry names them carId,
	// carIdentifiedBy and carCorrectedFrom.
	Session          string   `json:"session,omitempty"`
	CarID            string   `json:"car_id,omitempty"`
	CarIdentifiedBy  string   `json:"car_identified_by,omitempty"`
	CarCorrected     bool     `json:"car_corrected,omitempty"`
	CarCorrectedFrom string   `json:"car_corrected_from,omitempty"`
	Choose           []string `json:"choose,omitempty"`
	// SoCUnknown is a visitor's session: no level, and CurrentSoC counts
	// only what was delivered. Routes leave the level out.
	SoCUnknown bool `json:"-"`
	// Observed is set once the charger has reported since the box started:
	// before that, PluggedIn says nothing about the cable.
	Observed bool `json:"-"`
	// LevelFromCar is set while the session's level is anchored to a
	// reading the car itself made, and SoCConfirmedAt is when the level was
	// read or set; zero when it is not confirmed.
	LevelFromCar   bool      `json:"-"`
	SoCConfirmedAt time.Time `json:"-"`
	// PluggedAt is when this connection was first seen, and SessionKey the
	// charger's own session identity when it has one. Together they tell
	// one plug-in from the next.
	PluggedAt  time.Time `json:"-"`
	SessionKey string    `json:"-"`
	// CarMaxW is the most the session car's own charger takes, and
	// CarChargeLimit the car's charge limit while its level is exact; zero
	// when unknown. The plan and the reserve hold to them (LimitPower,
	// PlanningTarget).
	CarMaxW        float64 `json:"-"`
	CarChargeLimit float64 `json:"-"`
	// CarPhases is 1 when this session's car draws on one phase of a
	// three-phase order, seen on the site meter, and 0 when unknown; a car
	// profile's phases may set it later. CarPhase is that phase, 1-3. The
	// plan counts the car's one-phase levels (OnePhaseCarSteps) and the
	// order names the current per phase it should take.
	CarPhases int `json:"-"`
	CarPhase  int `json:"-"`

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
	// ManualChargeW is the held setpoint in watts. ManualReleaseSoC
	// (0–1), when non-zero, is the "charge now" target at which the
	// controller releases the hold back to the plan. Populated by the
	// API layer from the loadpoint controller.
	ManualActive     bool    `json:"manual_active"`
	ManualChargeW    float64 `json:"manual_charge_w,omitempty"`
	ManualReleaseSoC float64 `json:"manual_release_soc,omitempty"`
	// Manual is the live account of the hold: what was ordered, since when,
	// and what the charger did with it. Populated by the API layer from the
	// controller and the charger's reading; see ManualStatusFrom.
	Manual ManualStatus `json:"manual"`
	// Charger is the driver reading used for feedback in every charging mode.
	Charger *ChargerStatus `json:"charger,omitempty"`

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

	// CommandedW is what the controller last ordered this loadpoint to
	// deliver, after every clamp. CommandedKnown separates "ordered
	// zero" from "no dispatch tick has run yet". The UI reads the pair
	// to tell "the box is offering power the car is not taking" from
	// "the box is pausing on purpose".
	CommandedW     float64 `json:"commanded_w"`
	CommandedKnown bool    `json:"commanded_known"`
	// CommandedSinceMs is when the current order was first given; it moves
	// when CommandedW or CommandedReason changes. Zero until the first tick.
	CommandedSinceMs int64 `json:"commanded_since_ms,omitempty"`
	// Internal identity of the manual choice used to compute the order.
	ManualCommandUpdatedAt time.Time `json:"-"`
	// CommandedPhases is 1 or 3: the phases the last command lands on, from
	// its phase_mode and split. Zero before the first command.
	CommandedPhases int `json:"-"`

	// CommandedReason names the dispatch branch that decided CommandedW:
	// "plan", "no_plan_budget", "pv_surplus", "pv_surplus_pause",
	// "fuse_limit", "import_limit", "site_meter_stale", "manual_hold",
	// "below_phase_minimum", "wake_kick". Empty until the first dispatch tick. The UI renders the specific cause
	// instead of a generic "paused by the box".
	CommandedReason string `json:"commanded_reason,omitempty"`

	// GridDeferred is true while MPC has deferred grid-funded planning
	// because the target deadline lies past the published price
	// horizon — the loadpoint behaves surplus-only until tomorrow's
	// prices land. Without this flag that deferral is invisible and
	// reads as "PV only that nobody chose". Populated by the API layer
	// from the loadpoint controller.
	GridDeferred bool `json:"grid_deferred"`

	// PlanNextStartMs/PlanNextEndMs/PlanNextWh describe the next
	// window in which the active plan allocates charge energy to this
	// loadpoint; PlanTotalWh is everything the plan still intends to
	// deliver over the horizon. All zero when the planner has no
	// allocation. Populated by the API layer from the MPC plan.
	PlanPending     bool    `json:"plan_pending"`
	PlanOutdated    bool    `json:"plan_outdated"`
	PlanNextStartMs int64   `json:"plan_next_start_ms,omitempty"`
	PlanNextEndMs   int64   `json:"plan_next_end_ms,omitempty"`
	PlanNextWh      float64 `json:"plan_next_wh,omitempty"`
	PlanTotalWh     float64 `json:"plan_total_wh,omitempty"`
	// PlanWindows lists every remaining window (the next one first) so a
	// client can draw the plan instead of reading one sentence about
	// it. Same source as the PlanNext* fields; empty when the planner
	// has no allocation or the plan is stale.
	PlanWindows []PlanWindow `json:"plan_windows,omitempty"`
}

// PlanWindow is one contiguous stretch in which the active plan
// allocates charge energy to a loadpoint. Millisecond epoch bounds so
// the on-box UI can place it on a clock without a timezone round-trip.
type PlanWindow struct {
	StartMs int64   `json:"start_ms"`
	EndMs   int64   `json:"end_ms"`
	Wh      float64 `json:"wh"`
}

// Manager holds the running set of loadpoints. Thread-safe.
type Manager struct {
	nextSessionGeneration uint64
	sessionMu             sync.Mutex
	sessionStore          SessionStore
	pendingManual         map[string]pendingManualHold
	connectionHealth      map[string]bool
	connectionEdges       map[string]connectionEdge
	mu                    sync.RWMutex
	byID                  map[string]*loadpointRuntime
	order                 []string // insertion-preserving id list for deterministic listing

	// intentMu serializes durable goal and solar edits with config reloads.
	intentMu sync.Mutex
	// levelObserver hears a level set by hand (SetLevelObserver).
	levelObserver func(id string, soc float64, at time.Time)
	// scheduleSaver, if non-nil, is invoked synchronously whenever a
	// schedule is set or cleared. Wired by main.go to persist via
	// state.SaveConfig. Left nil in tests / sites without storage.
	scheduleSaver func(id string, s Schedule) error

	// surplusOnlySaver, if non-nil, persists the runtime surplus_only
	// flag whenever an operator toggles it. Without this the flag
	// reverts to whatever's in YAML on every restart — operators were
	// finding that frustrating since the toggle lives in the dashboard
	// EV modal, not the YAML they'd think to edit. Same pattern as
	// scheduleSaver.
	surplusOnlySaver func(id string, v bool) error

	// nowFn is the clock the manager uses for time-sensitive logic
	// (session-completion timer in particular). Defaults to time.Now;
	// tests inject a deterministic clock via SetNowFn.
	nowFn func() time.Time

	// loc is the time zone Schedule.Days weekday masks are read in.
	// Nil means time.Local — the box's own zone, which is the only
	// zone "charge on weekdays" can honestly mean. Tests pin a fixed
	// zone via SetLocation so weekday assertions don't depend on the
	// machine running them.
	loc *time.Location

	// bus, when set, receives ChargingSessionComplete at the completion
	// latch and ChargingInterrupted from the hysteresis below. Nil (the
	// default, and every existing test) publishes nothing — a nil bus is
	// already a safe no-op on Publish, so this needs no guard.
	bus *events.Bus
}

// SessionCompletionTimeout debounces a sustained vehicle-side refusal.
// A refusal is not evidence that the battery reached its target.
const SessionCompletionTimeout = 90 * time.Second

// The interruption hysteresis. A charge that had run steadily for at least
// InterruptSteadyRun and then stops below steadyChargeFloorW for
// interruptConfirm — cable still in, box not the one pausing it, completion
// latch not tripped — is a session that ended before it was done, and the
// one somebody wants their phone to mention. Every threshold here exists so
// a flapping cable or a passing cloud never becomes lock-screen noise: a
// short run never arms it, a short dip never trips it, and firing disarms
// it until another full steady run.
const (
	// steadyChargeFloorW is the "actually charging" floor. The smallest
	// real delivery step is 1Φ 6 A ≈ 1380 W; 500 W splits the difference
	// between that and settling noise with room for odd chargers.
	steadyChargeFloorW = 500.0
	// InterruptSteadyRun is how long a session must have charged
	// continuously before its stopping can count as an interruption.
	InterruptSteadyRun = 10 * time.Minute
	// interruptConfirm is how long the stop must persist before it is
	// believed — the same debounce the completion latch uses.
	interruptConfirm = SessionCompletionTimeout
)

// loadpointRuntime is the in-memory representation. Its fields are the
// union of configured parameters and observed state. Lives behind
// Manager so consumers access it via the public State snapshot.
type loadpointRuntime struct {
	configGeneration         uint64
	sessionGeneration        uint64
	connectionGeneration     uint64
	manualRestoreUnconfirmed bool
	manualSaveError          bool
	manualSavePending        bool
	energy                   *sessionEnergy
	powerAt                  time.Time
	powerWindow              time.Duration
	chargingPeriodSince      time.Time
	powerUnavailable         bool
	lastSavedEnergyWh        float64
	lastSavedEnergyAt        time.Time
	lastSessionCommitSeq     uint64
	sessionDeviceID          string
	sessionID                string
	socRetention             string
	completionNotified       bool
	// goalRiskNotified latches charging.goal_at_risk once per plug-in.
	// vehicleAnchored is true once the car itself reported a level this
	// session; chargedSinceStart once it drew power in this process, so a
	// restart beside a car that is already done says nothing.
	goalRiskNotified  bool
	vehicleAnchored   bool
	chargedSinceStart bool
	// carPhases is this session's evidence of a one-phase car
	// (one_phase_car.go).
	carPhases carPhases
	Config

	pluggedIn            bool
	connectionObservedAt time.Time
	currentSoC           float64
	currentPowerW        float64
	deliveredWhSession   float64
	// deliveredHistory samples deliveredWhSession during the session, so
	// a car reading can be anchored at the time it was measured.
	deliveredHistory         []deliveredSample
	finishAtVehicleLimit     bool
	finishGoalCompleted      bool
	finishGoalSavedCompleted bool
	finishGoalChecked        bool
	finishGoalExplicit       bool
	finishGoalSaved          time.Time
	finishGoalRetention      string
	targetSoC                float64
	targetTime               time.Time
	updatedAtMs              int64

	// Plug-in anchor: the SoC we believe the vehicle was at when
	// this session began. Persisted across Observe() calls so SoC
	// inference (pluginSoC + deliveredWh*DefaultChargeEfficiency/capacity) stays stable
	// even as session_wh grows. Reset to Config.PluginSoC on
	// every plug-in transition (prev !pluggedIn → now pluggedIn).
	sessionPluginSoC float64

	// baseCapacityWh is the capacity to restore on plug-out after the car
	// itself reported its own for the session (capacityFromCar): the next
	// car may be a different one.
	baseCapacityWh float64

	// car is the session's car (SetCarBinding), and socUnknown is a
	// visitor's session, whose level nobody knows.
	car        CarBinding
	socUnknown bool
	// configCapacityWh is config.yaml's vehicle_capacity_wh, which a
	// binding with no capacity of its own falls back to. observed is set
	// once the charger has reported since the box started.
	configCapacityWh float64
	observed         bool

	// capacityFromCar is set when the vehicle itself reported its battery
	// capacity (OCPP 2.0.1 NotifyEVChargingNeeds), which outranks the
	// session car's — one is measured, the other is an owner's estimate.
	// Plug-out restores baseCapacityWh.
	capacityFromCar bool

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

	// chargingDeclined latches once the vehicle has held "not
	// requesting" past SessionCompletionTimeout for this session.
	// It suspends planning while the vehicle refuses energy, without
	// changing the estimated battery level. Cleared on plug-out.
	chargingDeclined bool

	// socConfirmed is true only after a level from the user or a matched car.
	// A configured/default plug-in level remains a planning assumption.
	// socConfirmedAt is when the user set it or the car measured it. Zero means the
	// time was not kept (for example a level restored after restart);
	// a car reading may then replace it. A newer confirmation wins over
	// an older car measurement.
	socConfirmed   bool
	socConfirmedAt time.Time

	// surplusWithheld is set by the controller each tick: true when WE
	// are intentionally withholding power from this loadpoint (a
	// surplus_only pause below the 3-phase floor). While set, a vehicle
	// reporting "not requesting current" is responding to our own pause,
	// not declining charge — so Observe must not count it toward session
	// completion. Without this a cloudy spell below the floor latches the
	// session done and the planner stops offering PV surplus for the rest
	// of the day. Transient per-tick; the controller refreshes it.
	surplusWithheld bool

	// commandedW is the power the controller last ordered this loadpoint
	// to deliver; commandedKnown separates "ordered zero" from "nobody is
	// ordering". The interruption latch reads it: a charger at 0 W because
	// the plan parked it in an expensive hour, or an operator pressed
	// Stop, is the box doing its job, not a charge that failed.
	// commandedReason names the dispatch branch that decided the value —
	// see Manager.SetCommanded for the token set.
	commandedW      float64
	commandedKnown  bool
	commandedReason string
	commandedPhases int // 1 or 3: where the last command lands; 0 before it
	// commandedSince is when the current (commandedW, commandedReason) pair
	// was first ordered. The manual status counts elapsed time from it.
	commandedSince         time.Time
	manualCommandUpdatedAt time.Time

	// The interruption hysteresis state. chargingSteadySince anchors the
	// current continuous above-floor run; steadyRunArmed latches once that
	// run reaches InterruptSteadyRun and is consumed by a fire (or cleared
	// by plug-out), so one stop is one event; stoppedSince anchors the
	// below-floor spell being timed against interruptConfirm.
	chargingSteadySince time.Time
	stoppedSince        time.Time
	steadyRunArmed      bool
}

// NewManager returns an empty manager. Configure with Load().
func NewManager() *Manager {
	return &Manager{byID: map[string]*loadpointRuntime{}}
}

// SetBus wires charging events and the freshness used to qualify cable edges.
func (m *Manager) SetBus(bus *events.Bus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bus = bus
	if bus == nil {
		return
	}
	bus.Subscribe(events.KindHealthTick, func(e events.Event) {
		tick, ok := e.(events.HealthTick)
		if !ok {
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.connectionHealth = make(map[string]bool, len(tick.Health))
		for name, h := range tick.Health {
			m.connectionHealth[name] = h.TelemetryLive() && h.LastSuccess != nil && !h.LastSuccess.IsZero()
		}
	})
}

// SetCommandedW records what the controller last ordered this loadpoint to
// deliver. The interruption latch reads it to tell "the charge failed" from
// "the box paused it on purpose" — a plan parking the car through expensive
// hours must never page anyone. No-op for an unknown id.
func (m *Manager) SetCommandedW(id string, w float64) {
	m.SetCommanded(id, w, "")
}

// SetCommanded records the ordered watts together with the reason the
// dispatch branch chose that value — the fact operators were left to
// reverse-engineer from amp readings (#1009). Reasons are short stable
// tokens: "plan", "no_plan_budget", "pv_surplus", "pv_surplus_pause",
// "fuse_limit", "import_limit", "site_meter_stale", "manual_hold",
// "below_phase_minimum", "wake_kick". Empty keeps whatever was recorded before (used by the legacy SetCommandedW wrapper).
// No-op for an unknown id.
func (m *Manager) SetCommanded(id string, w float64, reason string) {
	m.setCommandedForManual(id, w, reason, time.Time{})
}

func (m *Manager) setCommandedForManual(id string, w float64, reason string, updatedAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lp, ok := m.byID[id]; ok {
		changed := !lp.commandedKnown || lp.commandedW != w ||
			(reason != "" && reason != lp.commandedReason)
		lp.commandedW = w
		lp.manualCommandUpdatedAt = updatedAt
		lp.commandedKnown = true
		if reason != "" {
			lp.commandedReason = reason
		}
		if changed {
			lp.commandedSince = time.Now()
		}
	}
}

// setCommandedPhases records the phases the last command lands on.
func (m *Manager) setCommandedPhases(id string, phases int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lp, ok := m.byID[id]; ok {
		lp.commandedPhases = phases
	}
}

// Load replaces the configured set. Idempotent: existing state is
// carried across when the ID is kept; removed IDs are dropped.
func (m *Manager) Load(cfgs []Config) {
	m.intentMu.Lock()
	defer m.intentMu.Unlock()
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	var changedCapacity []string
	m.mu.Lock()
	defer func() {
		m.mu.Unlock()
		for _, id := range changedCapacity {
			m.persistSession(id)
		}
	}()

	newByID := make(map[string]*loadpointRuntime, len(cfgs))
	newOrder := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		if c.ID == "" {
			continue
		}
		lp := &loadpointRuntime{Config: c, configCapacityWh: c.VehicleCapacityWh}
		if existing := m.byID[c.ID]; existing != nil && existing.DriverName == c.DriverName {
			lp.sessionGeneration = existing.sessionGeneration
			lp.connectionGeneration = existing.connectionGeneration
			lp.configGeneration = existing.configGeneration
		} else {
			m.nextSessionGeneration++
			lp.sessionGeneration = m.nextSessionGeneration
			lp.configGeneration = m.nextSessionGeneration
		}
		if existing, ok := m.byID[c.ID]; ok {
			// Preserve observed state across reload. The session
			// plug-in anchor is carried too — otherwise a config
			// hot-reload during a charging session would drop our
			// SoC reference and reset the estimate back to
			// PluginSoC even though delivered_wh has grown.
			lp.pluggedIn = existing.pluggedIn
			lp.connectionObservedAt = existing.connectionObservedAt
			lp.currentSoC = existing.currentSoC
			lp.currentPowerW = existing.currentPowerW
			lp.deliveredWhSession = existing.deliveredWhSession
			lp.deliveredHistory = existing.deliveredHistory
			lp.energy = existing.energy
			lp.powerAt = existing.powerAt
			lp.powerWindow = existing.powerWindow
			lp.chargingPeriodSince = existing.chargingPeriodSince
			lp.powerUnavailable = existing.powerUnavailable
			lp.lastSavedEnergyWh = existing.lastSavedEnergyWh
			lp.lastSavedEnergyAt = existing.lastSavedEnergyAt
			lp.lastSessionCommitSeq = existing.lastSessionCommitSeq
			lp.targetSoC = existing.targetSoC
			lp.finishAtVehicleLimit = existing.finishAtVehicleLimit
			lp.finishGoalCompleted = existing.finishGoalCompleted
			lp.finishGoalSavedCompleted = existing.finishGoalSavedCompleted
			lp.finishGoalChecked = existing.finishGoalChecked
			lp.finishGoalExplicit = existing.finishGoalExplicit
			lp.finishGoalSaved = existing.finishGoalSaved
			lp.finishGoalRetention = existing.finishGoalRetention
			lp.targetTime = existing.targetTime
			lp.updatedAtMs = existing.updatedAtMs
			lp.sessionPluginSoC = existing.sessionPluginSoC
			lp.schedule = existing.schedule
			lp.lastRolledFor = existing.lastRolledFor
			lp.notRequestingSince = existing.notRequestingSince
			lp.chargingDeclined = existing.chargingDeclined
			lp.socConfirmed = existing.socConfirmed && existing.DriverName == c.DriverName
			if lp.socConfirmed {
				lp.socConfirmedAt = existing.socConfirmedAt
			}
			if existing.DriverName == c.DriverName {
				lp.sessionDeviceID = existing.sessionDeviceID
				lp.sessionID = existing.sessionID
				lp.socRetention = existing.socRetention
				lp.completionNotified = existing.completionNotified
				lp.manualRestoreUnconfirmed = existing.manualRestoreUnconfirmed
				lp.manualSaveError = existing.manualSaveError
				lp.manualSavePending = existing.manualSavePending
			} else {
				lp.pluggedIn = false
				lp.currentSoC = 0
				lp.chargingDeclined = false
			}
			lp.commandedW = existing.commandedW
			lp.commandedReason = existing.commandedReason
			lp.commandedKnown = existing.commandedKnown
			lp.commandedSince = existing.commandedSince
			lp.manualCommandUpdatedAt = existing.manualCommandUpdatedAt
			lp.chargingSteadySince = existing.chargingSteadySince
			lp.stoppedSince = existing.stoppedSince
			lp.steadyRunArmed = existing.steadyRunArmed
			lp.capacityFromCar = existing.capacityFromCar
			lp.car = existing.car
			lp.observed = existing.observed
			if existing.DriverName == c.DriverName {
				lp.socUnknown = existing.socUnknown
			}
			if existing.car.CapacityWh > 0 {
				// The car's capacity outlives a config reload, as the car
				// does. A visitor's is the charger's, from the new config.
				lp.Config.VehicleCapacityWh = existing.car.CapacityWh
			}
			if existing.capacityFromCar {
				// An identified car survives config hot-reload: keep the
				// session's applied capacity, but re-base the plug-out
				// restore on the NEW config's value, or the car's.
				lp.baseCapacityWh = lp.Config.VehicleCapacityWh
				if existing.VehicleCapacityWh > 0 {
					lp.Config.VehicleCapacityWh = existing.VehicleCapacityWh
				}
			}
			if existing.DriverName == c.DriverName && lp.pluggedIn && existing.VehicleCapacityWh != lp.VehicleCapacityWh {
				// A capacity correction changes future Wh-to-SoC conversion,
				// not the battery level the user just saw or its confidence.
				delivered := 0.0
				if lp.VehicleCapacityWh > 0 {
					delivered = lp.deliveredWhSession * DefaultChargeEfficiency / lp.VehicleCapacityWh
				}
				lp.sessionPluginSoC = existing.currentSoC - delivered
				changedCapacity = append(changedCapacity, c.ID)
			}
		}
		newByID[c.ID] = lp
		newOrder = append(newOrder, c.ID)
	}
	for id := range m.connectionEdges {
		next, present := newByID[id]
		previous := m.byID[id]
		if !present || previous == nil || previous.DriverName != next.DriverName {
			delete(m.connectionEdges, id)
		}
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
	return m.snapshot(lp), true
}

// States returns snapshots of every configured loadpoint, sorted by
// the configured ID order. Useful for GET /api/loadpoints.
func (m *Manager) States() []State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]State, 0, len(m.order))
	for _, id := range m.order {
		if lp, ok := m.byID[id]; ok {
			out = append(out, m.snapshot(lp))
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

// SetSurplusWithheld marks a surplus pause the controller chose.
// Observe then ignores "not requesting" for the completion timer.
func (m *Manager) SetSurplusWithheld(id string, withheld bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lp, ok := m.byID[id]; ok {
		lp.surplusWithheld = withheld
	}
}

// Observe updates one loadpoint from charger telemetry. SoC is inferred
// from the plug-in anchor plus delivered Wh, because the charger does not
// read the pack. A new plug resets that anchor to Config.PluginSoC.
// requestActive false means the car stopped asking, not that we throttled
// it. After SessionCompletionTimeout the planner stops allocating.
// That does not mark the target reached. Unknown ids are ignored.
func (m *Manager) Observe(id string, pluggedIn bool, powerW, deliveredWh float64, requestActive bool) {
	m.ObserveSession(id, pluggedIn, powerW, deliveredWh, requestActive, "", "")
}

func (m *Manager) observe(id string, pluggedIn bool, powerW, deliveredWh float64, requestActive bool) ([]events.Event, *events.Bus) {
	m.mu.Lock()
	lp, ok := m.byID[id]
	if !ok {
		m.mu.Unlock()
		return nil, nil
	}
	// Events decided under the lock, published after it: the bus runs
	// handlers inline on the publisher, and a handler that looked back at
	// this manager would deadlock.
	var fired []events.Event
	bus := m.bus
	now := m.now()
	if m.observeConnectionLocked(id, lp.DriverName, pluggedIn, now) {
		fired = append(fired, events.ChargingConnected{LoadpointID: id, At: now})
	}
	if pluggedIn && !lp.pluggedIn {
		lp.connectionObservedAt = now
		if lp.finishAtVehicleLimit && lp.schedule.Recurring && lp.finishGoalCompleted {
			lp.finishGoalCompleted = false
			lp.targetSoC = 1
			lp.targetTime = lp.schedule.NextDeadlineUTC(now, m.loc)
			lp.lastRolledFor = lp.targetTime
		}
		// Plug-in transition: seed the session anchor and clear any
		// session-completion latched from a prior session.
		anchor := lp.PluginSoC
		if anchor <= 0 {
			anchor = units.DefaultPluginSoC
		}
		lp.sessionPluginSoC = anchor
		lp.socConfirmed = false
		lp.socUnknown = false
		lp.completionNotified = false
		lp.goalRiskNotified = false
		lp.vehicleAnchored = false
		lp.chargedSinceStart = false
		lp.deliveredHistory = nil
		lp.notRequestingSince = time.Time{}
		lp.chargingDeclined = false
		lp.carPhases = carPhases{}
	}
	if !pluggedIn {
		// Plug-out: drop any pending completion timer / latch.
		lp.notRequestingSince = time.Time{}
		lp.chargingDeclined = false
		lp.carPhases = carPhases{}
		if lp.capacityFromCar {
			// The car that measured its own capacity left with its
			// session — the next one may be different.
			lp.VehicleCapacityWh = lp.baseCapacityWh
			lp.capacityFromCar = false
			lp.baseCapacityWh = 0
		}
	}
	lp.pluggedIn = pluggedIn
	lp.currentPowerW = powerW
	lp.deliveredWhSession = deliveredWh
	if pluggedIn {
		lp.recordDelivered(now, deliveredWh)
	} else {
		lp.deliveredHistory = nil
	}

	if pluggedIn && powerW >= DeliveringW {
		// Measured energy delivery is stronger evidence than a delayed
		// request_active flag. Let planning follow the car immediately.
		lp.chargingDeclined = false
		lp.notRequestingSince = time.Time{}
	} else if pluggedIn && !requestActive && (lp.surplusWithheld || (lp.commandedKnown && lp.commandedW < DeliveringW)) {
		// Self-induced "not requesting": we paused this surplus_only
		// loadpoint below its floor, so the vehicle dropping current is
		// our doing, not a vehicle-side decline. Do not start/advance the
		// completion timer — otherwise a sub-floor spell would latch the
		// session done and the planner would stop offering surplus all
		// day. Reset the clock so a genuine refusal (once we resume
		// offering power) is timed from a clean start.
		lp.notRequestingSince = time.Time{}
	} else if pluggedIn && !requestActive && (!lp.commandedKnown || lp.commandedW >= DeliveringW) {
		// Vehicle has explicitly stopped requesting current while we ARE
		// offering power. Start (or continue) the completion timer; latch
		// once it elapses.
		if lp.notRequestingSince.IsZero() {
			lp.notRequestingSince = now
		}
		if !lp.chargingDeclined && lp.targetSoC > 0 &&
			!lp.notRequestingSince.IsZero() &&
			now.Sub(lp.notRequestingSince) >= SessionCompletionTimeout {
			lp.chargingDeclined = true
		}
	} else if pluggedIn && requestActive {
		// Vehicle is back to requesting. Reset the timer, but keep
		// chargingDeclined latched — once a vehicle has declined this
		// session, treating it as "still hungry" the moment an EVSE
		// retry briefly succeeds would reopen the export hole the
		// completion latch exists to close. Plug-cycle to reset.
		lp.notRequestingSince = time.Time{}
	}

	// The interruption hysteresis. Armed by a steady above-floor run,
	// tripped by a confirmed stop the box did not order, disarmed by
	// firing — see the constants beside SessionCompletionTimeout for why
	// each threshold exists.
	switch {
	case pluggedIn && powerW >= steadyChargeFloorW:
		if lp.chargingSteadySince.IsZero() {
			lp.chargingSteadySince = now
		}
		lp.stoppedSince = time.Time{}
		if now.Sub(lp.chargingSteadySince) >= InterruptSteadyRun {
			lp.steadyRunArmed = true
		}
	case pluggedIn:
		if !lp.chargingSteadySince.IsZero() {
			// Charging just stopped. Credit a run that crossed the
			// threshold on its way down, then start timing the stop.
			if now.Sub(lp.chargingSteadySince) >= InterruptSteadyRun {
				lp.steadyRunArmed = true
			}
			lp.chargingSteadySince = time.Time{}
			lp.stoppedSince = now
		}
		// A pause the box ordered — plan slot, operator Stop, surplus
		// clamp, safety standdown — is the box working, and a vehicle
		// that stopped requesting chose to stop. Neither is a failure.
		selfInflicted := lp.surplusWithheld ||
			(lp.commandedKnown && lp.commandedW < steadyChargeFloorW)
		if lp.steadyRunArmed && !lp.chargingDeclined && requestActive &&
			!selfInflicted && !lp.stoppedSince.IsZero() &&
			now.Sub(lp.stoppedSince) >= interruptConfirm {
			lp.steadyRunArmed = false
			fired = append(fired, events.ChargingInterrupted{
				LoadpointID: id,
				At:          now,
			})
		}
	default: // unplugged
		lp.chargingSteadySince = time.Time{}
		lp.stoppedSince = time.Time{}
		lp.steadyRunArmed = false
	}

	if pluggedIn {
		lp.currentSoC = estimateSoC(lp.sessionPluginSoC,
			deliveredWh, lp.VehicleCapacityWh)
		if powerW >= DeliveringW {
			lp.chargedSinceStart = true
		}
		// A car that reports nothing itself is done when the box's own
		// estimate, from a level someone set, reaches the target. Said in
		// kWh, which the box measured; the level is an estimate.
		if !lp.completionNotified && !lp.vehicleAnchored && lp.socConfirmed && lp.chargedSinceStart &&
			lp.targetSoC > 0 && lp.currentSoC >= lp.targetSoC && deliveredWh >= minCompletionWh {
			lp.completionNotified = true
			fired = append(fired, events.ChargingSessionComplete{LoadpointID: id, KWh: deliveredWh / 1000, At: now})
		}
	} else {
		lp.currentSoC = 0
	}
	lp.updatedAtMs = now.UnixMilli()
	lp.observed = true
	m.mu.Unlock()

	return fired, bus
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

// SetLocation pins the time zone Schedule.Days weekday masks are
// evaluated in. main.go leaves it alone — time.Local is the box's own
// zone — and tests pass a fixed zone. Pass nil to revert.
func (m *Manager) SetLocation(loc *time.Location) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loc = loc
}

// estimateSoC returns the vehicle SoC (0–1) inferred from the session
// anchor + energy delivered. Chargers like Easee don't expose the
// car's BMS; this is the best-effort estimate the MPC uses.
//
// Clamps to [0, 1]. Falls back to the anchor when capacity is
// unknown (can't translate Wh → fraction).
// DefaultChargeEfficiency is the shared AC-to-car estimate used by planning.
const DefaultChargeEfficiency = 0.9

func estimateSoC(pluginSoC, deliveredWh, capacityWh float64) float64 {
	if capacityWh <= 0 {
		return units.ClampFraction(pluginSoC)
	}
	return units.ClampFraction(pluginSoC + deliveredWh*DefaultChargeEfficiency/capacityWh)
}

// SetTarget updates the user-intent fields for an existing loadpoint.
// targetTime zero = no deadline. Returns false for unknown IDs.
func (m *Manager) SetTarget(id string, soc float64, targetTime time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok {
		return false
	}
	if units.ClampFraction(soc) > lp.targetSoC {
		lp.chargingDeclined = false
		lp.notRequestingSince = time.Time{}
	}
	lp.finishAtVehicleLimit = false
	lp.finishGoalCompleted = false
	lp.targetSoC = units.ClampFraction(soc)
	lp.targetTime = targetTime
	return true
}

// SetSurplusOnly changes solar-only charging and returns the previous choice.
// A missing loadpoint or failed save returns ok=false. Consumers use the
// transition to replan before charging can draw from the grid.
func (m *Manager) SetSurplusOnly(id string, v bool) (prev bool, ok bool) {
	prev, ok, err := m.SetSurplusOnlyChecked(id, v)
	return prev, ok && err == nil
}

// SetSurplusOnlyChecked keeps the previous solar preference until storage
// accepts the change. Readers and charging continue with the current choice.
func (m *Manager) SetSurplusOnlyChecked(id string, v bool) (prev bool, ok bool, err error) {
	m.intentMu.Lock()
	defer m.intentMu.Unlock()
	m.mu.RLock()
	lp, ok := m.byID[id]
	if !ok {
		m.mu.RUnlock()
		return false, false, nil
	}
	prev = lp.Config.SurplusOnly
	saver := m.surplusOnlySaver
	m.mu.RUnlock()
	if saver != nil && prev != v {
		if err := saver(id, v); err != nil {
			return prev, true, err
		}
	}
	m.mu.Lock()
	lp.Config.SurplusOnly = v
	m.mu.Unlock()
	return prev, true, nil
}

// SetSurplusOnlySaver wires the persistence callback. Pass nil to
// disable. The saver runs before each change without blocking state reads.
func (m *Manager) SetSurplusOnlySaver(saver func(id string, v bool) error) {
	m.intentMu.Lock()
	defer m.intentMu.Unlock()
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
	m.intentMu.Lock()
	defer m.intentMu.Unlock()
	for _, state := range m.States() {
		id := state.ID
		if v, ok := load(id); ok {
			m.mu.Lock()
			m.byID[id].Config.SurplusOnly = v
			m.mu.Unlock()
		}
	}
}

// SetSessionCapacityWh overrides the vehicle capacity for the rest of the
// session with a figure the car itself reported — OCPP 2.0.1
// NotifyEVChargingNeeds carries the EV's own battery capacity.
//
// Measured outranks configured, so this wins over both vehicle_capacity_wh and
// a vehicle profile applied for the same session, in either order. It shares
// the profile's session scope: plug-out restores the loadpoint's own value,
// because the next car may be a different one.
//
// Returns false for an unknown loadpoint id or a non-positive capacity.
func (m *Manager) SetSessionCapacityWh(id string, capacityWh float64) bool {
	if capacityWh <= 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok {
		return false
	}
	if !lp.capacityFromCar {
		lp.baseCapacityWh = lp.VehicleCapacityWh
	}
	lp.capacityFromCar = true
	lp.VehicleCapacityWh = capacityWh
	lp.updatedAtMs = m.now().UnixMilli()
	return true
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
	m.sessionMu.Lock()
	m.mu.Lock()
	lp, ok := m.byID[id]
	if !ok || !lp.pluggedIn || !finite(socPct) {
		m.mu.Unlock()
		m.persistSession(id)
		m.sessionMu.Unlock()
		return false
	}
	// A correction gives the planner another chance to offer energy.
	// Sustained refusal can re-arm after SessionCompletionTimeout.
	lp.chargingDeclined = false
	lp.notRequestingSince = time.Time{}
	at := m.now()
	reanchorSoCLocked(lp, socPct, at)
	soc, observer := lp.currentSoC, m.levelObserver
	m.mu.Unlock()
	m.persistSession(id)
	m.sessionMu.Unlock()
	if observer != nil {
		observer(id, soc, at)
	}
	return true
}

// AnchorVehicleSoC sets inferred SoC from a fresh BMS reading the caller
// already matched to this loadpoint. Between readings, delivered Wh
// moves the anchor. If anchoring stops, the last BMS value stays.
// False when the id is unknown or the car is unplugged.
func (m *Manager) AnchorVehicleSoC(id string, socPct float64) bool {
	return m.AnchorVehicleSoCAt(id, socPct, time.Time{})
}

// AnchorVehicleSoCAt anchors a qualified car reading at its measurement
// time. Known energy delivered since then moves the estimate forward. When
// history does not cover that time, adopt the reading without adding unknown
// energy. A zero observedAt means now.
func (m *Manager) AnchorVehicleSoCAt(id string, socPct float64, observedAt time.Time) bool {
	m.sessionMu.Lock()
	m.mu.Lock()
	var completion *events.ChargingSessionComplete
	bus := m.bus
	defer func() {
		m.mu.Unlock()
		m.persistSession(id)
		m.sessionMu.Unlock()
		if completion != nil {
			bus.Publish(*completion)
		}
	}()
	lp, ok := m.byID[id]
	if !ok {
		return false
	}
	if !lp.pluggedIn || !finite(socPct) || socPct < 0 || socPct > 1 {
		return false
	}
	deliveredAt := lp.deliveredWhSession
	if !observedAt.IsZero() && !observedAt.Before(lp.connectionObservedAt.Add(-anchorBeforePlugSlack)) {
		if known, ok := lp.deliveredAt(observedAt); ok {
			deliveredAt = known
		}
	}
	// A reading before this connection or outside retained history carries
	// no known energy delta. Keep its source time even in that raw fallback;
	// returning false made the caller re-anchor it as a reading taken now.
	reanchorSoCAtLocked(lp, socPct, deliveredAt, m.now())
	if !observedAt.IsZero() {
		// Compare car measurements by source time, not cloud delivery time.
		lp.socConfirmedAt = observedAt
	}
	lp.vehicleAnchored = true
	if lp.targetSoC > 0 && lp.currentSoC >= lp.targetSoC && !lp.completionNotified {
		lp.completionNotified = true
		completion = &events.ChargingSessionComplete{LoadpointID: id, KWh: lp.deliveredWhSession / 1000, At: m.now()}
	}
	return true
}

type deliveredSample struct {
	at time.Time
	wh float64
}

const (
	// deliveredHistoryKeep bounds how old a car reading may be and still
	// be anchored. Telemetry's VehicleAnchorMaxAge stays within it.
	deliveredHistoryKeep = 2 * time.Hour
	deliveredHistoryStep = 30 * time.Second
	// anchorBeforePlugSlack accepts a reading taken just before plug-in,
	// as live car drivers report a moment before the charger does.
	anchorBeforePlugSlack = 5 * time.Minute
)

// recordDelivered appends at most one sample per step and drops samples
// older than deliveredHistoryKeep. A counter that goes backwards starts a
// new history, since earlier samples no longer share its origin.
func (lp *loadpointRuntime) recordDelivered(now time.Time, wh float64) {
	h := lp.deliveredHistory
	if n := len(h); n > 0 {
		last := h[n-1]
		if wh < last.wh || now.Before(last.at) {
			h = h[:0]
		} else if now.Sub(last.at) < deliveredHistoryStep {
			return
		}
	}
	h = append(h, deliveredSample{at: now, wh: wh})
	cut := 0
	for cut < len(h)-1 && now.Sub(h[cut+1].at) >= deliveredHistoryKeep {
		cut++
	}
	if cut > 0 {
		h = append(h[:0], h[cut:]...)
	}
	lp.deliveredHistory = h
}

// deliveredAt returns the session energy at t, interpolated between
// samples. Before the first sample the energy is known only if that
// sample is still zero: nothing had been delivered yet.
func (lp *loadpointRuntime) deliveredAt(t time.Time) (float64, bool) {
	h := lp.deliveredHistory
	if len(h) == 0 {
		return 0, false
	}
	if t.Before(h[0].at) {
		return h[0].wh, h[0].wh == 0
	}
	last := h[len(h)-1]
	if !t.Before(last.at) {
		return lp.deliveredWhSession, true
	}
	for i := 1; i < len(h); i++ {
		if t.After(h[i].at) {
			continue
		}
		a, b := h[i-1], h[i]
		f := float64(t.Sub(a.at)) / float64(b.at.Sub(a.at))
		return a.wh + f*(b.wh-a.wh), true
	}
	return lp.deliveredWhSession, true
}

// reanchorSoCLocked re-bases the session anchor so the CURRENT estimate
// equals socPct, then recomputes current_soc from it. Caller must hold
// m.mu and have verified the loadpoint is plugged in. Shared by the
// manual (SetCurrentSoC) and automatic (AnchorVehicleSoC) correction
// paths so they stay arithmetically identical.
func reanchorSoCLocked(lp *loadpointRuntime, soc float64, at time.Time) {
	reanchorSoCAtLocked(lp, soc, lp.deliveredWhSession, at)
}

// reanchorSoCAtLocked sets the anchor so the estimate equals soc when the
// session had delivered deliveredWh. at is when this confirmation happened.
func reanchorSoCAtLocked(lp *loadpointRuntime, soc, deliveredWh float64, at time.Time) {
	soc = units.ClampFraction(soc)
	lp.socConfirmed = true
	lp.socUnknown = false
	if at.IsZero() {
		at = time.Now()
	}
	lp.socConfirmedAt = at
	// Re-anchor: new_anchor + delivered/capacity == soc.
	delivered := 0.0
	if lp.VehicleCapacityWh > 0 {
		delivered = deliveredWh * DefaultChargeEfficiency / lp.VehicleCapacityWh
	}
	// The offset may be negative when the corrected level is below the
	// energy already delivered. Clamp the resulting level, not the offset.
	lp.sessionPluginSoC = soc - delivered
	lp.currentSoC = estimateSoC(lp.sessionPluginSoC, lp.deliveredWhSession, lp.VehicleCapacityWh)
	lp.updatedAtMs = at.UnixMilli()
}

// VehicleSoCSupersedes reports whether a car measurement should replace the
// level the plan is using. An unconfirmed plug-in assumption always loses
// to a usable car reading. A confirmed level loses only when the measurement
// is newer than that confirmation.
func (m *Manager) VehicleSoCSupersedes(id string, observedAt time.Time) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lp := m.byID[id]
	if lp == nil || !lp.pluggedIn {
		return false
	}
	return vehicleSupersedesLocked(lp, observedAt)
}

func vehicleSupersedesLocked(lp *loadpointRuntime, observedAt time.Time) bool {
	if observedAt.IsZero() {
		return false
	}
	if !lp.socConfirmed || lp.socConfirmedAt.IsZero() {
		return true
	}
	return observedAt.After(lp.socConfirmedAt)
}

func (lp *loadpointRuntime) snapshot() State {
	steps := make([]float64, len(lp.AllowedStepsW))
	copy(steps, lp.AllowedStepsW)
	sort.Float64s(steps)
	st := State{
		ManualRestoreUnconfirmed: lp.manualRestoreUnconfirmed,
		ManualSaveError:          lp.manualSaveError,
		ManualSavePending:        lp.manualSavePending,
		VehicleCapacityWh:        lp.VehicleCapacityWh,
		CapacitySource:           "configured",
		ID:                       lp.ID,
		DriverName:               lp.DriverName,
		PluggedIn:                lp.pluggedIn,
		CurrentSoC:               lp.currentSoC,
		CurrentPowerW:            lp.currentPowerW,
		DeliveredWhSession:       lp.deliveredWhSession,
		TargetSoC:                lp.targetSoC,
		FinishAtVehicleLimit:     lp.finishAtVehicleLimit,
		GoalRetention:            lp.finishGoalRetention,
		GoalComplete:             lp.finishGoalCompleted,
		TargetTime:               lp.targetTime,
		UpdatedAtMs:              lp.updatedAtMs,
		MinChargeW:               lp.MinChargeW,
		MaxChargeW:               lp.MaxChargeW,
		AllowedStepsW:            steps,
		SurplusOnly:              lp.Config.SurplusOnly,
		Schedule:                 lp.schedule,
		ChargingDeclined:         lp.chargingDeclined,
		SoCRetention:             lp.socRetention,
		CarMaxW:                  lp.car.MaxW,
		CarChargeLimit:           lp.car.ChargeLimit,
		CommandedW:               lp.commandedW,
		CommandedReason:          lp.commandedReason,
		CommandedKnown:           lp.commandedKnown,
		CommandedPhases:          lp.commandedPhases,
		CarPhase:                 lp.carPhases.phase,
	}
	if lp.carPhases.phase > 0 {
		st.CarPhases = 1
	}
	if lp.VehicleCapacityWh <= 0 {
		st.VehicleCapacityWh = 60000
		st.CapacitySource = "default"
	}
	if lp.energy != nil {
		st.EnergySource = lp.energy.source
		if !lp.energy.counterAt.IsZero() {
			st.EnergyUpdatedAtMs = lp.energy.counterAt.UnixMilli()
		}
	}
	st.PowerUnavailable = lp.powerUnavailable
	if !lp.powerAt.IsZero() {
		st.PowerUpdatedAtMs = lp.powerAt.UnixMilli()
	}
	if st.PluggedIn {
		st.VehicleName = lp.car.Name
		st.Session = lp.car.Session
		st.CarID = lp.car.ID
		st.CarIdentifiedBy = lp.car.IdentifiedBy
		st.CarCorrected = lp.car.Corrected
		st.CarCorrectedFrom = lp.car.CorrectedFrom
		if lp.car.Choose != "" {
			st.Choose = strings.Split(lp.car.Choose, ",")
		}
		st.PluggedAt = lp.connectionObservedAt
		if lp.sessionDeviceID != "" && lp.sessionID != "" {
			st.SessionKey = lp.sessionDeviceID + "|" + lp.sessionID
		}
	}
	st.Observed = lp.observed
	st.LevelFromCar = lp.vehicleAnchored
	if lp.socConfirmed {
		st.SoCConfirmedAt = lp.socConfirmedAt
	}
	switch {
	case st.PluggedIn && lp.socUnknown:
		st.SoCUnknown = true
		st.SoCSource = "unknown"
	case st.PluggedIn && st.SoCSource == "" && !lp.socConfirmed:
		st.SoCSource = "assumed"
	}
	st.ManualCommandUpdatedAt = lp.manualCommandUpdatedAt
	if !lp.commandedSince.IsZero() {
		st.CommandedSinceMs = lp.commandedSince.UnixMilli()
	}
	return st
}

// SetScheduleSaver wires the persistence callback. Pass nil to disable.
// Safe to call before or after Load().
func (m *Manager) SetScheduleSaver(saver func(id string, s Schedule) error) {
	m.intentMu.Lock()
	defer m.intentMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scheduleSaver = saver
}

// SetSchedule stores the operator's intent. It returns false for an unknown
// loadpoint or a failed save. Use SetScheduleChecked to distinguish them.
func (m *Manager) SetSchedule(id string, s Schedule) bool {
	ok, err := m.SetScheduleChecked(id, s)
	return ok && err == nil
}

// SetScheduleChecked saves before changing the active goal. On storage
// failure the previous schedule and derived target remain in effect.
// The callback runs without m.mu so readers can keep seeing the current goal.
func (m *Manager) SetScheduleChecked(id string, s Schedule) (bool, error) {
	return m.setSchedule(id, s, true)
}

// setSchedule stores a goal; save runs the schedule saver first.
func (m *Manager) setSchedule(id string, s Schedule, save bool) (bool, error) {
	return m.setScheduleAs(id, func(Schedule) (Schedule, bool) { return s, true }, save)
}

// setScheduleWith stores the goal next returns from the loadpoint's current
// schedule, read under intentMu, without the saver. next reports false to
// leave the schedule alone.
func (m *Manager) setScheduleWith(id string, next func(cur Schedule) (Schedule, bool)) (bool, error) {
	return m.setScheduleAs(id, next, false)
}

func (m *Manager) setScheduleAs(id string, next func(cur Schedule) (Schedule, bool), save bool) (bool, error) {
	m.intentMu.Lock()
	defer m.intentMu.Unlock()
	m.mu.RLock()
	lp, ok := m.byID[id]
	saver := m.scheduleSaver
	now, loc := m.now(), m.loc
	var cur Schedule
	if ok {
		cur = lp.schedule
	}
	m.mu.RUnlock()
	if !ok {
		return false, nil
	}
	s, change := next(cur)
	if !change {
		return true, nil
	}
	s.Normalize()
	// Keep the household clock the owner picked: time_of_day_min when the
	// request has it, else the UTC minute read on today's date. Clients
	// still read its UTC minute today. A cleared goal has no ready time.
	if s.Empty() {
		s = s.withoutClock()
	} else {
		s.readClock(now, loc)
		s.TimeOfDayMinUTC = utcMinuteToday(now, s.clock, loc)
	}
	if s.FinishAtVehicleLimit {
		s.IntentID = rand.Text()
		if !s.Recurring {
			s.FirstDeadlineMS = s.NextDeadlineUTC(now, loc).UnixMilli()
		} else {
			s.FirstDeadlineMS = 0
		}
	} else {
		s.IntentID = ""
		s.FirstDeadlineMS = 0
	}
	// The weekday mask is 7 bits; a stray high bit from a future
	// client is dropped rather than left to confuse the roll.
	s.Days &= 0x7F
	if saver != nil && save {
		if err := saver(id, s); err != nil {
			return true, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Load shares intentMu, so the configured loadpoint cannot change
	// between the save and the in-memory update.
	if s.SoC > lp.schedule.SoC {
		lp.chargingDeclined = false
		lp.notRequestingSince = time.Time{}
	}
	lp.schedule = s
	lp.finishGoalCompleted = false
	lp.finishGoalSavedCompleted = false
	lp.finishGoalExplicit = true
	lp.finishGoalSaved = time.Time{}
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
	lp.targetSoC = 0
	lp.finishAtVehicleLimit = false
	return true, nil
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
// for unknown IDs or a failed save.
func (m *Manager) ClearSchedule(id string) bool {
	ok, err := m.ClearScheduleChecked(id)
	return ok && err == nil
}

// ClearScheduleChecked keeps the previous goal when its removal cannot save.
func (m *Manager) ClearScheduleChecked(id string) (bool, error) {
	// Removing the goal also removes its active derived deadline. Leaving
	// that target behind would keep planning a charge the UI says was removed.
	// Manual holds belong to the controller and are unaffected.
	return m.SetScheduleChecked(id, Schedule{})
}

// HydrateSchedules loads persisted schedules at boot. `loader(id)`
// returns the stored schedule for each configured loadpoint; missing
// entries return (Schedule{}, false). Unknown IDs in storage are
// silently ignored — the operator may have renamed a loadpoint, and
// resurrecting a schedule under the new ID would be surprising.
//
// A row saved before Core kept the household clock holds only a UTC
// minute, which does not say what offset it was saved with. It is read
// on today's date — the time the owner sees now — and saved back once so
// the next DST change cannot move it. A clock out of range is read the
// same way. The first failed save is returned; the goal is used either
// way and read again at the next boot. Does NOT call RollSchedules; the
// controller's first tick will handle that.
func (m *Manager) HydrateSchedules(loader func(id string) (Schedule, bool)) error {
	m.intentMu.Lock()
	defer m.intentMu.Unlock()
	var failed error
	m.mu.RLock()
	saver := m.scheduleSaver
	now, loc := m.now(), m.loc
	m.mu.RUnlock()
	for _, state := range m.States() {
		id := state.ID
		s, found := loader(id)
		if !found || s.Empty() {
			continue
		}
		s.Normalize()
		if s.readClock(now, loc) && saver != nil {
			if err := saver(id, s); err != nil && failed == nil {
				failed = err
			}
		}
		m.mu.Lock()
		m.byID[id].schedule = s
		m.mu.Unlock()
	}
	return failed
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
		// An unfinished vehicle-limit goal remains due after its deadline.
		// Moving it to tomorrow would defer the remaining charge again.
		if lp.finishGoalCompleted {
			if !s.Recurring || lp.targetTime.After(now) {
				continue
			}
			lp.finishGoalCompleted = false
			lp.targetTime = time.Time{}
		}
		if lp.finishAtVehicleLimit && lp.pluggedIn && !lp.chargingDeclined && !lp.targetTime.IsZero() {
			continue
		}
		next := s.NextDeadlineUTC(now, m.loc)
		if s.FinishAtVehicleLimit && !s.Recurring && s.FirstDeadlineMS > 0 {
			next = time.UnixMilli(s.FirstDeadlineMS)
		}
		if s.Recurring {
			if !lp.targetTime.IsZero() && lp.targetTime.After(now) {
				continue
			}
			lp.targetTime = next
			lp.finishAtVehicleLimit = s.FinishAtVehicleLimit
			lp.targetSoC = s.SoC
			if s.FinishAtVehicleLimit {
				lp.targetSoC = 1
			}
			lp.lastRolledFor = next
			continue
		}
		// Non-recurring: seed exactly once per SetSchedule. The Empty
		// SetSchedule path (clear) also resets lastRolledFor, so a
		// re-save with a non-recurring schedule re-seeds.
		if lp.lastRolledFor.IsZero() {
			lp.targetTime = next
			lp.finishAtVehicleLimit = s.FinishAtVehicleLimit
			lp.targetSoC = s.SoC
			if s.FinishAtVehicleLimit {
				lp.targetSoC = 1
			}
			lp.lastRolledFor = next
		}
	}
}

// RetryCharging lets an explicit Start action retry a vehicle that previously
// declined current. It changes no battery level or stored user intent.
func (m *Manager) RetryCharging(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lp := m.byID[id]; lp != nil {
		lp.chargingDeclined = false
		lp.notRequestingSince = time.Time{}
	}
}
