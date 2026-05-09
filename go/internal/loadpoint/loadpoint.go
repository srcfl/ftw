// Package loadpoint models an EV charge point as a first-class entity
// the planner can reason about. A loadpoint couples a physical charger
// driver (Easee, Zap, OCPP, …) with a specific vehicle and user intent
// (target SoC by target time).
//
// This package currently hosts the config-facing types and a read-only
// manager that surfaces configured loadpoints through the API. Phase 3
// of the planner overhaul introduces the skeleton without wiring it to
// the MPC's decision surface — that comes in Phase 4, where the DP is
// extended with EV-SoC state and the dispatch layer gains a per-
// loadpoint energy-budget path that mirrors the battery energy path.
//
// Keeping it lightweight is intentional: EVCC ships ~20 kLOC of
// loadpoint machinery (hysteresis, enable/disable delays, phase
// switching). We don't need most of that because the energy-budget
// contract is continuous by construction (no flap-flapping). Phase
// switching is a driver-local heuristic, not a planner concern.
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

	// SurplusOnly mirrors Config.SurplusOnly with any runtime override
	// (set via POST /api/loadpoints/{id}/target). Always emitted (no
	// omitempty) so a polling client can distinguish "explicitly off"
	// from "field absent because the server is too old to know".
	SurplusOnly bool `json:"surplus_only"`
}

// Manager holds the running set of loadpoints. Thread-safe.
type Manager struct {
	mu     sync.RWMutex
	byID   map[string]*loadpointRuntime
	order  []string // insertion-preserving id list for deterministic listing
}

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
// No-op for unknown IDs — a misconfigured driver shouldn't crash the
// manager.
func (m *Manager) Observe(id string, pluggedIn bool, powerW, deliveredWh float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok {
		return
	}
	if pluggedIn && !lp.pluggedIn {
		// Plug-in transition: seed the session anchor.
		anchor := lp.PluginSoCPct
		if anchor <= 0 {
			anchor = 20 // conservative default
		}
		lp.sessionPluginSoCPct = anchor
	}
	lp.pluggedIn = pluggedIn
	lp.currentPowerW = powerW
	lp.deliveredWhSession = deliveredWh
	if pluggedIn {
		lp.currentSoCPct = estimateSoCPct(lp.sessionPluginSoCPct,
			deliveredWh, lp.VehicleCapacityWh)
	} else {
		lp.currentSoCPct = 0
	}
	lp.updatedAtMs = time.Now().UnixMilli()
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
	defer m.mu.Unlock()
	lp, ok := m.byID[id]
	if !ok {
		return false, false
	}
	prev = lp.Config.SurplusOnly
	lp.Config.SurplusOnly = v
	return prev, true
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
	return true
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
	}
}
