package control

import (
	"encoding/json"
	"log/slog"
	"math"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/mpc"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Mode is the operating mode of the control loop.
type Mode string

const (
	ModeIdle            Mode = "idle"
	ModeSelfConsumption Mode = "self_consumption"
	ModePeakShaving     Mode = "peak_shaving"
	ModeCharge          Mode = "charge"
	ModePriority        Mode = "priority"
	ModeWeighted        Mode = "weighted"

	// Planner modes: control loop pulls GridTargetW from the MPC plan
	// for the current 15-min slot. If the plan is stale (>30 min) or
	// missing, we fall back to self_consumption behavior and log.
	// The three flavors mirror mpc.Mode — the difference is only what
	// the planner is allowed to do when it builds the plan:
	//   - planner_self:      no grid-charging, no export discharge
	//   - planner_cheap:     grid-charge ok, no export discharge
	//   - planner_arbitrage: full freedom within SoC + power limits
	ModePlannerSelf      Mode = "planner_self"
	ModePlannerCheap     Mode = "planner_cheap"
	ModePlannerArbitrage Mode = "planner_arbitrage"
)

// IsPlannerMode reports whether the mode is one of the planner modes.
func (m Mode) IsPlannerMode() bool {
	return m == ModePlannerSelf || m == ModePlannerCheap || m == ModePlannerArbitrage
}

// PlanTargetFunc is injected by main.go: given the current time, returns
// the plan's directive for the current slot. When ok=false, the plan is
// stale/missing and the control loop falls back to self_consumption with
// grid_target=0.
//
// Returns (mode_string, grid_target_w, ok). mode_string maps to a Mode
// constant; the dispatch uses its existing mode logic for HOW batteries
// respond. The plan is a scheduler, not a regulator.
//
// Legacy — the new contract (energy-allocation per slot, EMS converts to
// power) uses SlotDirectiveFunc. See docs/plan-ems-contract.md.
type PlanTargetFunc func(now time.Time) (string, float64, bool)

// SlotDirective mirrors mpc.SlotDirective — we redefine here to keep the
// control package import-cycle free. Populated by main.go's injected
// SlotDirectiveFunc adapter.
type SlotDirective struct {
	SlotStart       time.Time
	SlotEnd         time.Time
	BatteryEnergyWh float64 // site-signed: + = charge, − = discharge
	SoCTargetPct    float64
	Strategy        string  // echoed for logging / API; mirrors mpc.Mode
	PVLimitW        float64 // 0 = no curtail; > 0 = cap aggregate PV output
}

// SlotDirectiveFunc returns the plan's energy-allocation directive for
// the slot containing `now`. When ok=false the plan is stale or missing
// and the control loop falls back to auto_fallback (local self-consumption
// rule with no forward-planning) — same behavior as PlanTargetFunc's
// stale-plan branch.
type SlotDirectiveFunc func(now time.Time) (SlotDirective, bool)

// MaxCommandW is the default per-command power cap (±5 kW), applied when
// a driver has no per-battery override. A deliberate floor-to-conservative
// pick for v0.2x: safer than guessing a driver's headroom wrong. Override
// on a per-driver basis via `config.Driver.max_charge_w` /
// `max_discharge_w` (see PowerLimits + State.DriverLimits, issue #145).
const MaxCommandW = 5000

// evActiveThresholdW is the floor below which state.EVChargingW is
// treated as driver-side noise (a connected-but-idle wallbox bleeds a
// few watts of last-known smoothed reading after the contactor
// opens). Above the threshold, the BatteryCoversEV=false safety net
// caps battery discharge to the house side so the planner doesn't
// drain the battery into the EV. Below it, the EV is effectively
// idle and the safety net stays off, letting the planner's evening-
// peak export plan run unmolested. 100 W matches the deadband used
// elsewhere in this package for sign decisions.
const evActiveThresholdW = 100.0

// PowerLimits holds the per-driver charge/discharge ceiling. Zero on
// either field means "use the global MaxCommandW default" — the value
// an unset config key carries through the YAML → Driver struct →
// dispatch map pipeline. A non-zero value overrides the default at
// every clamp point (clampWithSoC and the post-slew re-clamp).
//
// A per-driver cap higher than the site fuse doesn't buy extra throughput:
// the fuse-guard still scales at the site boundary (#145 safety invariant).
type PowerLimits struct {
	MaxChargeW    float64
	MaxDischargeW float64
}

// DispatchTarget is one command to issue to a single battery driver.
// `TargetW` is in site sign convention:
//   + = charge the battery (battery becomes a load, site imports more)
//   − = discharge the battery (battery becomes a source, site imports less)
type DispatchTarget struct {
	Driver  string  `json:"driver"`
	TargetW float64 `json:"target_w"`
	Clamped bool    `json:"clamped"`
}

// CurtailTarget is a per-driver PV curtailment command. Emitted by
// ComputePVCurtail when the active plan slot's PVLimitW signals that
// exporting more PV than the limit would lose money (negative-spot
// hours with no positive feed-in tariff, plus zero export bonus).
//
// LimitW=0 means "release any previous curtail" — main.go translates
// the zero-limit form into a `curtail_disable` action so the driver
// returns to its default operating mode. LimitW>0 dispatches as
// `curtail` with the magnitude as the absolute power cap.
type CurtailTarget struct {
	Driver string  `json:"driver"`
	LimitW float64 `json:"limit_w"`
}

// State holds all persistent state for one instance of the control loop.
// One per site.
type State struct {
	Mode           Mode
	GridTargetW    float64
	GridToleranceW float64
	SiteMeterDriver string

	// SiteFuseAmps is the per-phase trip current of the site's main
	// breaker (cfg.Fuse.MaxAmps). Used by the per-phase clamp inside
	// applyFuseGuard / forceFuseDischarge: when the meter reports
	// per-phase amps via DerReading.Data (l1_a / l2_a / l3_a, emitted
	// by Pixii / Ferroamp / Sungrow), any single phase exceeding
	// SiteFuseAmps is treated as additional aggregate overage so the
	// existing scaling+discharge logic responds.
	//
	// Zero disables the per-phase clamp (back-compat for tests and
	// sites without per-phase meter data).
	SiteFuseAmps    float64
	SiteFuseVoltage float64
	SiteFusePhases  int

	// SiteFuseSafetyA is the headroom (in amps) the dispatch keeps
	// below the breaker's nominal trip current. Phase amps are clamped
	// at SiteFuseAmps − SiteFuseSafetyA on both directions. Without
	// this margin, hardware-side per-phase protection inside the
	// inverter (e.g. Pixii's local current limiter) trips before the
	// dispatch sees the aggregate go over fuse — the inverter cuts
	// output to 0 in one tick and the dispatch then has to ramp from
	// idle, producing a visible flap. Defaults to 0.5 A wired in main.
	// Zero disables the margin (back-compat).
	SiteFuseSafetyA float64

	// For Priority mode
	PriorityOrder []string
	// For Weighted mode
	Weights map[string]float64

	// Peak limit — enforced only in PeakShaving mode
	PeakLimitW float64

	// PeakImportCeilingW is a hard import ceiling enforced in EVERY mode,
	// parallel to (and at or below) the physical fuse. Default 0 = disabled.
	// When > 0, the import side of every clamp (applyFuseGuard, joint
	// EV/battery allocator, forceFuseDischarge) uses min(fuse, peak)
	// as its threshold; the battery covers transient overruns while
	// the loadpoint controller ramps the EV down. Steady-state with
	// BatteryCoversEV=false: EV settles at its throttled rate and the
	// battery stops being commanded to discharge — the bridge is brief
	// by construction, not by an explicit timer. Steady-state with
	// BatteryCoversEV=true: the planner is already aware EV draw is in
	// scope, no additional behaviour here.
	//
	// Export side and per-phase clamp use fuseMaxW unchanged — peak is
	// an aggregate import-only concept (tariff). Persisted in state.db
	// under "peak_import_ceiling_w".
	PeakImportCeilingW float64
	// EV charging signal — batteries won't try to cover this much of import
	EVChargingW float64

	// EVSurplusOnlyReserveW is the aggregate PV headroom that must be
	// kept available for EVs under surplus_only loadpoints. When > 0:
	//   - the energy-allocation path caps battery aggregate charge so
	//     it doesn't consume PV that the EV could be claiming
	//   - the legacy PI / self-consumption path biases the grid setpoint
	//     so it leaves `reserveRemaining` of export untouched
	// Where reserveRemaining = max(0, EVSurplusOnlyReserveW - EVChargingW)
	// — once the EV has ramped up to the reserve, no further headroom is
	// withheld from the battery. Populated each tick by main.go from
	// loadpoint.Manager.States(): sum of MaxChargeW across LPs that are
	// SurplusOnly && PluggedIn. Set to 0 when no such LP is connected,
	// in which case all behaviour reverts to the pre-existing path.
	EVSurplusOnlyReserveW float64
	// BatteryCoversEV overrides the default EV-exclusion behaviour. When
	// false (default), EVChargingW is subtracted from the meter reading
	// before the PI runs so batteries don't shuffle energy through the
	// inverter to feed the EV on a normal day. When true, the subtraction
	// is skipped and the battery is free to discharge into the EV up to
	// its own SoC / power / fuse clamps — useful in price-arbitrage
	// situations where the operator wants to drain the battery now and
	// refill it later from cheap solar. Persisted in state.db via
	// "battery_covers_ev"; toggled from HA and POST /api/battery_covers_ev.
	BatteryCoversEV bool

	// PI controller (outer, site-level)
	PI *PIController

	// Slew + holdoff
	SlewRateW            float64
	MinDispatchIntervalS int
	LastDispatch         *time.Time
	PrevTargets          map[string]float64

	LastTargets []DispatchTarget

	// Cascade toggle — set by main.go based on whether models exist
	UseCascade bool

	// PlanTarget is consulted at the top of each control cycle when
	// Mode is a planner mode. Nil outside planner modes. Injected from
	// main.go — the control package doesn't need to know about mpc.
	//
	// Legacy (grid-target driven). The new path uses SlotDirective.
	PlanTarget PlanTargetFunc

	// SlotDirective is the new plan→EMS callback (energy-allocation
	// contract). When set AND UseEnergyDispatch is true, ComputeDispatch
	// uses the energy-driven code path instead of the PI-on-grid-target
	// path. Injected from main.go like PlanTarget. See
	// docs/plan-ems-contract.md.
	SlotDirective SlotDirectiveFunc

	// UseEnergyDispatch toggles between the legacy PI-on-grid path and
	// the new energy-allocation path. False until validated in production
	// and flipped via config. Default off preserves today's behavior.
	UseEnergyDispatch bool

	// PVSurplusAbsorbSoCCapPct is the opt-in PV-surplus absorber
	// underlay: in planner_cheap / planner_arbitrage, when live grid is
	// exporting more than PVSurplusAbsorbThresholdW BEYOND what the
	// planner's slot allocation would produce, AND the fleet's average
	// SoC is below this cap, the energy-path's targetTotalW is bumped
	// up by min(extra_export, cap_headroom_W) so the surprise lands in
	// the battery instead of crossing the meter at low spot price.
	//
	// 0 = disabled (default — preserves the pre-2026-05 behavior
	// asserted by TestEnergyDispatchDoesNotAbsorbPVSurprise). >0 turns
	// the feature on with that percentage as the SoC ceiling.
	//
	// Never reverses a discharge plan: when the planner has already
	// committed to discharge this slot (targetTotalW < 0, e.g.
	// evening-peak export), the absorber stays passive.
	PVSurplusAbsorbSoCCapPct float64

	// PVSurplusAbsorbThresholdW is the dead-band on the absorber: how
	// much live export beyond plan we tolerate before charging the
	// battery instead. Defaults to 100 W (smaller than the PI
	// deadband, but large enough to ignore meter quantisation noise).
	// Only consulted when PVSurplusAbsorbSoCCapPct > 0.
	PVSurplusAbsorbThresholdW float64

	// currentDirective + slotDelivered track the active slot's energy
	// accounting. Reset when the slot rolls over (by SlotStart equality).
	// Zero-valued until UseEnergyDispatch fires its first cycle.
	currentDirective SlotDirective
	slotDelivered    float64   // Wh delivered to batteries since slot start
	lastTickTs       time.Time // for ∫ battery_w dt

	// PlanStale tracks whether the last cycle fell back to self_consumption
	// because the plan was missing. Surfaced via the API for the UI.
	PlanStale bool

	// InverterGroups maps driver name → inverter-group tag (e.g.
	// "ferroamp", "sungrow"). Drivers sharing a tag are assumed to
	// share a single inverter unit: their PV readings feed DC-direct
	// into the same-group battery. During charging, `distributeProportional`
	// prefers routing the total first to batteries whose group also
	// has live PV output, so a kWh doesn't cross inverters through
	// the AC bus (DC→AC→AC→DC ≈ 3-4 pp loss vs DC-local). Nil or empty
	// preserves the capacity-proportional default. Issue #143.
	InverterGroups map[string]string

	// SupportsPVCurtail flags drivers whose lua advertises a "curtail"
	// action (sungrow, ferroamp, deye, huawei, …). ComputePVCurtail
	// only dispatches to flagged drivers; an EV charger or a meter
	// driver wouldn't know what to do with a `curtail` payload.
	// Populated from config.Driver.SupportsPVCurtail in main.go;
	// hot-swappable via the config-reload watcher.
	SupportsPVCurtail map[string]bool

	// LastCurtailedDrivers remembers which drivers got a non-zero
	// curtail dispatch on the previous tick. ComputePVCurtail uses
	// the diff to emit a `curtail_disable` exactly once when the
	// plan no longer wants curtailment — without this we'd silently
	// leave the inverter capped after a slot rolls over.
	LastCurtailedDrivers map[string]bool

	// FuseEVMaxW is the joint allocator's verdict for the EV's allowed
	// wattage this tick. Only meaningful when FuseSaturated is true.
	// Read by the loadpoint controller (via a hook) to curtail the EV
	// command so battery and EV cooperatively share the fuse budget.
	// Recomputed every dispatch tick.
	FuseEVMaxW float64
	// FuseSaturated signals that the joint allocator had to scale battery
	// and/or EV demand to fit under the fuse this tick. Used to gate the
	// loadpoint controller's read of FuseEVMaxW and as the trigger for
	// an MPC reactive replan from main.go.
	FuseSaturated bool

	// DriverLimits maps driver name → per-battery charge/discharge cap.
	// Missing entries (or zero fields) fall through to the global
	// MaxCommandW default. Consulted in every clamp step — per-battery
	// clampWithSoC, post-slew re-clamp, and fuse-guard's reference to
	// total headroom. Hot-swappable via the config-reload watcher.
	// Issue #145.
	DriverLimits map[string]PowerLimits

	// FuseHold* — hysteresis state from applyFuseGuard. After a clamp
	// fires, the latched maximum aggregate-battery magnitude (for the
	// direction that tripped) is held for ~30 s so the planner can't
	// immediately re-ramp into the same threshold and oscillate at
	// the boundary. Each new fire extends the window.
	//
	// Without this, the dispatch + slew + planner feedback loop
	// stabilises with phase amps riding right at the trip threshold:
	// every tick scales the discharge JUST enough to clear the trip,
	// the next tick the planner re-asks for more, and the system
	// hovers permanently at the boundary instead of leaving headroom.
	FuseHoldMaxDischargeW float64
	FuseHoldMaxChargeW    float64
	FuseHoldUntil         time.Time

	// ManualHold pins the aggregate battery setpoint to a fixed power
	// for a bounded duration, bypassing both the active manual mode
	// and the MPC. Hot-installed via POST /api/battery/manual_hold;
	// auto-expires. Zero ExpiresAt means inactive.
	//
	// Site sign convention: PowerW > 0 = charge, < 0 = discharge,
	// 0 = idle. SoC clamps, slew, and the fuse guard still apply on
	// the resulting target — operators cannot bypass safety bounds.
	// Mutated under the same outer ctrlMu that protects the rest of
	// State; no internal mutex.
	ManualHold BatteryManualHold
}

// BatteryManualHold is the full payload of a battery manual override.
// See State.ManualHold for invariants.
type BatteryManualHold struct {
	PowerW    float64
	ExpiresAt time.Time
}

// SetBatteryManualHold installs a manual override on the aggregate
// battery setpoint. Caller must hold the outer ctrlMu. A zero
// ExpiresAt clears any active hold (same as ClearBatteryManualHold).
func (s *State) SetBatteryManualHold(h BatteryManualHold) {
	if h.ExpiresAt.IsZero() {
		s.ManualHold = BatteryManualHold{}
		return
	}
	s.ManualHold = h
}

// ClearBatteryManualHold removes any active hold regardless of expiry.
// Idempotent. Caller must hold the outer ctrlMu.
func (s *State) ClearBatteryManualHold() {
	s.ManualHold = BatteryManualHold{}
}

// GetBatteryManualHold returns the active hold for `now`, lazily
// evicting an expired one. Caller must hold the outer ctrlMu.
func (s *State) GetBatteryManualHold(now time.Time) (BatteryManualHold, bool) {
	if s.ManualHold.ExpiresAt.IsZero() {
		return BatteryManualHold{}, false
	}
	if !now.Before(s.ManualHold.ExpiresAt) {
		s.ManualHold = BatteryManualHold{}
		return BatteryManualHold{}, false
	}
	return s.ManualHold, true
}

// NewState creates default control state (port of Rust ControlState::new).
func NewState(gridTargetW, gridToleranceW float64, siteMeter string) *State {
	pi := NewPI(0.5, 0.1, 3000, 10000)
	pi.Setpoint = gridTargetW
	return &State{
		Mode:                 ModeSelfConsumption,
		GridTargetW:          gridTargetW,
		GridToleranceW:       gridToleranceW,
		SiteMeterDriver:      siteMeter,
		PriorityOrder:        nil,
		Weights:              map[string]float64{},
		PeakLimitW:           5000,
		EVChargingW:          0,
		PI:                   pi,
		SlewRateW:            500,
		MinDispatchIntervalS: 5,
		PrevTargets:          map[string]float64{},
		UseCascade:           true,
	}
}

// SetGridTarget updates both the state and the PI setpoint.
func (s *State) SetGridTarget(w float64) {
	s.GridTargetW = w
	s.PI.Setpoint = w
}

// batteryInfo is internal state read from telemetry per dispatch cycle.
type batteryInfo struct {
	driver        string
	capacityWh    float64
	currentW      float64
	soc           float64
	online        bool
	group         string  // inverter-affinity tag; empty = untagged (#143)
	maxChargeW    float64 // per-driver cap; 0 = use MaxCommandW default (#145)
	maxDischargeW float64 // per-driver cap; 0 = use MaxCommandW default (#145)
}

// chargeCap returns the effective per-battery charge ceiling, falling
// back to MaxCommandW when the driver didn't set an explicit limit.
// Kept a method so every clamp point queries the same fallback rule.
func (b batteryInfo) chargeCap() float64 {
	if b.maxChargeW > 0 {
		return b.maxChargeW
	}
	return MaxCommandW
}

// dischargeCap is the symmetric version of chargeCap for discharge
// targets. Returned as a positive magnitude; callers apply the minus
// sign at the comparison site.
func (b batteryInfo) dischargeCap() float64 {
	if b.maxDischargeW > 0 {
		return b.maxDischargeW
	}
	return MaxCommandW
}

// ComputeDispatch runs one cycle of the control loop and returns the targets
// to issue. Caller is expected to pass them to drivers.
//
// driverCapacities: map of driver name → battery capacity in Wh. Only drivers
// present here are considered for battery dispatch.
//
// fuseMaxW: total site current budget (amps × volts × phases).
func ComputeDispatch(
	store *telemetry.Store,
	state *State,
	driverCapacities map[string]float64,
	fuseMaxW float64,
) []DispatchTarget {
	// ---- Planner modes: the plan is a scheduler, not a regulator ----
	// The plan decides WHEN each strategy applies (self-consumption now,
	// charge at 02:00, export at 17:00). The EMS decides HOW batteries
	// respond every 5 s based on the live meter.
	//
	// Three execution paths, selected by the operator-picked planner mode:
	//
	//   * planner_self — reactive self-consumption (PI → gridW=0) with a
	//     per-slot idle gate from the plan. Honours the mode's contract
	//     ("never imports to charge, never exports via the battery")
	//     against forecast error. See docs/plan-ems-contract.md §"Exception:
	//     planner_self" and issue #130.
	//
	//   * planner_cheap / planner_arbitrage with UseEnergyDispatch=true
	//     (default): energy-allocation. Plan returns battery energy for
	//     the slot; EMS converts to instantaneous power from
	//     (remaining_wh / remaining_s); grid flow is the residual.
	//
	//   * planner_cheap / planner_arbitrage with UseEnergyDispatch=false
	//     (opt-out): legacy PI-on-grid-target path. Plan returns
	//     grid_target_w; PI chases it.
	//
	// All three share gather-batteries → distribute → slew → fuse below.
	// They differ only in how `totalCorrection` is computed and whether
	// the deadband applies.
	effectiveMode := state.Mode
	useEnergyPath := false
	// plannerSelfIdleGate is true when operator picked planner_self AND the
	// plan allocated a below-threshold amount of battery action for the
	// current slot — the EMS holds the battery at 0 (ramping via slew)
	// regardless of live surplus, so the DP's decision to save SoC for a
	// later slot is honoured.
	plannerSelfIdleGate := false
	var currentDirective SlotDirective

	// ---- Manual hold: highest-priority override ----
	// Operator pinned a fixed aggregate battery setpoint (charge /
	// discharge / idle) for a bounded duration. Skips the planner-mode
	// pre-processing, the idle/charge short-circuits, the holdoff timer,
	// and the deadband. Falls through to distribute → slew → SoC clamp
	// → fuse guard so safety bounds still apply.
	manualHold, manualHoldActive := state.GetBatteryManualHold(time.Now())
	if manualHoldActive {
		// Reset PI + slot accumulators so reverting to a planner mode
		// after the hold expires doesn't read stale state — same reset
		// that the ModePlannerSelf branch performs below.
		state.PI.Reset()
		state.currentDirective = SlotDirective{}
		state.slotDelivered = 0
		state.lastTickTs = time.Time{}
		state.PlanStale = false
		state.SetGridTarget(0)
		// Force the proportional distribution path. effectiveMode stays
		// SelfConsumption so the idle/charge short-circuits below don't
		// fire when state.Mode is one of those.
		effectiveMode = ModeSelfConsumption
	}
	switch {
	case manualHoldActive:
		// Already handled — leave effectiveMode at ModeSelfConsumption.
	case state.Mode == ModePlannerSelf:
		effectiveMode = ModeSelfConsumption
		state.SetGridTarget(0)
		// Reset the energy-allocation bookkeeping so a future switch to
		// planner_cheap / planner_arbitrage within the same 15-minute
		// slot can't read stale `slotDelivered` accumulated before the
		// operator hopped through planner_self. Without this reset, the
		// `SlotStart` comparison in the energy path would match (still
		// the same clock-aligned slot) and skip its own rollover reset
		// — reading the pre-hop delivered-Wh number and over-commanding
		// charge/discharge for the rest of the slot. Codex P2 on PR #131.
		state.currentDirective = SlotDirective{}
		state.slotDelivered = 0
		state.lastTickTs = time.Time{}
		planFresh := false
		if state.SlotDirective != nil {
			if dir, ok := state.SlotDirective(time.Now()); ok {
				planFresh = true
				state.PlanStale = false
				slotH := dir.SlotEnd.Sub(dir.SlotStart).Hours()
				if slotH > 0 && math.Abs(dir.BatteryEnergyWh)/slotH < mpc.IdleGateThresholdW {
					plannerSelfIdleGate = true
				}
			}
		}
		if !planFresh {
			if !state.PlanStale {
				slog.Warn("planner_self: plan stale — reactive self_consumption, no idle gates")
			}
			state.PlanStale = true
		}
	case state.Mode.IsPlannerMode():
		// planner_cheap / planner_arbitrage.
		if state.UseEnergyDispatch && state.SlotDirective != nil {
			if dir, ok := state.SlotDirective(time.Now()); ok {
				currentDirective = dir
				useEnergyPath = true
				// Distribution mode is decoupled from planner strategy in
				// the energy path — the operator-selected strategy drives
				// the plan's DP, distribution is always proportional across
				// online batteries. If the operator wants priority or
				// weighted, they use the manual modes, not a planner mode.
				effectiveMode = ModeSelfConsumption
				state.PlanStale = false
			}
		}
		if !useEnergyPath {
			var modeStr string
			var gridW float64
			ok := false
			if state.PlanTarget != nil {
				modeStr, gridW, ok = state.PlanTarget(time.Now())
			}
			if ok {
				effectiveMode = Mode(modeStr)
				state.SetGridTarget(gridW)
				state.PlanStale = false
			} else {
				if !state.PlanStale {
					slog.Warn("mpc plan stale — falling back to self_consumption")
				}
				effectiveMode = ModeSelfConsumption
				state.SetGridTarget(0)
				state.PlanStale = true
			}
		}
	}

	// ---- Idle + Charge short-circuits ----
	switch effectiveMode {
	case ModeIdle:
		// Even in idle, the reactive fuse-saver runs: an unplanned
		// load (manual_hold injecting EV power, oven turning on,
		// neighbour's pool pump on the same fuse) can push grid
		// import past the fuse with the battery sitting at 0 W. The
		// fuse-saver overrides idle and forces discharge.
		out := fuseSaverFromZero(store, state, driverCapacities, fuseMaxW)
		// LastTargets reflects what we actually issued: nil when
		// the fuse-saver no-op'd, the discharge targets when it
		// fired. /api/status, the history snapshot, and the RLS
		// model loop all depend on this being accurate.
		state.LastTargets = out
		if out != nil {
			now := time.Now()
			state.LastDispatch = &now
		}
		return out
	case ModeCharge:
		targets := chargeAll(store, driverCapacities, state.DriverLimits)
		state.LastTargets = targets
		return targets
	}

	// ---- Holdoff ----
	// Manual holds bypass the holdoff: the operator just installed a
	// setpoint and expects immediate effect, not a 5 s wait. The fuse
	// guard at the end of the cycle still protects the site.
	if !manualHoldActive && state.LastDispatch != nil {
		elapsed := time.Since(*state.LastDispatch).Seconds()
		if elapsed < float64(state.MinDispatchIntervalS) {
			// Holdoff suppresses normal re-dispatch, but the
			// fuse-saver overrides — an overflow can't wait 5 s for
			// the next eligible tick.
			out := fuseSaverFromZero(store, state, driverCapacities, fuseMaxW)
			if out != nil {
				// Same bookkeeping as the idle path so downstream
				// consumers (status/history/learner) see the
				// commanded discharge.
				state.LastTargets = out
				now := time.Now()
				state.LastDispatch = &now
			}
			return out
		}
	}

	// ---- Read site meter ----
	rawGridW := 0.0
	if r := store.Get(state.SiteMeterDriver, telemetry.DerMeter); r != nil {
		rawGridW = r.SmoothedW
	}
	// Live EV charger readings override the manual slider on each tick —
	// hardware truth beats guesses. Only override when something >0 is
	// actually being reported, so an offline / stale EV driver doesn't
	// silently zero out a user-set manual value.
	evSum := store.SumOnlineEVW()
	if evSum > 0 {
		state.EVChargingW = evSum
	}
	// EV signal: subtract EV load from grid so batteries don't try to cover
	// it. EV is always a positive import at the meter; subtracting it makes
	// the "effective grid" the controller works on the house-side portion
	// only — a sensible default that avoids shuffling energy through the
	// inverter twice on a normal day.
	//
	// BatteryCoversEV (default false) flips this: the operator opts in to
	// have the battery discharge into the EV. Useful when grid prices are
	// high right now but expected to drop later (e.g. solar coming up), so
	// it's cheaper to drain the battery now and refill it off-peak. All
	// clamps (SoC, per-driver MaxDischargeW, fuse guard) still apply —
	// exceeding battery capacity just means the residual comes from grid.
	gridW := rawGridW
	// Surplus-only EV transient cover: when an LP is in surplus_only mode
	// AND the EV is currently drawing AND the site is importing, the EV
	// is drawing more than the available surplus. This typically happens
	// during the 5–15 s window after surplus_only is freshly enabled
	// (the EV ramps current down through Easee Cloud + the car's onboard
	// charger — both are slow), or during a sudden cloud transient. The
	// home battery (Pixii) responds in <1 s, so let it cover the import
	// burst until the EV ramp-down completes. Mechanism: don't subtract
	// EV from gridW, so the PI sees the real import and discharges
	// battery accordingly. Self-deactivates the moment grid goes
	// negative again — at that point EV draw matches surplus and there's
	// no import to cover, so steady-state surplus_only behavior is
	// unchanged. The pre-existing reserve cap on the CHARGE side still
	// stops the battery from competing with the EV for surplus when PV
	// exceeds load+EV.
	coverEV := state.BatteryCoversEV
	if !coverEV && state.EVSurplusOnlyReserveW > 0 && state.EVChargingW > 0 && rawGridW > 0 {
		coverEV = true
	}
	if !coverEV {
		gridW -= state.EVChargingW
	}

// ---- Gather online batteries ----
	batteries := make([]batteryInfo, 0, len(driverCapacities))
	for name, cap := range driverCapacities {
		r := store.Get(name, telemetry.DerBattery)
		h := store.DriverHealth(name)
		if r == nil || h == nil {
			continue
		}
		// Default to near-empty SoC so dispatch errs on the side of
		// caution (no discharge) if a battery never reports SoC.
		// Using 0.5 would allow discharge of a potentially empty battery.
		soc := 0.1
		if r.SoC != nil {
			soc = *r.SoC
		}
		lim := state.DriverLimits[name]
		batteries = append(batteries, batteryInfo{
			driver:        name,
			capacityWh:    cap,
			currentW:      r.SmoothedW,
			soc:           soc,
			online:        h.IsOnline(),
			group:         state.InverterGroups[name],
			maxChargeW:    lim.MaxChargeW,
			maxDischargeW: lim.MaxDischargeW,
		})
	}
	onlineBats := make([]batteryInfo, 0, len(batteries))
	for _, b := range batteries {
		if b.online {
			onlineBats = append(onlineBats, b)
		}
	}
	if len(onlineBats) == 0 {
		state.LastTargets = nil
		return nil
	}

	// ---- Sum of battery current power (site-signed) ----
	// Used by both paths: legacy distributors take (currentTotal + correction);
	// energy path computes correction as (desired_total - currentTotal).
	var currentTotal float64
	for _, b := range onlineBats {
		currentTotal += b.currentW
	}

	// ---- Compute totalCorrection — paths diverge here ----
	var totalCorrection float64
	switch {
	case manualHoldActive:
		// Drive the aggregate battery toward the operator's setpoint.
		// PI was already reset above; deadband is intentionally skipped
		// so even small setpoints are honoured exactly. Slew, SoC clamps,
		// and the fuse guard still apply downstream.
		totalCorrection = manualHold.PowerW - currentTotal
	case plannerSelfIdleGate:
		// planner_self + plan says idle this slot: drive the battery
		// total toward 0 regardless of live grid flow. Slew ramps it
		// down over several cycles; the PI stays out of it so no
		// integral wind-up carries into the next slot.
		state.PI.Reset()
		totalCorrection = -currentTotal
		// deliberately skip the deadband — it's a gridW check and
		// doesn't see "battery wants to be at zero but isn't yet".
	case useEnergyPath:
		// Energy-allocation path: plan's slot directive says "this many Wh
		// over this slot". Derive the instantaneous power needed to hit the
		// remaining energy in the remaining time, then pass (target - currentTotal)
		// as the correction the existing distributors expect.
		now := time.Now()
		// Slot rollover: new slot → reset the delivered accumulator.
		if !currentDirective.SlotStart.Equal(state.currentDirective.SlotStart) {
			state.currentDirective = currentDirective
			state.slotDelivered = 0
			state.lastTickTs = now
		} else {
			// Accumulate energy delivered since the last tick, using live
			// battery telemetry (the truth about what the fleet is doing
			// right now). This lets the formula course-correct when the
			// commanded setpoint couldn't be met.
			dt := now.Sub(state.lastTickTs).Seconds()
			if dt > 0 && dt < 300 { // cap dt at 5min so a long pause doesn't poison accumulator
				state.slotDelivered += currentTotal * dt / 3600.0
			}
			state.lastTickTs = now
			// A reactive replan can shrink the slot's energy budget while
			// the accumulator already overshot the new (smaller) target —
			// e.g. live PV/load drift triggers replan that decides the
			// remaining slot should stop discharging, but slotDelivered
			// already exceeds the new BatteryEnergyWh. Without capping,
			// remainingWh flips sign and the dispatch tries to "buy back"
			// the discharge — exactly the trade the planner avoided.
			// Cap so remainingWh = 0 (idle for the rest of the slot)
			// instead of going positive (charge during a discharge slot).
			state.currentDirective.BatteryEnergyWh = currentDirective.BatteryEnergyWh
			state.currentDirective.SlotEnd = currentDirective.SlotEnd
			if currentDirective.BatteryEnergyWh < 0 && state.slotDelivered < currentDirective.BatteryEnergyWh {
				state.slotDelivered = currentDirective.BatteryEnergyWh
			}
			if currentDirective.BatteryEnergyWh > 0 && state.slotDelivered > currentDirective.BatteryEnergyWh {
				state.slotDelivered = currentDirective.BatteryEnergyWh
			}
		}
		remainingWh := currentDirective.BatteryEnergyWh - state.slotDelivered
		remainingS := currentDirective.SlotEnd.Sub(now).Seconds()
		var targetTotalW float64
		if remainingS > 0.5 {
			targetTotalW = remainingWh * 3600.0 / remainingS
		}
		// Grid target is a pure observation on this path — useful for UI
		// + legacy API, not driving PI. Use SetGridTarget so both
		// GridTargetW *and* PI.Setpoint move to 0 in lockstep: if the
		// operator later switches out of a planner mode, the legacy
		// path's PI.Update would otherwise compute error against a
		// stale setpoint while deadband/error checks use the synced
		// GridTargetW, producing wrong corrections.
		state.SetGridTarget(0)
		state.PI.Reset()

		// BatteryCoversEV=false safety net: the energy path executes the
		// plan's BatteryEnergyWh directive blindly, but the MPC may have
		// planned battery→EV transfers (it joint-optimises both). Cap any
		// commanded discharge to the level the reactive path would target
		// — i.e. enough to zero the *house* side, not to feed the EV.
		// Charging is left untouched. Mirrors the dispatch.go:453 rule on
		// the legacy path.
		//
		// Use a real EV-active threshold (evActiveThresholdW) instead of
		// `> 0`: connected-but-idle chargers report low-W noise (~1 W
		// from Easee's last-known reading bleed) that would otherwise
		// trip this safety net on every evening-peak slot — pinning a
		// planned -9 kW discharge to just-cover-the-house. The threshold
		// preserves the EV-protection guarantee for any real draw while
		// letting noise pass through. Regression:
		// TestEnergyDispatchIgnoresEVChargingWNoiseUnderThreshold,
		// TestEnergyDispatchClampsDischargeWhenEVActuallyCharging.
		//
		// EXCEPTION: surplus-only transient cover. Same gate as the
		// rawGridW-subtraction path above — when a surplus_only LP is
		// drawing more than current surplus (site importing), the battery
		// is allowed to bridge the EV ramp-down for ~10 s while the
		// Easee/car-side current ramp completes. The cap reverts the
		// moment grid goes negative.
		evActive := state.EVChargingW > evActiveThresholdW
		surplusTransient := state.EVSurplusOnlyReserveW > 0 && evActive && rawGridW > 0
		// CANONICAL "battery may not feed EV" accounting. The MPC's
		// NoBatteryToEV DP feasibility rule (mpc.go, see the
		// houseResidualW check inside the action loop) mirrors this
		// computation so the planner stops emitting allocations that
		// this clamp then has to censor. TODO(refactor): extract the
		// houseResidualW math + the (battW<0, evW>0) feasibility
		// predicate into a small helper consumed by both this clamp
		// and the DP rule, so a future change to the accounting can't
		// drift between plan and runtime.
		if !state.BatteryCoversEV && !surplusTransient && evActive && targetTotalW < 0 {
			houseGridW := rawGridW - state.EVChargingW
			reactiveTotal := currentTotal - houseGridW
			if targetTotalW < reactiveTotal {
				targetTotalW = reactiveTotal
			}
		}

		// PV surplus absorber underlay (opt-in). Catches the gap between
		// the MPC's 15-min slot allocation and live PV/load drift: when
		// the plan's target would still leave grid exporting beyond the
		// threshold AND average SoC is below cap AND we're not already
		// in a planned discharge, redirect the leftover export into the
		// battery instead of crossing the meter at low spot price.
		//
		// Only adds charge — never reverses a discharge plan. The slot
		// Wh accumulator (state.slotDelivered) sees the extra and the
		// next replan reads true SoC, so the plan adapts naturally.
		//
		// Order: runs BEFORE the surplus-only EV reserve cap below, so
		// any addition the absorber makes is then re-capped if an EV is
		// reserving PV headroom for itself.
		if state.PVSurplusAbsorbSoCCapPct > 0 && targetTotalW >= 0 {
			threshold := state.PVSurplusAbsorbThresholdW
			if threshold <= 0 {
				threshold = 100
			}
			var sumSoCWh, totalCap float64
			for _, b := range onlineBats {
				sumSoCWh += b.soc * b.capacityWh
				totalCap += b.capacityWh
			}
			var avgSoCPct float64
			if totalCap > 0 {
				avgSoCPct = (sumSoCWh / totalCap) * 100
			}
			if avgSoCPct < state.PVSurplusAbsorbSoCCapPct {
				// Grid level if dispatch ran the plan as-is.
				projectedGridW := rawGridW + (targetTotalW - currentTotal)
				extraExportW := -projectedGridW
				if extraExportW > threshold {
					// Remaining headroom in Wh, converted to a power
					// share over the rest of the slot. Floor remainingS
					// at 60 s so the late-slot edge doesn't ask for
					// implausibly high power.
					socHeadroomWh := (state.PVSurplusAbsorbSoCCapPct - avgSoCPct) / 100 * totalCap
					remainS := remainingS
					if remainS < 60 {
						remainS = 60
					}
					headroomW := socHeadroomWh * 3600 / remainS
					addW := extraExportW
					if addW > headroomW {
						addW = headroomW
					}
					targetTotalW += addW
				}
			}
		}

		// Surplus-only EV reserve (energy path): cap battery aggregate
		// charge to leave PV headroom for an EV that's under a
		// surplus_only loadpoint. The MPC's grid-charge ban handles the
		// planning side; this enforces it on every tick (covers reactive
		// drift, stale plan fallback, and the period before the next
		// replan picks up the EV's needs). Discharge is unaffected — the
		// reserve only matters when the battery is competing with the
		// EV for the same surplus PV. Final cap — runs AFTER the PV
		// surplus absorber so the absorber can't override an EV reserve.
		if state.EVSurplusOnlyReserveW > 0 && targetTotalW > 0 {
			pvSurplus := -gridW + currentTotal
			reserveRemaining := state.EVSurplusOnlyReserveW - state.EVChargingW
			if reserveRemaining < 0 {
				reserveRemaining = 0
			}
			ceiling := pvSurplus - reserveRemaining
			if ceiling < 0 {
				ceiling = 0
			}
			if targetTotalW > ceiling {
				targetTotalW = ceiling
			}
		}

		totalCorrection = targetTotalW - currentTotal
	default:
		// Legacy PI-on-grid-target path. Used by:
		//   - manual modes (self_consumption, peak_shaving, priority, weighted)
		//   - planner_self (the "participate reactively" branch — idle-gate
		//     already handled above)
		//   - planner_cheap / planner_arbitrage when UseEnergyDispatch=false

		// Surplus-only EV reserve (legacy/reactive path): when an EV
		// under a surplus_only loadpoint is connected, bias the grid
		// signal so the PI leaves `reserveRemaining` of export untouched
		// for the EV. Without this, the PI drives gridW→0 by absorbing
		// every kW of surplus into the battery and the EV controller
		// (which polls a separate surplus number) sees nothing left to
		// claim — flap mode. The bias is only applied to the self-
		// consumption flavour: PeakShaving has its own peak-relative
		// error, and other modes (Charge, Priority, Weighted) are out
		// of scope for surplus-only semantics.
		//
		// Three regions:
		//   gridW < -reserveRemaining: exporting MORE than the reserve.
		//     Bias so PI absorbs only the excess (export beyond reserve).
		//     biasedGridW = gridW + reserveRemaining (still negative).
		//   -reserveRemaining <= gridW <= 0: exporting within the reserve.
		//     Idle the battery — let the EV claim the export. biasedGridW=0.
		//   gridW > 0: importing. Unchanged — battery still covers
		//     household imports normally (the EV isn't drawing on
		//     surplus_only, so there's no double-counting concern).
		// A naive bias of `gridW + reserve` over the whole range would
		// flip sign in the within-reserve band and tell the PI to
		// DISCHARGE battery into the EV's reserved export space — the
		// opposite of what we want.
		biasedGridW := gridW
		if state.EVSurplusOnlyReserveW > 0 && effectiveMode == ModeSelfConsumption {
			reserveRemaining := state.EVSurplusOnlyReserveW - state.EVChargingW
			if reserveRemaining < 0 {
				reserveRemaining = 0
			}
			if gridW < -reserveRemaining {
				biasedGridW = gridW + reserveRemaining
			} else if gridW < 0 {
				biasedGridW = 0
			}
			// gridW >= 0: leave biasedGridW = gridW (import behavior).
		}

		var errW float64
		switch effectiveMode {
		case ModePeakShaving:
			// Only act when grid import exceeds peak_limit. Allow any amount of
			// export, allow import up to peak_limit.
			if gridW > state.PeakLimitW {
				errW = gridW - state.PeakLimitW
			} else if gridW < 0 {
				errW = gridW // exporting → charge with surplus
			} else {
				errW = 0
			}
		default:
			errW = biasedGridW - state.GridTargetW
		}

		// Deadband only applies to the legacy path — the energy formula
		// produces small corrections naturally when close to target.
		// Surplus-only EV reserve override: when reserve is active, do
		// NOT short-circuit on small error. The bias above can collapse
		// the error to zero in the within-reserve band, but the battery
		// might be running on a stale higher setpoint from before the
		// reserve kicked in. Returning nil here would leave the battery
		// stuck at that previous level, defeating the whole point of
		// the reserve. Falling through forces a fresh dispatch that
		// drives the battery toward 0 (or the post-PI cap below).
		surplusActive := state.EVSurplusOnlyReserveW > 0 && effectiveMode == ModeSelfConsumption
		if !surplusActive && math.Abs(errW) < state.GridToleranceW {
			return nil
		}

		// Outer PI — drives total correction we want across all batteries.
		// Site convention: gridW positive = too much import → we want to discharge
		// batteries (site-signed correction should be negative).
		// PI setpoint = GridTargetW, measurement = gridW.
		// For PeakShaving we feed a slightly different measurement so the same PI works.
		var piMeasurement float64
		if effectiveMode == ModePeakShaving {
			piMeasurement = state.GridTargetW + errW
		} else {
			piMeasurement = biasedGridW
		}
		out := state.PI.Update(piMeasurement)
		totalCorrection = out.Output

		// Live-meter clamp on the legacy PI path: plan decides charge or
		// discharge direction; the live error decides magnitude. Prevents
		// the load-twin over-prediction case where reactive PI commands a
		// discharge larger than what's needed to close errW. Scoped to
		// this default arm only — manualHold, useEnergyPath, and
		// plannerSelfIdleGate each have their own contracts that
		// intentionally cross the GridTargetW line.
		//
		// "Don't overshoot": with load and PV held constant within a tick,
		// gridW moves 1:1 with bat (conservation: grid = load + bat + pv).
		// So newBat = currentTotal - errW lands gridW exactly on
		// GridTargetW. The clamp caps the PI's request at that ideal
		// landing point — anything beyond would push gridW past the
		// target (the original PR #270 incident: wound-up PI overshoots
		// into export).
		//
		// The earlier formula `allowed = -errW` used errW as if it were
		// an absolute discharge magnitude rather than a delta. That
		// (a) ignored currentTotal entirely, pinning bat at exactly the
		// level that produces the current import — a self-consistent
		// stuck state whenever load exceeds |errW|, the steady-state
		// case in any house with continuous load above the gap; and
		// (b) forced bat to 0 when targetTotal and errW disagreed in
		// sign, which made the dispatcher hard-cut discharge during
		// natural overshoot/correction cycles (PI's correction sign
		// always points toward closing errW, so during a recovery from
		// overshoot targetTotal and errW *should* disagree — pulling
		// bat to 0 mid-recovery introduces flapping).
		//
		// Only constraint here: don't push targetTotal past idealTarget
		// in the dispatch direction. If PI is well-tuned and not wound
		// up, targetTotal stays on the right side and passes through.
		//
		// Deadband (state.GridToleranceW) already gated entry to this
		// arm at the abs(errW) < dead check above; no second deadband
		// haircut here.
		targetTotal := currentTotal + totalCorrection
		idealTarget := currentTotal - errW
		var allowed float64
		switch {
		case errW > 0 && targetTotal < idealTarget:
			// Importing: discharge needed. PI wants more than will land
			// us on target — cap so we don't punch through into export.
			allowed = idealTarget
		case errW < 0 && targetTotal > idealTarget:
			// Exporting: charge needed. PI wants more than will land us
			// on target — cap so we don't punch through into import.
			allowed = idealTarget
		default:
			allowed = targetTotal
		}
		dead := state.GridToleranceW
		if allowed != targetTotal {
			slog.Warn("dispatch: meter clamp reduced battery target",
				"requested_total_w", targetTotal,
				"clamped_total_w", allowed,
				"grid_w", gridW,
				"grid_target_w", state.GridTargetW,
				"err_w", errW,
				"current_total_w", currentTotal,
				"deadband_w", dead,
				"mode", string(effectiveMode))
		}
		totalCorrection = allowed - currentTotal

		// Surplus-only EV reserve cap: stack on top of the meter clamp.
		// Mirror the energy-path cap so the battery target on the
		// legacy/reactive path also respects the EV reserve. The PI bias
		// above tries to steer the integrator the right way, but PI
		// windup, deadband interaction, and the slow integrator unwind
		// would otherwise leave the battery on a stale charge command
		// for tens of seconds — long enough to starve the EV. The meter
		// clamp above caps "don't overshoot GridTargetW"; this cap is
		// tighter when EV reserve is active: "don't charge past
		// pvSurplus minus reserveRemaining" so the EV's promised PV
		// slice stays unspent.
		if surplusActive {
			targetTotal2 := currentTotal + totalCorrection
			if targetTotal2 > 0 {
				pvSurplus := -gridW + currentTotal
				reserveRemaining := state.EVSurplusOnlyReserveW - state.EVChargingW
				if reserveRemaining < 0 {
					reserveRemaining = 0
				}
				ceiling := pvSurplus - reserveRemaining
				if ceiling < 0 {
					ceiling = 0
				}
				if targetTotal2 > ceiling {
					totalCorrection = ceiling - currentTotal
				}
			}
		}
	}

	// ---- Joint fuse-budget allocator ----
	// When EV draw + commanded battery charge would exceed the site fuse,
	// scale BOTH proportionally so they share the budget rather than
	// oscillating against each other (battery ramps up per plan → fuse
	// guard cuts it → plan ramps again next tick — operator report
	// 2026-04-27). The battery side is mutated here; the EV side is
	// published via state.FuseEVMaxW + FuseSaturated for the loadpoint
	// controller to read on its next Tick.
	//
	// Math (site sign, + = import):
	//   targetTotal = currentTotal + totalCorrection
	//   B  = max(0, targetTotal)        battery charge component (≥0)
	//   Bn = min(0, targetTotal)        battery discharge component (≤0)
	//   E  = state.EVChargingW          EV draw (≥0)
	//   H  = rawGridW − currentTotal − E   "house" net (load + PV)
	//   newGrid' = H + (Bn + B*scale) + E*scale
	//   solve newGrid' ≤ fuseMaxW  ⇒  scale ≤ (fuseMaxW − H − Bn) / (B + E)
	//
	// Discharge alone never trips this — Bn is negative, so it lifts the
	// numerator (more headroom). Only positive battery demand competes
	// with EV.
	//
	// BatteryCoversEV mode: H is computed from rawGridW (the raw meter,
	// unchanged regardless of BatteryCoversEV) so H stays correct in
	// both modes. The PI's gridW is the only place that branches on
	// BatteryCoversEV; the joint allocator's geometry is independent
	// of that. Regression: TestJointFuseAllocatorWithBatteryCoversEV.
	state.FuseEVMaxW = 0
	state.FuseSaturated = false
	if fuseMaxW > 0 && state.EVChargingW > 0 {
		// Use the effective import ceiling (fuse minus safety margin,
		// or tariff peak when tighter) so PeakImportCeilingW throttles
		// the EV through the same surface as the fuse. Steady-state
		// with peak active and BatteryCoversEV=false: the EV settles
		// at this scaled rate; the battery's transient discharge to
		// bridge the ramp-down is handled by forceFuseDischarge below.
		ceilingW := state.effectiveImportCeilingW(fuseMaxW)
		targetTotal := currentTotal + totalCorrection
		B := math.Max(0, targetTotal)
		Bn := math.Min(0, targetTotal)
		E := state.EVChargingW
		H := rawGridW - currentTotal - E
		projectedGrid := H + targetTotal + E
		if projectedGrid > ceilingW && (B+E) > 0 {
			scale := (ceilingW - H - Bn) / (B + E)
			if scale < 0 {
				scale = 0
			}
			if scale > 1 {
				scale = 1
			}
			newBattery := Bn + B*scale
			totalCorrection = newBattery - currentTotal
			state.FuseEVMaxW = E * scale
			state.FuseSaturated = true
		}
	}

	// ---- Per-group PV surplus for DC-local charge routing (#143) ----
	// Empty when no drivers carry an inverter-group tag → distributeProportional
	// falls through to its capacity-only split (today's behavior).
	groupPV := map[string]float64{}
	if len(state.InverterGroups) > 0 {
		for _, r := range store.ReadingsByType(telemetry.DerPV) {
			group := state.InverterGroups[r.Driver]
			if group == "" {
				continue // untagged PV: no locality signal, treat as AC-bus
			}
			// PV is site-signed (negative = generating). Magnitude = surplus
			// potentially routable DC-direct to the same-group battery.
			groupPV[group] += math.Abs(r.SmoothedW)
		}
	}

	// ---- Distribute across batteries ----
	var raw []DispatchTarget
	switch effectiveMode {
	case ModeSelfConsumption, ModePeakShaving:
		raw = distributeProportional(onlineBats, totalCorrection, groupPV)
	case ModePriority:
		raw = distributePriority(onlineBats, totalCorrection, state.PriorityOrder)
	case ModeWeighted:
		raw = distributeWeighted(onlineBats, totalCorrection, state.Weights)
	}

	// ---- Slew rate limit per driver ----
	//
	// Slew FROM the battery's actual measured output (SmoothedW), not
	// from the previous command. When the battery can't meet a command
	// (e.g. SoC at min and commanded to discharge, SoC at max and
	// commanded to charge, or driver offline), the command stays pinned
	// far from reality. Using the stored command as the slew anchor then
	// forces `|target - stale_command| / slew_rate` cycles of ramping
	// before the direction reverses — a 5 kW stale command with a 500
	// W/cycle slew at 5 s interval means 50 s of wasted export before
	// the surplus-absorb starts.
	//
	// Using actual-smoothed-W is the truth about where the battery is,
	// and lets the dispatch pivot immediately when the setpoint reverses.
	// Falls back to the previous command if no reading is available
	// (driver just started, or stale telemetry).
	for i := range raw {
		anchor, hasAnchor := state.PrevTargets[raw[i].Driver]
		if r := store.Get(raw[i].Driver, telemetry.DerBattery); r != nil {
			anchor = r.SmoothedW
			hasAnchor = true
		}
		if !hasAnchor {
			continue
		}
		delta := raw[i].TargetW - anchor
		if math.Abs(delta) > state.SlewRateW {
			sign := 1.0
			if delta < 0 {
				sign = -1.0
			}
			raw[i].TargetW = anchor + sign*state.SlewRateW
			raw[i].Clamped = true
		}
	}

	// ---- Re-clamp after slew ----
	// The slew anchor is the battery's actual output (SmoothedW). If the
	// battery was already beyond its per-command cap (e.g. after a
	// manual restart, external control, or driver returning an out-of-range
	// reading), the slewed target inherits the overshoot. Re-apply the
	// per-driver cap (DriverLimits, falling back to MaxCommandW) so we
	// never issue a command outside safe bounds.
	for i := range raw {
		maxC := float64(MaxCommandW)
		maxD := float64(MaxCommandW)
		if lim, ok := state.DriverLimits[raw[i].Driver]; ok {
			if lim.MaxChargeW > 0 {
				maxC = lim.MaxChargeW
			}
			if lim.MaxDischargeW > 0 {
				maxD = lim.MaxDischargeW
			}
		}
		if raw[i].TargetW > maxC {
			raw[i].TargetW = maxC
			raw[i].Clamped = true
		} else if raw[i].TargetW < -maxD {
			raw[i].TargetW = -maxD
			raw[i].Clamped = true
		}
	}

	// ---- Fuse guard (bidirectional, #145) ----
	raw = applyFuseGuard(raw, store, state, fuseMaxW)

	// ---- Plan/exec sign-mismatch floor (planner modes only) ----
	// Operator-report 2026-04-28 (08:00–08:15 CEST): planner_arbitrage
	// peak slot wanted battery_w = -2400 W (discharge to export at peak),
	// dispatch produced +1640..+1860 W (charged from PV surplus). PV
	// got swallowed by the battery instead of sold at 334 öre/kWh.
	//
	// Root cause was a code-path divergence elsewhere; this is the
	// rail that makes that whole class of bug a no-op:
	//
	//   plan says discharge, exec produces charge → idle this tick
	//   plan says charge,    exec produces discharge → idle this tick
	//
	// "Idle for this tick" is the right floor because:
	//   - Discharging a charge slot would burn cycles against operator
	//     intent. Idling and waiting for the next replan is harmless.
	//   - Charging a discharge slot would buy energy at the exact slot
	//     the planner intended to SELL it. Idling and letting PV export
	//     naturally captures most of the lost revenue without any risk.
	//
	// Only applied in planner modes — manual modes have no plan to
	// disagree with. forceFuseDischarge runs AFTER this so a fuse
	// overflow can still drive discharge regardless of plan intent.
	//
	// Skipped when a manual hold is active: the operator is
	// deliberately overriding the planner, so a sign mismatch with the
	// plan is the intended behaviour, not a bug to clamp out.
	if !manualHoldActive {
		raw = applyPlanSignFloor(raw, state)
	}

	// forceFuseDischarge runs LAST, deliberately AFTER the slew loop
	// at line 625. A fuse overflow can demand a battery target that's
	// far beyond what slew would normally allow in a single 5 s tick
	// (e.g. 0 W → −3 kW), and slew-limiting that would leave the
	// fuse violated for multiple ticks. The fuse is the
	// non-negotiable ceiling — it bypasses slew. Regression-guarded
	// by TestFuseSaverBypassesSlew.
	raw = forceFuseDischarge(raw, store, state, driverCapacities, fuseMaxW)

	// ---- Republish FuseEVMaxW after forceFuseDischarge ----
	// The joint allocator (line 625) computes FuseEVMaxW assuming the
	// battery target it just produced is what gets dispatched. But
	// forceFuseDischarge may have flipped that target from charge to
	// discharge — freeing additional fuse headroom for the EV.
	// Without this re-publish the loadpoint controller throttles the
	// EV against a stale (too-conservative) cap for one tick. Run only
	// when the joint allocator already engaged this tick — otherwise
	// FuseEVMaxW is "no advice" and should stay 0.
	// Skip the republish when the operator's tariff peak is the binding
	// ceiling (peak < fuse). The republish's purpose is to free up EV
	// headroom freed by forceFuseDischarge's transient battery bridge —
	// that's the right thing for FUSE protection (hardware safety,
	// battery is the last line of defense and there's no policy
	// preference about how long to drain it). For TARIFF protection,
	// the EV cap must reflect the steady state where the battery
	// isn't covering, otherwise the bridge becomes a permanent
	// shuttle and the operator pays the peak charge anyway. Let the
	// joint allocator's pre-forceFuseDischarge FuseEVMaxW stand.
	peakBindingW := fuseMaxW - state.fuseSafetyMarginW()
	peakBinding := state != nil && state.PeakImportCeilingW > 0 && state.PeakImportCeilingW < peakBindingW
	if !peakBinding && state.FuseSaturated && state.EVChargingW > 0 && fuseMaxW > 0 {
		var postBat float64
		seen := make(map[string]struct{}, len(raw))
		for _, t := range raw {
			if _, ok := seen[t.Driver]; ok {
				continue
			}
			seen[t.Driver] = struct{}{}
			postBat += t.TargetW
		}
		// H = rawGridW − currentTotal − E (unchanged from joint allocator)
		// newGrid = H + postBat + E*scale ≤ ceiling
		// → scale ≤ (ceiling − H − postBat) / E, capped to ≤1
		// Use the effective import ceiling so peak-driven caps survive
		// the post-forceFuseDischarge republish (otherwise the joint
		// allocator's tariff-tightened cap gets overwritten with the
		// fuse-only headroom).
		ceilingW := state.effectiveImportCeilingW(fuseMaxW)
		var rawGridW2 float64
		if r := store.Get(state.SiteMeterDriver, telemetry.DerMeter); r != nil {
			rawGridW2 = r.SmoothedW
		}
		H := rawGridW2 - currentTotal - state.EVChargingW
		headroom := ceilingW - H - postBat
		if headroom < 0 {
			headroom = 0
		}
		newCap := headroom
		if newCap > state.EVChargingW {
			newCap = state.EVChargingW
		}
		state.FuseEVMaxW = newCap
	}

	// Update state
	now := time.Now()
	state.LastDispatch = &now
	for _, t := range raw {
		state.PrevTargets[t.Driver] = t.TargetW
	}
	state.LastTargets = raw
	return raw
}

// ComputePVCurtail returns one CurtailTarget per affected driver for
// this dispatch tick.
//
// When the active plan slot's PVLimitW > 0 (the MPC's annotateCurtailment
// flagged that exporting more PV than the limit would lose money — e.g.
// negative spot, no positive feed-in tariff), the limit is allocated
// proportionally across drivers in `state.SupportsPVCurtail` according
// to each driver's live PV output. Drivers not in that set are silently
// skipped — an EV charger or a meter driver wouldn't know what to do
// with a `curtail` payload.
//
// Drivers that received curtail last tick but aren't in the new set
// (slot rolled over, or PVLimitW dropped to 0) get LimitW=0, which
// main.go translates to `curtail_disable`. That guarantees the cap
// is released exactly once when the plan no longer wants it — no
// silent capping after a mode change or a fresh plan.
//
// Returns nil when nothing is curtailed and nothing needs releasing.
//
// Idempotent: state mutation (LastCurtailedDrivers) reflects the
// post-call set of actively-curtailed drivers.
func ComputePVCurtail(state *State, store *telemetry.Store) []CurtailTarget {
	if state == nil {
		return nil
	}
	// Fetch the active slot directly — independent of dispatch mode,
	// because curtailment is an economic decision that applies in
	// any mode the operator picked. The plan's `annotateCurtailment`
	// already gated PVLimitW on negative export revenue.
	var limit float64
	if state.SlotDirective != nil {
		if dir, ok := state.SlotDirective(time.Now()); ok {
			limit = dir.PVLimitW
		}
	}

	// Decide which drivers should be curtailed this tick.
	next := map[string]float64{}
	if limit > 0 && store != nil && len(state.SupportsPVCurtail) > 0 {
		// Allocate the site-wide limit proportionally to each PV-
		// supporting driver's live |PV|. A driver currently producing
		// nothing gets nothing (no point asking it to cap output it
		// isn't producing).
		type pvD struct {
			name string
			abs  float64
		}
		var drivers []pvD
		var total float64
		for _, r := range store.ReadingsByType(telemetry.DerPV) {
			if !state.SupportsPVCurtail[r.Driver] {
				continue
			}
			if r.RawW >= 0 {
				continue // not generating right now
			}
			abs := -r.RawW
			drivers = append(drivers, pvD{name: r.Driver, abs: abs})
			total += abs
		}
		if total > 0 {
			for _, d := range drivers {
				next[d.name] = limit * (d.abs / total)
			}
		}
	}

	var out []CurtailTarget
	// Release any driver we curtailed last tick but not this one.
	for d := range state.LastCurtailedDrivers {
		if _, ok := next[d]; !ok {
			out = append(out, CurtailTarget{Driver: d, LimitW: 0})
		}
	}
	for d, w := range next {
		out = append(out, CurtailTarget{Driver: d, LimitW: w})
	}

	if len(next) == 0 {
		state.LastCurtailedDrivers = nil
	} else {
		updated := make(map[string]bool, len(next))
		for d := range next {
			updated[d] = true
		}
		state.LastCurtailedDrivers = updated
	}
	return out
}

// distributeProportional splits the total desired battery power across the
// available batteries by capacity. Each battery gets its share of the TOTAL
// desired site battery power — not its share of the delta. This prevents the
// "drift" bug where each battery drifts independently under prolonged error.
//
// When `groupPV` is non-empty AND the fleet is charging (desiredTotal > 0),
// the algorithm first routes up to `min(desiredTotal, ΣPV_g)` preferentially
// to batteries whose inverter-group also reports live PV output — keeping
// the flow DC-coupled on the same inverter avoids the ~3-4 pp round-trip
// loss of cross-inverter AC routing. Any remaining correction is then
// spread proportionally across all batteries (capacity-weighted), identical
// to today. With no `groupPV` info or during discharge, the algorithm
// collapses to a single capacity-proportional split. Issue #143.
func distributeProportional(bats []batteryInfo, totalCorrection float64, groupPV map[string]float64) []DispatchTarget {
	var totalCap float64
	for _, b := range bats {
		totalCap += b.capacityWh
	}
	if totalCap <= 0 {
		return nil
	}
	var currentTotal float64
	for _, b := range bats {
		currentTotal += b.currentW
	}
	desiredTotal := currentTotal + totalCorrection

	// Discharge, idle, or no PV locality info → capacity-only split.
	// Discharge energy flows to the AC bus regardless of where it
	// originated, so DC-locality has no win for the negative branch.
	var totalPV float64
	for _, w := range groupPV {
		totalPV += w
	}
	if desiredTotal <= 0 || totalPV <= 0 {
		return distributeByCapacity(bats, desiredTotal, totalCap)
	}

	// Charging with PV locality info: prefer DC-local routing.
	//   localCap  = min(desiredTotal, totalPV)  — how much of the total
	//               fleet charge can be kept DC-coupled
	//   overflow  = desiredTotal - localCap     — excess that has to cross
	//               inverters via the AC bus; no locality benefit, so
	//               it's allocated by capacity like today.
	//
	// Within the local pool, each group gets a share of localCap
	// proportional to its PV output; within a group, that share is split
	// by capacity (same rule as the fleet-wide split).
	localCap := math.Min(desiredTotal, totalPV)
	overflow := desiredTotal - localCap

	capByGroup := map[string]float64{}
	for _, b := range bats {
		capByGroup[b.group] += b.capacityWh
	}

	out := make([]DispatchTarget, 0, len(bats))
	for _, b := range bats {
		var localShare float64
		if capG := capByGroup[b.group]; capG > 0 && groupPV[b.group] > 0 {
			localShare = (groupPV[b.group] / totalPV) * localCap * (b.capacityWh / capG)
		}
		overflowShare := overflow * (b.capacityWh / totalCap)
		target := localShare + overflowShare
		clamped, was := clampWithSoC(target, b)
		out = append(out, DispatchTarget{Driver: b.driver, TargetW: clamped, Clamped: was})
	}
	return out
}

// distributeByCapacity is the legacy capacity-proportional split, extracted
// so both the discharge path and the no-groupPV fallback share the same code.
func distributeByCapacity(bats []batteryInfo, desiredTotal, totalCap float64) []DispatchTarget {
	out := make([]DispatchTarget, 0, len(bats))
	for _, b := range bats {
		target := desiredTotal * (b.capacityWh / totalCap)
		clamped, was := clampWithSoC(target, b)
		out = append(out, DispatchTarget{Driver: b.driver, TargetW: clamped, Clamped: was})
	}
	return out
}

// distributePriority assigns correction to the primary battery first, falling
// back to secondaries only when saturated.
func distributePriority(bats []batteryInfo, totalCorrection float64, order []string) []DispatchTarget {
	remaining := totalCorrection
	out := make([]DispatchTarget, 0, len(bats))
	// Named order first
	for _, name := range order {
		for _, b := range bats {
			if b.driver != name {
				continue
			}
			t := b.currentW + remaining
			clamped, was := clampWithSoC(t, b)
			remaining -= clamped - b.currentW
			out = append(out, DispatchTarget{Driver: b.driver, TargetW: clamped, Clamped: was})
		}
	}
	// Unmentioned batteries stay at their current power
	for _, b := range bats {
		seen := false
		for _, o := range out {
			if o.Driver == b.driver {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, DispatchTarget{Driver: b.driver, TargetW: b.currentW})
		}
	}
	return out
}

// distributeWeighted splits by custom weights. Missing batteries default to weight=1.
func distributeWeighted(bats []batteryInfo, totalCorrection float64, weights map[string]float64) []DispatchTarget {
	var totalW float64
	for _, b := range bats {
		w, ok := weights[b.driver]
		if !ok { w = 1.0 }
		totalW += w
	}
	if totalW <= 0 { return nil }
	var currentTotal float64
	for _, b := range bats { currentTotal += b.currentW }
	desiredTotal := currentTotal + totalCorrection

	out := make([]DispatchTarget, 0, len(bats))
	for _, b := range bats {
		w, ok := weights[b.driver]
		if !ok { w = 1.0 }
		t := desiredTotal * (w / totalW)
		clamped, was := clampWithSoC(t, b)
		out = append(out, DispatchTarget{Driver: b.driver, TargetW: clamped, Clamped: was})
	}
	return out
}

// chargeAll forces every online battery to its per-driver MaxChargeW
// (or MaxCommandW default when the driver doesn't override). Used by
// the "Charge" manual mode as a sanity-check / pre-peak-fill knob.
// Issue #145 — previously hardcoded at +5 kW regardless of hardware.
func chargeAll(store *telemetry.Store, capacities map[string]float64, limits map[string]PowerLimits) []DispatchTarget {
	out := make([]DispatchTarget, 0)
	for name := range capacities {
		h := store.DriverHealth(name)
		if h == nil || !h.IsOnline() {
			continue
		}
		target := float64(MaxCommandW)
		if lim, ok := limits[name]; ok && lim.MaxChargeW > 0 {
			target = lim.MaxChargeW
		}
		// Site convention: + = charge.
		out = append(out, DispatchTarget{Driver: name, TargetW: target})
	}
	return out
}

// clampWithSoC applies the hard safety clamps for one battery command:
//   - don't discharge below SoC 5 % (site: don't make target < 0 when SoC < 0.05);
//     BMS handles fine-grained SoC but we never ask it to pull an empty pack.
//   - cap charge at the battery's MaxChargeW (falls back to MaxCommandW default).
//   - cap discharge at the battery's MaxDischargeW (same fallback).
//
// The caps are asymmetric on purpose — real hybrid inverters often have
// different charge and discharge capability (e.g. Ferroamp 15 kW charge /
// 10 kW discharge). Issue #145.
func clampWithSoC(target float64, b batteryInfo) (float64, bool) {
	clamped := target
	wasClamped := false
	// Block discharge when the battery is empty.
	if b.soc < 0.05 && target < 0 {
		clamped = 0
		wasClamped = true
	}
	if clamped > b.chargeCap() {
		clamped = b.chargeCap()
		wasClamped = true
	} else if clamped < -b.dischargeCap() {
		clamped = -b.dischargeCap()
		wasClamped = true
	}
	return clamped, wasClamped
}

// applyFuseGuard enforces the site fuse budget on both directions of
// grid flow — import AND export. Any dispatched target would shift the
// grid flow by (target − current_battery_power); the guard predicts
// the post-dispatch grid reading and, if it would exceed ±fuseMaxW,
// scales the same-direction targets toward zero until the boundary is
// respected.
//
// Prediction (site sign: grid = load + pv + battery):
//
//	predicted_grid = live_grid − Σ current_battery_w + Σ target
//
// Because load and pv are invariant in the 5 s dispatch window, only
// the battery row changes when we apply new targets.
//
// Directional scaling:
//   - predicted > +fuseMaxW (too much import): scale POSITIVE (charge)
//     targets down. 1 W less charge = 1 W less import, so the reduction
//     directly offsets the overage.
//   - predicted < −fuseMaxW (too much export): scale NEGATIVE (discharge)
//     targets toward zero — the symmetric case.
//
// Issue #145 changed the guard from "PV + discharge > fuse → scale
// discharge" (old, discharge-only, assumed zero load) to this
// bidirectional predicted-grid approach so heavy PV-free charge slots
// can't push aggregate imports past the fuse. The new path also uses
// live load inference so the discharge side no longer over-scales
// during high-load hours.
func applyFuseGuard(targets []DispatchTarget, store *telemetry.Store, state *State, fuseMaxW float64) []DispatchTarget {
	if fuseMaxW <= 0 || state == nil {
		return targets
	}
	siteMeter := state.SiteMeterDriver
	// Aggregate live battery power so we can hold load+pv constant.
	var currentBat float64
	for _, r := range store.ReadingsByType(telemetry.DerBattery) {
		currentBat += r.SmoothedW
	}
	var currentGrid float64
	if siteMeter != "" {
		if r := store.Get(siteMeter, telemetry.DerMeter); r != nil {
			currentGrid = r.SmoothedW
		}
	}
	var sumTarget float64
	for _, t := range targets {
		sumTarget += t.TargetW
	}
	predicted := currentGrid - currentBat + sumTarget

	// Per-phase overage: the worst single-phase amperage above the fuse
	// trip threshold (less the safety margin), expressed as AGGREGATE
	// battery action needed to bring it back. Assumes a 3Φ-balanced
	// battery — each unit of total battery action contributes 1/3 to
	// each phase, so a worst-phase overage of N watts requires 3 × N
	// watts of total battery action to bring it under. Conservative
	// for 1Φ batteries (Pixii Home etc.): they over-correct on the
	// other phases, less import / export there, still safe. See PR
	// #208 follow-up in `docs/safety.md` §3a.
	//
	// `perPhaseOverageW` is direction-agnostic — phase amps from the
	// meter are absolute magnitudes. Attribute to whichever side the
	// AGGREGATE METER is currently flowing (currentGrid), not to the
	// post-target `predicted`. Per-phase amps and currentGrid are read
	// from the same DerMeter sample, so they're internally consistent;
	// `predicted` mixes in `sumTarget` (a hypothetical future state)
	// and can swing across 0 on a large planner request, attributing
	// a current-export overage to the import path (or vice versa) and
	// pushing the grid further into the violating direction.
	perPhase := perPhaseOverageW(store, state) * 3.0
	// Aggregate budget honours the safety margin too — keep dispatch
	// commands strictly inside the breaker envelope so the inverter's
	// own per-phase limiter doesn't fire first and cause a flap.
	//
	// Import ceiling is the tighter of (fuse − safety margin) and the
	// operator's tariff peak; export ceiling is fuse-only — peak is
	// import-only (effekttariff is billed on import).
	effFuseW := fuseMaxW - state.fuseSafetyMarginW()
	if effFuseW < 0 {
		effFuseW = 0
	}
	effImportW := state.effectiveImportCeilingW(fuseMaxW)
	importOverage := predicted - effImportW
	exportOverage := -effFuseW - predicted
	if perPhase > 0 {
		if currentGrid >= 0 {
			if perPhase > importOverage {
				importOverage = perPhase
			}
		} else {
			if perPhase > exportOverage {
				exportOverage = perPhase
			}
		}
	}

	out := make([]DispatchTarget, len(targets))
	copy(out, targets)

	// Hold-mode hysteresis: a recent clamp latched a max-magnitude per
	// direction. Re-apply it now even if the live overage is zero so
	// the planner can't ramp back through the boundary on the next
	// tick. Window is refreshed every time the clamp fires, so the
	// hold persists as long as the planner keeps trying to push past.
	now := time.Now()
	if state.FuseHoldUntil.After(now) {
		if state.FuseHoldMaxDischargeW > 0 {
			var totalDischarge float64
			for _, t := range out {
				if t.TargetW < 0 {
					totalDischarge += -t.TargetW
				}
			}
			if totalDischarge > state.FuseHoldMaxDischargeW {
				scale := state.FuseHoldMaxDischargeW / totalDischarge
				for i := range out {
					if out[i].TargetW < 0 {
						out[i].TargetW *= scale
						out[i].Clamped = true
					}
				}
			}
		}
		if state.FuseHoldMaxChargeW > 0 {
			var totalCharge float64
			for _, t := range out {
				if t.TargetW > 0 {
					totalCharge += t.TargetW
				}
			}
			if totalCharge > state.FuseHoldMaxChargeW {
				scale := state.FuseHoldMaxChargeW / totalCharge
				for i := range out {
					if out[i].TargetW > 0 {
						out[i].TargetW *= scale
						out[i].Clamped = true
					}
				}
			}
		}
	} else if !state.FuseHoldUntil.IsZero() {
		// Hold window expired — reset the latch so a future planner
		// re-ramp doesn't get permanently capped by stale state.
		state.FuseHoldMaxDischargeW = 0
		state.FuseHoldMaxChargeW = 0
		state.FuseHoldUntil = time.Time{}
	}

	if importOverage <= 0 && exportOverage <= 0 {
		return out
	}

	// Headroom buffer: shrink targets by `overage + half the configured
	// safety margin` so post-clamp the grid sits *below* the threshold
	// instead of riding right at it. Without this, the next dispatch
	// tick lets the planner re-ramp into the threshold and the system
	// oscillates at the boundary — the operator's "safety margin"
	// becomes the active steady-state instead of a buffer below it.
	//
	// Half-margin chosen so the post-clamp phase amps still cluster
	// near the threshold (operator wants the fuse used efficiently)
	// but with enough buffer to absorb load fluctuations and per-phase
	// imbalance on the next tick. Operators wanting tighter or looser
	// buffer just adjust safety_margin_a.
	buffer := state.fuseSafetyMarginW() * 0.5

	switch {
	case importOverage > 0:
		// Too much import (aggregate or per-phase) → shrink charging.
		var totalCharge float64
		for _, t := range out {
			if t.TargetW > 0 {
				totalCharge += t.TargetW
			}
		}
		if totalCharge <= 0 {
			// No charge commands to pull back — the overage is load-driven
			// and nothing this layer can do. Leave targets untouched;
			// the reactive fuse-saver below (forceFuseDischarge) can
			// still flip idle batteries to discharge.
			return out
		}
		newTotal := totalCharge - importOverage - buffer
		if newTotal < 0 {
			newTotal = 0
		}
		scale := newTotal / totalCharge
		for i := range out {
			if out[i].TargetW > 0 {
				out[i].TargetW *= scale
				out[i].Clamped = true
			}
		}
		// Latch the new charge cap for the hold window. Each fire
		// extends the window so the planner can't ramp through.
		state.FuseHoldMaxChargeW = newTotal
		state.FuseHoldUntil = now.Add(30 * time.Second)
	case exportOverage > 0:
		var totalDischarge float64
		for _, t := range out {
			if t.TargetW < 0 {
				totalDischarge += -t.TargetW
			}
		}
		if totalDischarge <= 0 {
			// Nothing to scale — PV alone is pushing past the fuse.
			// Fuse-guard can't curtail PV; pv_limit_w from the plan
			// is the lever in that scenario (annotateCurtailment).
			return out
		}
		newTotal := totalDischarge - exportOverage - buffer
		if newTotal < 0 {
			newTotal = 0
		}
		scale := newTotal / totalDischarge
		for i := range out {
			if out[i].TargetW < 0 {
				out[i].TargetW *= scale
				out[i].Clamped = true
			}
		}
		state.FuseHoldMaxDischargeW = newTotal
		state.FuseHoldUntil = now.Add(30 * time.Second)
	}
	return out
}

// fuseSafetyMarginW converts SiteFuseSafetyA into aggregate watts using
// the configured per-phase voltage and phase count. Returns 0 when the
// margin is unset OR the dependent fuse params are unset (back-compat
// with tests / e2e harness that wire only SiteFuseAmps). No hardcoded
// 230 V / 3 phases here — both come from config.
func (s *State) fuseSafetyMarginW() float64 {
	if s == nil || s.SiteFuseSafetyA <= 0 || s.SiteFuseVoltage <= 0 || s.SiteFusePhases <= 0 {
		return 0
	}
	return s.SiteFuseSafetyA * s.SiteFuseVoltage * float64(s.SiteFusePhases)
}

// effectiveImportCeilingW returns the binding ceiling for grid import in
// watts: the fuse limit minus its safety margin, further capped by
// PeakImportCeilingW when the operator has opted into a tariff peak
// (PeakImportCeilingW > 0). Used by every import-side clamp in the
// dispatch path so peak and fuse share one enforcement surface.
//
// Peak is taken at face value (no additional safety subtraction) — the
// operator typed the contract limit, so the controller honours exactly
// that. The fuse retains its independent safety margin because it
// guards against a hardware breaker, not a billing line.
func (s *State) effectiveImportCeilingW(fuseMaxW float64) float64 {
	eff := fuseMaxW - s.fuseSafetyMarginW()
	if eff < 0 {
		eff = 0
	}
	if s != nil && s.PeakImportCeilingW > 0 && s.PeakImportCeilingW < eff {
		eff = s.PeakImportCeilingW
	}
	return eff
}

// perPhaseOverageW returns the wattage by which the worst single phase
// exceeds the per-phase fuse amperage (less the safety margin). 0 when
// within limits, when per-phase data isn't available, or when the
// per-phase clamp is disabled (state.SiteFuseAmps == 0). The meter
// driver must emit l1_a / l2_a / l3_a in DerReading.Data — Pixii,
// Ferroamp, and Sungrow all do this today.
//
// Direction-agnostic: phase amps from the meter are absolute magnitudes,
// so this function reports overage regardless of whether the breaker is
// being approached on the import or export side. The caller attributes
// the overage to whichever direction the aggregate grid is flowing.
func perPhaseOverageW(store *telemetry.Store, state *State) float64 {
	if state == nil || state.SiteFuseAmps <= 0 || state.SiteMeterDriver == "" {
		return 0
	}
	r := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	if r == nil || len(r.Data) == 0 {
		return 0
	}
	var d struct {
		L1A *float64 `json:"l1_a"`
		L2A *float64 `json:"l2_a"`
		L3A *float64 `json:"l3_a"`
	}
	if err := json.Unmarshal(r.Data, &d); err != nil {
		return 0
	}
	// Use absolute magnitude — drivers like Pixii decode per-phase amps
	// as signed i16, so export shows up as negative numbers and a naive
	// `*p > maxA` walk starting from 0 leaves maxA = 0 (no fire) even
	// when a phase is at -17 A. The fuse trips on current magnitude
	// regardless of direction, and the caller attributes the overage
	// to the active aggregate-grid direction.
	maxA := 0.0
	for _, p := range []*float64{d.L1A, d.L2A, d.L3A} {
		if p == nil {
			continue
		}
		a := math.Abs(*p)
		if a > maxA {
			maxA = a
		}
	}
	threshold := state.SiteFuseAmps - state.SiteFuseSafetyA
	if threshold < 0 {
		threshold = 0
	}
	if maxA <= threshold {
		return 0
	}
	v := state.SiteFuseVoltage
	if v <= 0 {
		v = 230 // back-compat for tests / e2e that wire only SiteFuseAmps
	}
	return (maxA - threshold) * v
}

// applyPlanSignFloor enforces "executed battery total must agree in sign
// with the plan's intent for this slot" — the safety rail described at
// the call-site comment. Only active in planner modes. When the sum of
// post-distribute, post-clamp targets has the opposite sign to the
// active slot's plan intent, every target is forced to zero (idle).
//
// Sources of plan intent (first-non-empty wins):
//   - state.SlotDirective(now).BatteryEnergyWh — energy-allocation path
//   - state.PlanTarget(now) "charge" mode → charge intent
//   - state.PlanTarget(now) "self_consumption" with negative gridW →
//     discharge-to-export intent (matches mpc.actionToSlot's mapping)
//
// If no source is available (no planner callbacks wired, or both
// returned !ok) the floor is a no-op — there's no intent to compare
// against. Threshold of 100 W matches mpc.IdleGateThresholdW so a
// near-zero plan target counts as "idle, no opinion on sign".
func applyPlanSignFloor(targets []DispatchTarget, state *State) []DispatchTarget {
	if len(targets) == 0 || state == nil || !state.Mode.IsPlannerMode() {
		return targets
	}
	intent := planSignIntent(state)
	if intent == 0 {
		return targets
	}
	var sumTarget float64
	for _, t := range targets {
		sumTarget += t.TargetW
	}
	const idleBand = 100.0
	if math.Abs(sumTarget) < idleBand {
		return targets
	}
	execSign := 0
	if sumTarget > idleBand {
		execSign = 1
	} else if sumTarget < -idleBand {
		execSign = -1
	}
	if execSign == 0 || execSign == intent {
		return targets
	}
	slog.Warn("plan/exec sign mismatch — clamping to idle for this tick",
		"plan_intent", intent, "exec_sum_w", sumTarget, "mode", string(state.Mode))
	out := make([]DispatchTarget, len(targets))
	for i, t := range targets {
		out[i] = DispatchTarget{Driver: t.Driver, TargetW: 0, Clamped: true}
	}
	return out
}

// planSignIntent returns +1 for charge, -1 for discharge, 0 for idle /
// unknown. Reads SlotDirective first (energy path), falls back to
// PlanTarget (legacy path). Centralises the multi-source lookup so the
// floor and any future intent-aware code stay aligned.
func planSignIntent(state *State) int {
	const idleWh = 50.0     // a near-zero per-slot energy is idle, not signed
	const idleGridW = 100.0 // matches mpc.IdleGateThresholdW for sign decisions
	if state.SlotDirective != nil {
		if dir, ok := state.SlotDirective(time.Now()); ok {
			if dir.BatteryEnergyWh > idleWh {
				return +1
			}
			if dir.BatteryEnergyWh < -idleWh {
				return -1
			}
			return 0
		}
	}
	if state.PlanTarget != nil {
		if modeStr, gridW, ok := state.PlanTarget(time.Now()); ok {
			switch Mode(modeStr) {
			case ModeCharge:
				return +1
			case ModeSelfConsumption:
				// mpc.actionToSlot encodes a planned discharge-to-export
				// as ("self_consumption", negative_grid_w). Mirror that
				// mapping here: negative grid target = discharge intent.
				if gridW < -idleGridW {
					return -1
				}
				return 0
			}
		}
	}
	return 0
}

// fuseSaverFromZero is the early-return entry point for the fuse-saver.
// Called from ComputeDispatch branches that would otherwise return nil
// (idle mode, holdoff window) so the safety primary still gets a chance
// to fire before we walk away from the cycle. Builds zero-W targets for
// every battery that is BOTH online (per DriverHealth) AND has a current
// DerBattery reading, then runs them through forceFuseDischarge; if no
// overflow is predicted, the result is nil (caller sees the same
// empty-dispatch behaviour as before). Filtering to online+telemetry
// matches ComputeDispatch's main path and avoids commanding offline
// batteries via the fuse-saver back door.
func fuseSaverFromZero(
	store *telemetry.Store,
	state *State,
	driverCapacities map[string]float64,
	fuseMaxW float64,
) []DispatchTarget {
	if fuseMaxW <= 0 || len(driverCapacities) == 0 || store == nil {
		return nil
	}
	zeros := make([]DispatchTarget, 0, len(driverCapacities))
	for name := range driverCapacities {
		h := store.DriverHealth(name)
		if h == nil || !h.IsOnline() {
			continue
		}
		if r := store.Get(name, telemetry.DerBattery); r == nil {
			continue
		}
		zeros = append(zeros, DispatchTarget{Driver: name, TargetW: 0})
	}
	if len(zeros) == 0 {
		return nil
	}
	out := forceFuseDischarge(zeros, store, state, driverCapacities, fuseMaxW)
	for _, t := range out {
		if t.TargetW != 0 || t.Clamped {
			return out
		}
	}
	return nil
}

// forceFuseDischarge is the reactive fuse-saver primary. It runs AFTER
// applyFuseGuard and unconditionally drains the home battery whenever
// predicted grid import would exceed the fuse, regardless of mode,
// regardless of operator intent (BatteryCoversEV toggle, planner
// allocation, manual_hold injecting unplanned EV draw, an oven
// turning on, or any other off-plan load).
//
// The contract: under no software-controllable circumstance should
// the operator's hardware fuse trip because the EMS sat idle while
// the meter was over the limit. The hardware breaker remains the
// final cutoff for sub-tick spikes; this layer eliminates
// steady-state overflow at the 5 s control-tick rate.
//
// Why this exists separately from applyFuseGuard:
//
//	applyFuseGuard scales POSITIVE (charge) targets DOWN to 0 when
//	import is over fuse. That helps when the planner asked for a
//	charge — it prevents the EMS from making the overflow worse —
//	but cannot help in the common "battery idle, surprise load" case
//	because there's no charge to shrink. The PR #206 manual_hold
//	ramp test surfaced this: the EV was pinned at ~5.5 kW while the
//	home battery sat at 0 W per the planner's idle slot, and gridW
//	went over fuseSafeMaxW until the operator stopped the test.
//
// Algorithm:
//
//  1. Recompute predicted gridW after applyFuseGuard's scaling
//     (currentGrid − currentBat + sumTarget). currentGrid is the
//     live meter reading and reflects ALL loads including off-plan
//     EV draw / manual_hold / unplanned spikes.
//  2. If predicted ≤ fuseMaxW: nothing to do.
//  3. Otherwise allocate `overage = predicted − fuseMaxW` of
//     additional discharge, distributed proportionally to each
//     online battery's remaining discharge headroom (per-battery
//     MaxDischargeW − current target magnitude, gated on SoC ≥ 5 %).
//  4. Mark every modified target Clamped so the dispatch trace
//     shows the fuse-saver fired.
//
// Out of scope: sub-tick reactivity. A 5 s tick is the floor here;
// going faster requires pushing the dispatch loop down to ~1 s.
// Hardware fuse trips remain the only protection for sub-tick spikes.
func forceFuseDischarge(
	targets []DispatchTarget,
	store *telemetry.Store,
	state *State,
	driverCapacities map[string]float64,
	fuseMaxW float64,
) []DispatchTarget {
	if fuseMaxW <= 0 || len(targets) == 0 || state == nil {
		return targets
	}
	// Sum currentBat only across the batteries we're about to control
	// — uncontrolled or offline batteries' current draw is captured in
	// the live grid reading already; counting them again would
	// double-subtract their contribution and mispredict.
	seenBat := make(map[string]struct{}, len(targets))
	var currentBat float64
	for _, t := range targets {
		if _, seen := seenBat[t.Driver]; seen {
			continue
		}
		seenBat[t.Driver] = struct{}{}
		if r := store.Get(t.Driver, telemetry.DerBattery); r != nil {
			currentBat += r.SmoothedW
		}
	}
	var currentGrid float64
	if state.SiteMeterDriver != "" {
		if r := store.Get(state.SiteMeterDriver, telemetry.DerMeter); r != nil {
			currentGrid = r.SmoothedW
		}
	}
	var sumTarget float64
	for _, t := range targets {
		sumTarget += t.TargetW
	}
	predicted := currentGrid - currentBat + sumTarget

	// Effective import ceiling: fuse minus safety margin, capped further
	// by PeakImportCeilingW when the operator has set a tariff peak.
	// This is what makes the reactive fuse-saver also defend the peak —
	// the battery briefly bridges while the loadpoint controller ramps
	// the EV down in response to the joint allocator's FuseEVMaxW.
	// Per-phase overage stays on the fuse-only path below; tariff is
	// aggregate, not per-phase.
	effImportW := state.effectiveImportCeilingW(fuseMaxW)
	overage := predicted - effImportW
	// Per-phase overage trumps aggregate when bigger — but ONLY on the
	// import side. perPhaseOverageW is direction-agnostic (uses |amps|),
	// so an export-side phase trip would otherwise cause this function
	// to command MORE discharge, pushing the over-current phase further
	// over the breaker. applyFuseGuard's exportOverage branch already
	// shrinks discharge for that case before we run.
	//
	// Gate on `currentGrid` (live aggregate at the meter) rather than
	// `predicted`. Per-phase amps and currentGrid come from the same
	// DerMeter sample; predicted mixes in sumTarget which can swing
	// across 0 and silently flip the gate.
	if currentGrid >= 0 {
		perPhaseOverage := perPhaseOverageW(store, state) * 3.0
		if perPhaseOverage > overage {
			overage = perPhaseOverage
		}
	}
	if overage <= 0 {
		return targets
	}

	type slot struct {
		idx      int
		headroom float64
	}
	slots := make([]slot, 0, len(targets))
	var totalHeadroom float64
	for i, t := range targets {
		cap, ok := driverCapacities[t.Driver]
		if !ok || cap <= 0 {
			continue
		}
		r := store.Get(t.Driver, telemetry.DerBattery)
		if r == nil {
			continue
		}
		soc := 0.1
		if r.SoC != nil {
			soc = *r.SoC
		}
		if soc < 0.05 {
			continue // empty pack — can't draw on it
		}
		lim := state.DriverLimits[t.Driver]
		dCap := lim.MaxDischargeW
		if dCap <= 0 {
			dCap = MaxCommandW
		}
		var alreadyDischarging float64
		if t.TargetW < 0 {
			alreadyDischarging = -t.TargetW
		}
		room := dCap - alreadyDischarging
		if room <= 0 {
			continue
		}
		slots = append(slots, slot{idx: i, headroom: room})
		totalHeadroom += room
	}
	if totalHeadroom <= 0 {
		return targets
	}

	allocate := overage
	if allocate > totalHeadroom {
		allocate = totalHeadroom
	}

	out := make([]DispatchTarget, len(targets))
	copy(out, targets)
	for _, s := range slots {
		share := allocate * (s.headroom / totalHeadroom)
		// Subtract `share` from the existing target — that gives the
		// algorithm one consistent rule across all signs:
		//   +3000 - 2960 = +40   (still charging, but reduced)
		//    0    - 2960 = -2960 (idle → discharge)
		//   -1000 - 960  = -1960 (already discharging → more)
		// The net change to sumTarget is exactly `share`, so the
		// post-dispatch predicted gridW lands at fuseMaxW. Setting
		// TargetW = -share when positive (the prior implementation)
		// over-corrected by the original charge magnitude — letting
		// out`predicted` undershoot the fuse and discharging more
		// than necessary.
		out[s.idx].TargetW -= share
		out[s.idx].Clamped = true
	}
	return out
}
