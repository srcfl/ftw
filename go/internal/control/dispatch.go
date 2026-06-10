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
	// missing, planner_self holds batteries at 0 W until a fresh plan
	// exists; the price-aware mode must not silently absorb PV during
	// high-price export windows just because the planner is unavailable.
	// Other planner modes fall back to self_consumption behavior and log.
	// The three flavors mirror mpc.Mode — the difference is only what
	// the planner is allowed to do when it builds the plan:
	//   - planner_self:        no grid-charging, no battery export
	//   - planner_cheap:       grid-charge ok, no export discharge
	//   - planner_passive_arb: charge from cheapest (PV or grid), no
	//                          export discharge (merges planner_self
	//                          and planner_cheap as of v0.82)
	//   - planner_arbitrage:   full freedom within SoC + power limits
	ModePlannerSelf             Mode = "planner_self"
	ModePlannerCheap            Mode = "planner_cheap"
	ModePlannerPassiveArbitrage Mode = "planner_passive_arbitrage"
	ModePlannerArbitrage        Mode = "planner_arbitrage"
)

// AllModes is the canonical, ordered list of every operator-selectable
// Mode. It is the single source of truth: the API mode validator and the
// Home Assistant discovery `select` options both derive from it, so a new
// mode can't be added to the enum without automatically appearing in both
// places. Order is operator-facing (simple → advanced → planner), so it's
// also a safe order to render in a UI dropdown.
func AllModes() []Mode {
	return []Mode{
		ModeIdle, ModeSelfConsumption, ModePeakShaving,
		ModeCharge, ModePriority, ModeWeighted,
		ModePlannerSelf, ModePlannerCheap,
		ModePlannerPassiveArbitrage, ModePlannerArbitrage,
	}
}

// IsValidMode reports whether s names a known Mode.
func IsValidMode(m Mode) bool {
	for _, valid := range AllModes() {
		if m == valid {
			return true
		}
	}
	return false
}

// PlannerMPCMode maps a planner Mode to the mpc.Mode strategy the planner
// should build its plan with. ok is false for every non-planner mode, so a
// caller can gate MPC propagation on it without a separate IsPlannerMode
// check and without risking a zero-value mpc.Mode("") being pushed for an
// unmapped planner mode. It is the single source of truth for the
// control.ModePlanner* → mpc.Mode mapping: the API mode setter, the HA
// command callback, and the startup mode-restore all derive from it, so a
// new planner mode can't be wired into one path and forgotten in another.
func PlannerMPCMode(m Mode) (mpc.Mode, bool) {
	switch m {
	case ModePlannerSelf:
		return mpc.ModeSelfConsumption, true
	case ModePlannerCheap:
		return mpc.ModeCheapCharge, true
	case ModePlannerPassiveArbitrage:
		return mpc.ModePassiveArbitrage, true
	case ModePlannerArbitrage:
		return mpc.ModeArbitrage, true
	}
	return "", false
}

// IsPlannerMode reports whether the mode is one of the planner modes.
func (m Mode) IsPlannerMode() bool {
	return m == ModePlannerSelf ||
		m == ModePlannerCheap ||
		m == ModePlannerPassiveArbitrage ||
		m == ModePlannerArbitrage
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

	// PlannedGridW is the plan's forecast of slot-average gridW given the
	// planned battery / load / PV mix (site-signed: + = import). The
	// energy-dispatch path uses it as a CHARGE-ONLY soft reactive cap:
	// on a charge slot (targetTotalW > 0), don't push live gridW past
	// plan in the import direction. When live PV / load drifts away
	// from the plan's forecast — e.g. a cloud cuts PV mid-slot during
	// a planner_arbitrage charge slot that expected to charge off
	// solar — the cap pulls the battery target toward "what would make
	// live gridW match plan", preventing the energy budget from blindly
	// driving extra grid import.
	//
	// Discharge slots are intentionally NOT clamped on the energy-allocation
	// path: extra export during an EXPORT-INTENT slot (PlannedGridW < 0,
	// e.g. peak-shave discharge picked by the DP for its export price) is
	// bonus revenue and backing off would undermine the DP choice. See
	// docs/safety.md §8 for the full rationale.
	//
	// Cover-load discharge slots (PlannedGridW ≈ 0 or import) are a
	// DIFFERENT story: the DP picked discharge to offset an expensive
	// import, not to export. There the energy path is rerouted to reactive
	// PI on grid=0 — see the cover-load carve-out a few hundred lines
	// below where useEnergyPath is decided.
	//
	// HasPlannedGridW gates whether the cap should consult PlannedGridW
	// at all. Stored as a separate bool (rather than a *float64) so the
	// per-tick directive bridge in main.go doesn't escape-allocate. The
	// zero-value SlotDirective used by existing tests / legacy callers
	// has HasPlannedGridW=false → cap opts out by default.
	PlannedGridW    float64
	HasPlannedGridW bool

	// LoadpointEnergyWh is the current slot's planned EV charging budget.
	// Runtime uses planned EV energy to keep surplus-only EV from being
	// satisfied by home-battery discharge if a stale plan tries.
	LoadpointEnergyWh map[string]float64
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
// `TargetW` is in site sign convention: positive charges the battery
// (battery becomes a load, site imports more); negative discharges it
// (battery becomes a source, site imports less).
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
	Mode            Mode
	GridTargetW     float64
	GridToleranceW  float64
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

	// MaxExportW is a hard export ceiling (W, magnitude) enforced in EVERY
	// mode, at or below the physical fuse. Default 0 = disabled (export
	// bounded only by the fuse). When > 0 the export side of the fuse
	// guard uses min(fuse−margin, MaxExportW) as its threshold and scales
	// battery discharge back so predicted export stays under it. Protects
	// inverters that trip on sustained export well below the breaker
	// rating — the recurring Ferroamp EnergyHub 0x8030 fault after ~8 kW
	// sustained midday export. Sourced from site.max_export_w.
	MaxExportW float64
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

	// EVCurtailHeadroomW is the parallel quantity sized for the
	// PV-curtail decision. EVSurplusOnlyReserveW above is intentionally
	// 0 for plugged-but-not-drawing EVs (so the battery can claim
	// surplus a refusing EV won't), which is wrong when deciding
	// whether to cut PV — a stopped EV with SoC headroom would start
	// drawing if PV were allowed to grow above its min charge.
	// loadpoint.SurplusPotentialW computes this more permissive
	// reserve and main.go writes it here each tick.
	EVCurtailHeadroomW float64
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
	SlewRateW float64
	// SlewEnabled gates the per-cycle ramp limiter. Disabled = trust
	// the inverter's internal ramp control entirely (commands jump to
	// the PI's computed target). See Site.SlewEnabled in config for
	// the operator-facing rationale.
	SlewEnabled          bool
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

	// slotActualWh + slotActualLastTs + slotActualSlotStart are the
	// path-agnostic per-slot delivery accumulator. Updated on EVERY
	// dispatch tick (every mode, every path) from the live battery
	// aggregate — independent of the energy-allocation bookkeeping
	// above (which only updates inside useEnergyPath).
	//
	// The point is observability: when reactive paths (cover-load
	// discharge, planner_self, planner_passive_arbitrage idle slots,
	// the planner_arbitrage cover-load carve-out from PR #378) execute,
	// the existing slotDelivered does not track them. Without an
	// independent accumulator there's no way to measure whether those
	// paths over- or under-deliver vs the plan's BatteryEnergyWh.
	//
	// At slot rollover (SlotStart change) the just-ended slot is
	// evaluated and over/under-delivery is logged + counted. The
	// resulting counters surface on /api/status so operators can spot
	// systemic forecast vs reality drift. Site-signed: negative = the
	// fleet discharged Wh during the slot.
	slotActualWh        float64
	slotActualLastTs    time.Time
	slotActualSlotStart time.Time
	slotActualPlannedWh float64 // planned BatteryEnergyWh cached for the slot in flight

	// SlotDeliveryStats counts how many slots ended with the actual
	// fleet delivery falling outside ±50 % of the planned magnitude.
	// Idle/charge slots with |planned| ≤ 50 Wh are ignored — the
	// ratio is meaningless when the denominator is ~0. Read-only from
	// the API; mutated only inside ComputeDispatch under the caller's
	// outer ctrlMu.
	SlotDeliveryStats SlotDeliveryStats

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

	// DCLinkProtectionEnabled opts into a live-state curtail trigger
	// that fires INDEPENDENTLY of the planner directive when the
	// inverter's DC link is most exposed to a load-step fault: SoC
	// near full + PV significantly exceeds load. 2026-05-25 incident:
	// a 2.7 kW load step under 6 kW PV + 85 % SoC tripped Ferroamp's
	// internal protection. Pre-curtailing PV to load+margin keeps the
	// inverter's headroom inside the safe window so the same step
	// doesn't push it past the DC-link overvoltage threshold.
	//
	// Disabled by default — operators on hardware that doesn't trip
	// or who'd rather lose a few % of PV-export revenue than absorb
	// the risk can leave it off.
	DCLinkProtectionEnabled bool
	// DCLinkProtectionSoCThreshold is the SoC fraction (0-1) above
	// which the protective curtail engages. 0 = use default 0.80.
	DCLinkProtectionSoCThreshold float64
	// DCLinkProtectionMarginW is the W of headroom kept above live
	// load. The curtail limit becomes load + margin so a load step
	// smaller than `margin` lands without curtail-recompute. 0 =
	// use default 1000.
	DCLinkProtectionMarginW float64

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

	// ManualPVHold pins a PV curtail cap for a bounded duration,
	// overriding whatever the planner's slot directive says about
	// PVLimitW. Driver=="" applies at the site-aggregate level
	// (split proportionally across drivers in SupportsPVCurtail by
	// live |PV|); Driver=="<name>" caps only that one driver and
	// leaves the rest uncapped. Hot-installed via POST
	// /api/pv/manual_hold; auto-expires. Zero ExpiresAt means inactive.
	ManualPVHold PVManualHold

	// SettlementAwareSelfConsumption lets self_consumption look at the
	// running net Wh inside the current fixed 15-minute settlement window
	// (00/15/30/45) and bias the live grid target negative when the slot
	// has already accumulated import. It is intentionally asymmetric:
	// prior import may be worked back by exporting from the battery, but
	// prior export is not "repaid" by importing from grid. That preserves
	// the self-consumption contract while matching how operators judge
	// the system over a billing window rather than one noisy second.
	SettlementAwareSelfConsumption bool

	settlementSlotStart time.Time
	settlementLastTs    time.Time
	settlementNetWh     float64
	settlementTargetW   float64
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

// PVManualHold is an operator-installed PV curtail override. See
// State.ManualPVHold for invariants.
//
//   - Driver=="" → site-aggregate cap; ComputePVCurtail splits LimitW
//     across SupportsPVCurtail drivers proportionally to their live |PV|.
//   - Driver=="<name>" → caps only that driver; other PV-curtail
//     drivers are left uncapped (and released if they were capped
//     under the planner directive on the previous tick).
//
// LimitW is the absolute cap in watts (≥ 0). 0 W means "force PV off"
// for the scoped surface — useful for verifying that the curtail action
// reaches the inverter at all.
type PVManualHold struct {
	Driver    string
	LimitW    float64
	ExpiresAt time.Time
}

// SetPVManualHold installs a PV curtail override. Caller must hold the
// outer ctrlMu. Zero ExpiresAt clears any active hold.
func (s *State) SetPVManualHold(h PVManualHold) {
	if h.ExpiresAt.IsZero() {
		s.ManualPVHold = PVManualHold{}
		return
	}
	s.ManualPVHold = h
}

// ClearPVManualHold removes any active PV hold. Caller must hold the
// outer ctrlMu. Idempotent.
func (s *State) ClearPVManualHold() {
	s.ManualPVHold = PVManualHold{}
}

// GetPVManualHold returns the active PV hold for `now`, lazily
// evicting an expired one. Caller must hold the outer ctrlMu.
func (s *State) GetPVManualHold(now time.Time) (PVManualHold, bool) {
	if s.ManualPVHold.ExpiresAt.IsZero() {
		return PVManualHold{}, false
	}
	if !now.Before(s.ManualPVHold.ExpiresAt) {
		s.ManualPVHold = PVManualHold{}
		return PVManualHold{}, false
	}
	return s.ManualPVHold, true
}

func resetEnergyDispatchBookkeeping(state *State) {
	state.currentDirective = SlotDirective{}
	state.slotDelivered = 0
	state.lastTickTs = time.Time{}
}

// SlotDeliveryStats tracks observed gaps between the plan's per-slot
// BatteryEnergyWh and the live fleet's actual delivered Wh. Updated at
// slot rollover by updateSlotDeliveryMetrics. Pure observability — no
// dispatch decision reads these.
type SlotDeliveryStats struct {
	OverDeliveryCount  uint64 `json:"over_delivery_count"`
	UnderDeliveryCount uint64 `json:"under_delivery_count"`
	// SignMismatchCount counts slots where the fleet moved energy in the
	// opposite direction from the plan (planned discharge → actual
	// charge, or vice-versa) for non-idle slots. Caught separately
	// because the magnitude-only over/under check would happily report
	// `|−425| ≈ |+425|` as on-target — the largest possible miss.
	SignMismatchCount uint64 `json:"sign_mismatch_count"`
}

// slotDeliveryOverThreshold + slotDeliveryUnderThreshold define the
// magnitude band that counts as "delivered within plan". The plan-vs-
// actual ratio is computed as |actual| / max(|planned|, 1); ratios
// outside [0.5, 1.5] increment the corresponding counter (when the
// slot wasn't an idle one — see slotDeliveryIdleWhCutoff).
const (
	slotDeliveryOverThreshold  = 1.5
	slotDeliveryUnderThreshold = 0.5
	slotDeliveryIdleWhCutoff   = 50.0
	slotDeliveryMaxTickDtS     = 300.0
)

// updateSlotDeliveryMetrics is the path-agnostic per-slot Wh tracker.
// It runs on EVERY dispatch tick (every mode, every path) so reactive
// paths that bypass the energy-allocation bookkeeping still feed an
// independent record of "what did the fleet actually deliver this
// slot, and how does that compare to the plan?". See State.slotActualWh
// for the rationale and the cover-load carve-out (PR #378) context.
//
// When SlotDirective is unset or returns ok=false the accumulator is
// paused for this tick: there's no slot context to attribute Wh to.
//
// When the directive's SlotStart advances the just-ended slot is
// scored: |actual| / max(|planned|, 1). Ratios > 1.5 log an
// over-delivery and bump OverDeliveryCount; ratios < 0.5 log
// under-delivery and bump UnderDeliveryCount. Idle/charge slots
// where |planned| ≤ slotDeliveryIdleWhCutoff are skipped — measuring
// a ratio against ~0 is meaningless.
//
// The first-ever tick (zero-value slotActualSlotStart) initialises
// the accumulator without emitting anything.
func updateSlotDeliveryMetrics(state *State, currentTotalW float64, now time.Time) {
	if state == nil || state.SlotDirective == nil {
		return
	}
	dir, ok := state.SlotDirective(now)
	if !ok {
		return
	}

	if state.slotActualSlotStart.IsZero() {
		state.slotActualSlotStart = dir.SlotStart
		state.slotActualLastTs = now
		state.slotActualWh = 0
		state.slotActualPlannedWh = dir.BatteryEnergyWh
		return
	}

	if !dir.SlotStart.Equal(state.slotActualSlotStart) {
		// Slot rollover: evaluate the just-ended slot using the
		// planned Wh cached from prior ticks. Reactive paths don't
		// touch state.currentDirective (that's energy-path bookkeeping),
		// so this accumulator keeps its own slotActualPlannedWh.
		plannedWh := state.slotActualPlannedWh
		if math.Abs(plannedWh) > slotDeliveryIdleWhCutoff {
			actualWh := state.slotActualWh
			// Sign mismatch is the categorical failure mode: we moved
			// energy in the opposite direction from the plan. Caught
			// before the magnitude-ratio check below, which would
			// otherwise treat opposite-sign-equal-magnitude (planned
			// −425, actual +425) as ratio 1.0 = on target.
			if plannedWh*actualWh < 0 && math.Abs(actualWh) > slotDeliveryIdleWhCutoff {
				slog.Info("dispatch slot sign mismatch",
					"mode", state.Mode,
					"planned_wh", plannedWh,
					"actual_wh", actualWh,
					"slot_start", state.slotActualSlotStart)
				state.SlotDeliveryStats.SignMismatchCount++
			} else {
				ratio := math.Abs(actualWh) / math.Max(math.Abs(plannedWh), 1)
				switch {
				case ratio > slotDeliveryOverThreshold:
					slog.Info("dispatch slot over-delivery",
						"mode", state.Mode,
						"planned_wh", plannedWh,
						"actual_wh", actualWh,
						"ratio", ratio,
						"slot_start", state.slotActualSlotStart)
					state.SlotDeliveryStats.OverDeliveryCount++
				case ratio < slotDeliveryUnderThreshold:
					slog.Info("dispatch slot under-delivery",
						"mode", state.Mode,
						"planned_wh", plannedWh,
						"actual_wh", actualWh,
						"ratio", ratio,
						"slot_start", state.slotActualSlotStart)
					state.SlotDeliveryStats.UnderDeliveryCount++
				}
			}
		}
		state.slotActualSlotStart = dir.SlotStart
		state.slotActualLastTs = now
		state.slotActualWh = 0
		state.slotActualPlannedWh = dir.BatteryEnergyWh
		return
	}

	dt := now.Sub(state.slotActualLastTs).Seconds()
	if dt > 0 && dt < slotDeliveryMaxTickDtS {
		state.slotActualWh += currentTotalW * dt / 3600.0
	}
	state.slotActualLastTs = now
	// Keep the planned value fresh in case a mid-slot replan changed it.
	state.slotActualPlannedWh = dir.BatteryEnergyWh
}

const (
	settlementSlotDuration = 15 * time.Minute
	settlementEnterNetWh   = 30.0
	settlementExitNetWh    = 10.0
	settlementMinRemainS   = 30.0
	settlementMaxDtS       = 60.0
	settlementTargetAlpha  = 0.35
	settlementMinSoC       = 0.50
)

func (s *State) resetSettlementAccounting() {
	s.settlementSlotStart = time.Time{}
	s.settlementLastTs = time.Time{}
	s.settlementNetWh = 0
	s.settlementTargetW = 0
}

func (s *State) settlementGridTarget(now time.Time, gridW float64) float64 {
	slotStart := now.Truncate(settlementSlotDuration)
	if s.settlementSlotStart.IsZero() ||
		!s.settlementSlotStart.Equal(slotStart) ||
		s.settlementLastTs.IsZero() ||
		s.settlementLastTs.Before(slotStart) ||
		s.settlementLastTs.After(now) {
		s.settlementSlotStart = slotStart
		s.settlementLastTs = now
		s.settlementNetWh = 0
		s.settlementTargetW = 0
		return 0
	}

	dtS := now.Sub(s.settlementLastTs).Seconds()
	s.settlementLastTs = now
	if dtS > 0 {
		if dtS > settlementMaxDtS {
			dtS = settlementMaxDtS
		}
		s.settlementNetWh += gridW * dtS / 3600.0
	}

	remainingS := slotStart.Add(settlementSlotDuration).Sub(now).Seconds()
	if remainingS < settlementMinRemainS {
		s.settlementTargetW = 0
		return 0
	}

	threshold := settlementEnterNetWh
	if s.settlementTargetW < 0 {
		threshold = settlementExitNetWh
	}
	if s.settlementNetWh <= threshold {
		s.settlementTargetW = smoothSettlementTarget(s.settlementTargetW, 0)
		return 0
	}
	rawTargetW := -s.settlementNetWh * 3600.0 / remainingS
	s.settlementTargetW = smoothSettlementTarget(s.settlementTargetW, rawTargetW)
	return s.settlementTargetW
}

func smoothSettlementTarget(prev, next float64) float64 {
	if math.Abs(next) < 1 && math.Abs(prev) < 1 {
		return 0
	}
	out := prev + (next-prev)*settlementTargetAlpha
	if next == 0 && math.Abs(out) < 5 {
		return 0
	}
	return out
}

func minBatterySoC(bats []batteryInfo) float64 {
	if len(bats) == 0 {
		return 0
	}
	min := 1.0
	for _, b := range bats {
		if b.soc < min {
			min = b.soc
		}
	}
	return min
}

type plannerSelfDecision struct {
	idleGate          bool
	exportSurplusGate bool
	// noChargeOnStalePlan: planner_self is price-aware, so a stale plan
	// has no opinion on whether right-now is an export slot or a charge
	// slot. The old "fall back to plain self_consumption" path could
	// silently absorb PV that the planner would have exported; the
	// briefly-tried "hold at 0 W" was the opposite mistake and left the
	// house importing through every restart. The middle path: run the
	// reactive grid-zero regulator (cover load with discharge as usual)
	// but block self-charge until a fresh plan exists, so unexpected
	// PV surplus exports instead of charging blind.
	noChargeOnStalePlan bool
}

func preparePlannerSelf(state *State, now time.Time) plannerSelfDecision {
	state.SetGridTarget(0)
	// Reset the energy-allocation bookkeeping so a future switch to
	// planner_cheap / planner_arbitrage within the same 15-minute slot
	// can't read stale `slotDelivered` accumulated before the operator
	// hopped through planner_self. Without this reset, the SlotStart
	// comparison in the energy path would match and skip its own rollover
	// reset. Codex P2 on PR #131.
	resetEnergyDispatchBookkeeping(state)

	dir, planFresh := plannerSelfDirectiveAt(state, now)
	if !planFresh {
		if !state.PlanStale {
			slog.Warn("planner_self: plan stale — discharge-only self_consumption (no self-charge until fresh plan)")
		}
		state.PlanStale = true
		return plannerSelfDecision{noChargeOnStalePlan: true}
	}

	state.PlanStale = false
	return plannerSelfDecision{
		idleGate:            plannerSelfDirectiveIsIdle(dir),
		exportSurplusGate:   plannerSelfDirectiveExportsSurplus(dir),
		noChargeOnStalePlan: false,
	}
}

func plannerSelfDirectiveAt(state *State, now time.Time) (SlotDirective, bool) {
	if state.SlotDirective == nil {
		return SlotDirective{}, false
	}
	return state.SlotDirective(now)
}

func plannerSelfDirectiveIsIdle(dir SlotDirective) bool {
	slotH := dir.SlotEnd.Sub(dir.SlotStart).Hours()
	if slotH <= 0 {
		return false
	}
	// Idle decision is derived from the plan's forecast baseline grid
	// (what the grid would be with the battery doing nothing: house load,
	// PV, and any planned EV draw), not only from the DP's quantized
	// battery action. Small forecast surplus/deficit can quantize to
	// BatteryEnergyWh=0; treating that as deliberate idle would hold the
	// battery at 0 while the same surplus exports or deficit imports.
	if dir.HasPlannedGridW {
		baselineW := dir.PlannedGridW - dir.BatteryEnergyWh/slotH
		return math.Abs(baselineW) < mpc.IdleGateThresholdW
	}
	return math.Abs(dir.BatteryEnergyWh)/slotH < mpc.IdleGateThresholdW
}

func plannerSelfDirectiveExportsSurplus(dir SlotDirective) bool {
	slotH := dir.SlotEnd.Sub(dir.SlotStart).Hours()
	if slotH <= 0 || !dir.HasPlannedGridW {
		return false
	}
	battW := dir.BatteryEnergyWh / slotH
	if battW > mpc.IdleGateThresholdW {
		return false
	}
	baselineW := dir.PlannedGridW - battW
	return baselineW < -mpc.IdleGateThresholdW
}

// NewState creates default control state (port of Rust ControlState::new).
func NewState(gridTargetW, gridToleranceW float64, siteMeter string) *State {
	pi := NewPI(0.5, 0.1, 3000, 10000)
	pi.Setpoint = gridTargetW
	return &State{
		Mode:                           ModeSelfConsumption,
		GridTargetW:                    gridTargetW,
		GridToleranceW:                 gridToleranceW,
		SiteMeterDriver:                siteMeter,
		PriorityOrder:                  nil,
		Weights:                        map[string]float64{},
		PeakLimitW:                     5000,
		EVChargingW:                    0,
		PI:                             pi,
		SlewRateW:                      500,
		SlewEnabled:                    true,
		MinDispatchIntervalS:           5,
		PrevTargets:                    map[string]float64{},
		UseCascade:                     true,
		SettlementAwareSelfConsumption: false,
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

	// Per-direction blocks the driver reports this cycle. A battery that
	// can't move in the demanded direction (e.g. a Ferroamp ESO floored at
	// its SoC limit) is excluded from that direction's split so its share
	// goes to a capable sibling instead of leaking to the grid. Stored as
	// "blocked" rather than "capable" so the zero value (false = not blocked
	// = capable) is the safe default: a battery a driver never flags, and
	// any directly-constructed batteryInfo, stays capable. See
	// batteryDirectionBlocks + distributeProportional.
	dischargeBlocked bool
	chargeBlocked    bool
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

// batteryDirectionBlocks reads the optional discharge_capable / charge_capable
// flags a driver may include in its battery emit (DerReading.Data) and reports
// whether the battery is BLOCKED in each direction this cycle. A direction is
// blocked only when the driver explicitly emits that *_capable flag as false
// (e.g. the Ferroamp driver when all its ESOs are floored). Absent, empty, or
// unparseable → not blocked: a driver that doesn't report capability is
// assumed able, keeping this backward-compatible with every existing driver.
func batteryDirectionBlocks(data json.RawMessage) (dischargeBlocked, chargeBlocked bool) {
	if len(data) == 0 {
		return false, false
	}
	var c struct {
		DischargeCapable *bool `json:"discharge_capable"`
		ChargeCapable    *bool `json:"charge_capable"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return false, false
	}
	dischargeBlocked = c.DischargeCapable != nil && !*c.DischargeCapable
	chargeBlocked = c.ChargeCapable != nil && !*c.ChargeCapable
	return
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
	// ---- Per-slot Wh delivery observability (path-agnostic) ----
	// Runs on EVERY tick before any mode/short-circuit decision so the
	// idle / charge / holdoff / reactive-fallback paths all contribute
	// to the actual-delivered accumulator. Independent of the
	// energy-allocation bookkeeping (slotDelivered) which only updates
	// inside useEnergyPath — that's why reactive cover-load discharge
	// (PR #378) and planner_passive_arbitrage idle slots are invisible
	// to that accumulator. This one isn't.
	//
	// Pure observability — log + counter only. No dispatch decision
	// reads SlotDeliveryStats; no Wh cap is applied to reactive paths
	// from this data. The point is to measure first, decide whether a
	// cap is warranted later.
	{
		now := time.Now()
		var liveBatTotal float64
		for name := range driverCapacities {
			if r := store.Get(name, telemetry.DerBattery); r != nil {
				h := store.DriverHealth(name)
				if h != nil && h.IsOnline() {
					liveBatTotal += r.SmoothedW
				}
			}
		}
		updateSlotDeliveryMetrics(state, liveBatTotal, now)
	}

	// ---- Planner modes: the plan is a scheduler, not a regulator ----
	// The plan decides WHEN each strategy applies (self-consumption now,
	// charge at 02:00, export at 17:00). The EMS decides HOW batteries
	// respond every 5 s based on the live meter.
	//
	// Three execution paths, selected by the operator-picked planner mode:
	//
	//   * planner_self — reactive self-consumption with per-slot gates
	//     from the plan. Idle slots are charge-only unless the plan's
	//     no-battery baseline explicitly exports PV, in which case the EMS
	//     holds battery power at 0 to preserve that export/headroom choice.
	//     Honours the mode's contract ("never imports to charge, never
	//     exports via the battery") against forecast error. See
	//     docs/plan-ems-contract.md §"Exception: planner_self" and issue
	//     #130.
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
	// planner_self gates constrain only the CHARGE direction. "Smart
	// self-consumption" is about WHEN to refill the battery, never
	// about importing electricity at any price the operator can't see.
	// Discharge to cover live load is always allowed; the operator's
	// floor is "never import what stored energy could've covered".
	//
	// plannerSelfIdleGate fires when the plan modeled this slot as
	// near-balanced (battery_w ≈ 0). Reactive PI runs as in plain
	// self_consumption; on the charge side, only PV surplus exceeding
	// IdleGateThresholdW is absorbed so PI noise doesn't trigger churn.
	//
	// plannerSelfExportSurplusGate fires when the plan modeled an
	// explicit export this slot. Reactive PI runs; on the charge side,
	// any charge is blocked so the surplus stays out the meter.
	//
	// plannerSelfNoChargeStalePlan is the missing-plan safety: identical
	// to exportSurplusGate's charge-block, applied when no fresh plan
	// exists.
	plannerSelfIdleGate := false
	plannerSelfExportSurplusGate := false
	plannerSelfNoChargeStalePlan := false
	// arbitrageFamilyIdleSlot tracks "we're in planner_passive_arbitrage on an
	// idle plan-slot (BatteryEnergyWh ≈ 0)". For these slots the DP picked
	// idle deliberately; the live-export gate below uses this to suppress
	// reactive absorption when actual conditions show PV surplus the
	// forecast missed (mirror of plannerSelfExportSurplusGate, but
	// triggered by LIVE grid sign rather than planned grid — since
	// passive_arbitrage idle slots can be set with planned grid near zero).
	arbitrageFamilyIdleSlot := false
	// coverLoadDischargeSlot: planner_arbitrage discharge slot the DP
	// picked for covering load rather than peak export (PlannedGridW ≈ 0).
	// Same outer-scope lift as arbitrageFamilyIdleSlot so the post-block
	// fall-through can recognise carve-out slots and force grid_target=0
	// instead of letting PlanTarget set the planned import.
	coverLoadDischargeSlot := false
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
		resetEnergyDispatchBookkeeping(state)
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
		decision := preparePlannerSelf(state, time.Now())
		plannerSelfIdleGate = decision.idleGate
		plannerSelfExportSurplusGate = decision.exportSurplusGate
		plannerSelfNoChargeStalePlan = decision.noChargeOnStalePlan
	case state.Mode.IsPlannerMode():
		// planner_cheap / planner_arbitrage.
		if state.UseEnergyDispatch && state.SlotDirective != nil {
			if dir, ok := state.SlotDirective(time.Now()); ok {
				currentDirective = dir
				// planner_arbitrage and planner_passive_arbitrage idle slots: skip the energy path and
				// fall through to reactive PI (same as planner_self does always).
				// When the plan slot is idle (BatteryEnergyWh ≈ 0), the energy
				// formula produces targetTotalW=0 and cannot react to live
				// conditions — a PV forecast miss leaves the site importing while
				// the battery sits at 0 W. The reactive PI path handles this
				// correctly, and planHasNonDischargeIntent (below) permits
				// discharge for non-charge arbitrage-family slots.
				// Charge slots (BatteryEnergyWh > idleWh) still use the energy
				// path so the DP's deliberate grid-charge intent is honoured.
				const idleWhGate = 50.0
				// |BatteryEnergyWh| ≤ idleWhGate — true idle only. A
				// planned-discharge slot is also ≤ idleWhGate by the
				// signed comparison alone (negative numbers satisfy
				// the inequality), and would incorrectly route the
				// live-export charge block onto a deliberate discharge
				// decision. arbitrage-family discharge slots are
				// caught by coverLoadDischargeSlot below. Codex P2 / #375
				// follow-up.
				arbitrageFamilyIdleSlot = (state.Mode == ModePlannerPassiveArbitrage ||
					state.Mode == ModePlannerArbitrage) &&
					math.Abs(dir.BatteryEnergyWh) <= idleWhGate
				// planner_arbitrage cover-load discharge slots: same fallthrough.
				// The energy path's "extra export is bonus revenue" carve-out
				// (see SlotDirective.PlannedGridW doc) is correct for peak-export
				// slots where the DP picked the slot for its export price. But
				// when PlannedGridW ≈ 0 (or import), the DP picked discharge
				// to *cover load* — no export was anticipated. Locking discharge
				// at the planned rate then exports any forecast-load undershoot
				// at the spot price the operator later buys back at consumer
				// price. Reactive PI on grid=0 fixes both directions: load
				// undershoot backs off; load overshoot ramps further. The
				// passive_arbitrage variant of this carve-out already covers
				// passive cover-load discharge (BatteryEnergyWh <= idleWh
				// captures non-charge slots). Operator-report 2026-05-28.
				const coverLoadExportToleranceW = 100.0
				// Same cover-load reasoning applies in both planner_arbitrage
				// and planner_passive_arbitrage. Both modes can plan a
				// discharge slot whose purpose is to *cover load* (not to
				// export at peak price); both need reactive PI on grid=0
				// when the forecast load is wrong. The earlier (#378)
				// implementation only listed planner_arbitrage and relied
				// on the passive_arbitrage flag's loose predicate to fold
				// in the passive variant — Codex P2 / #375 follow-up
				// tightened that flag to true-idle only, so include the
				// passive mode explicitly here.
				coverLoadDischargeSlot = (state.Mode == ModePlannerArbitrage ||
					state.Mode == ModePlannerPassiveArbitrage) &&
					dir.HasPlannedGridW &&
					dir.BatteryEnergyWh < -idleWhGate &&
					dir.PlannedGridW > -coverLoadExportToleranceW
				if !arbitrageFamilyIdleSlot && !coverLoadDischargeSlot {
					useEnergyPath = true
				}
				// Distribution mode is decoupled from planner strategy in
				// the energy path — the operator-selected strategy drives
				// the plan's DP, distribution is always proportional across
				// online batteries. If the operator wants priority or
				// weighted, they use the manual modes, not a planner mode.
				effectiveMode = ModeSelfConsumption
				state.PlanStale = false
				// Carve-out slots must chase grid=0, not the legacy
				// PlanTarget's planned grid. main.go wires PlanTarget
				// alongside SlotDirective; for arbitrage cover-load,
				// PlanTarget returns ("self_consumption", planned_import),
				// and falling through to the !useEnergyPath branch below
				// would SetGridTarget(+plannedImport) — defeating the
				// reactive carve-out entirely. Force the setpoint here
				// and skip the legacy lookup. Codex P1, PR #378 follow-up.
				if arbitrageFamilyIdleSlot || coverLoadDischargeSlot {
					state.SetGridTarget(0)
					// Mirror preparePlannerSelf (dispatch.go:797-804):
					// if the slot was previously on the energy path —
					// directly or via an operator mode-hop earlier in
					// the same 15-min window — slotDelivered /
					// lastTickTs / currentDirective hold stale values.
					// A future transition back to the energy path
					// within the same slot would then read those and
					// miscompute remainingWh. Reset on every carve-out
					// tick (idempotent).
					resetEnergyDispatchBookkeeping(state)
				}
			}
		}
		if !useEnergyPath && !arbitrageFamilyIdleSlot && !coverLoadDischargeSlot {
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
		state.resetSettlementAccounting()
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
		state.resetSettlementAccounting()
		targets := chargeAll(store, driverCapacities, state.DriverLimits)
		return applyDispatchSafetyPipeline(targets, store, state, driverCapacities, fuseMaxW, dispatchSafetyOptions{
			updatePrevTargets: true,
			recordDispatch:    true,
		})
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
	// BatteryCoversEV (default false) flips this in modes where the
	// operator wants batteries to cover EV draw. In normal self-consumption
	// the EV import is left to the grid unless BatteryCoversEV is enabled.
	gridW := rawGridW
	coverEV := state.BatteryCoversEV
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
		dischargeBlocked, chargeBlocked := batteryDirectionBlocks(r.Data)
		batteries = append(batteries, batteryInfo{
			driver:           name,
			capacityWh:       cap,
			currentW:         r.SmoothedW,
			soc:              soc,
			online:           h.IsOnline(),
			group:            state.InverterGroups[name],
			maxChargeW:       lim.MaxChargeW,
			maxDischargeW:    lim.MaxDischargeW,
			dischargeBlocked: dischargeBlocked,
			chargeBlocked:    chargeBlocked,
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
	// Planner slots that are idle/charge-only must not leave an individual
	// battery discharging just because slew anchored from a negative live
	// reading. Plain self_consumption remains the classic grid-zero mode:
	// it may discharge to cover local load and charge from live surplus.
	planNonDischargeIntent := !manualHoldActive && planHasNonDischargeIntent(state)
	// planner_self gates intentionally NOT in noSelfDischarge — the
	// operator's "never import" floor takes precedence over the
	// planner's slot preference, so discharge to cover load is always
	// allowed when planner_self is the active mode. The other planner
	// modes' non-discharge intent (planner_cheap / planner_arbitrage
	// during charging slots) remains in scope.
	noSelfDischarge := planNonDischargeIntent

	// ---- Sum of battery current power (site-signed) ----
	// Used by both paths: legacy distributors take (currentTotal + correction);
	// energy path computes correction as (desired_total - currentTotal).
	var currentTotal float64
	for _, b := range onlineBats {
		currentTotal += b.currentW
	}
	surplus := newSurplusAccounting(rawGridW, gridW, currentTotal, state)

	// passive_arbitrage idle slot + live PV surplus: don't absorb. Forecasts
	// guide FUTURE slots; for the slot we're already in, the live meter is
	// authoritative. The DP picked idle here on purpose — when actual
	// conditions diverge upward in PV (or downward in load) such that the
	// baseline-grid-with-battery-at-zero shows export, sustain the DP's
	// "don't charge" choice rather than letting reactive PI swallow it.
	// baselineGridW removes the batteries' current contribution from the
	// live meter — same shape as planner_self's planned-baseline gate, but
	// computed from telemetry instead of the (possibly-stale) plan.
	// Extended to cover-load discharge slots too (#379 follow-up): a
	// planned-discharge slot whose forecast load doesn't materialise must
	// not turn into "absorb the live PV surplus" via reactive PI on grid=0.
	// Battery stays at 0 in both directions for the slot — neither
	// reactive charge from PV nor force-export discharge.
	arbitrageFamilyIdleLiveExportGate := false
	if arbitrageFamilyIdleSlot || coverLoadDischargeSlot {
		baselineGridW := gridW - currentTotal
		if baselineGridW < -mpc.IdleGateThresholdW {
			arbitrageFamilyIdleLiveExportGate = true
		}
	}
	// CHARGE-direction safeties for planner_self. exportSurplusGate +
	// stale-plan block charge fully; idleGate applies a soft ceiling
	// computed from live PV surplus (handled below, not via this flag).
	// arbitrageFamilyIdleLiveExportGate is the live-grid mirror that
	// extends the same charge-block to planner_passive_arbitrage idle slots.
	noSelfCharge := !manualHoldActive && (plannerSelfExportSurplusGate || plannerSelfNoChargeStalePlan || arbitrageFamilyIdleLiveExportGate)

	// ---- Compute totalCorrection — paths diverge here ----
	var totalCorrection float64
	switch {
	case manualHoldActive:
		state.resetSettlementAccounting()
		// Drive the aggregate battery toward the operator's setpoint.
		// PI was already reset above; deadband is intentionally skipped
		// so even small setpoints are honoured exactly. Slew, SoC clamps,
		// and the fuse guard still apply downstream.
		totalCorrection = manualHold.PowerW - currentTotal
	case useEnergyPath:
		state.resetSettlementAccounting()
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
			// Mirror cap for the GROW-budget case: a mid-slot replan that
			// raises the slot's energy budget (e.g. peak-price reactive
			// replan late in a low-discharge slot) makes the old
			// slotDelivered look "behind" by a large margin, and
			// remainingWh × 3600 / remainingS demands catastrophic
			// catch-up power (>> slot avg, clamped to MaxDischargeW).
			// Observed on .139 2026-05-17: slot avg in plan was −900 W
			// but dispatch held −9000 W for 70 s near slot-end after a
			// mid-slot replan; battery hit its inverter limit.
			//
			// Rebase slotDelivered toward "expected at new pace" so the
			// catch-up rate stays close to slot average. The new plan is
			// treated as if it had been active since slot start, scaled
			// by elapsed fraction. Only applied when the actual delivery
			// is BEHIND the expected pace (catch-up direction); when
			// ahead, the asymmetric cap above already handles it.
			slotH := currentDirective.SlotEnd.Sub(currentDirective.SlotStart).Seconds() / 3600.0
			if slotH > 0 {
				elapsedH := now.Sub(currentDirective.SlotStart).Seconds() / 3600.0
				if elapsedH > 0 && elapsedH < slotH {
					expectedDelivered := (elapsedH / slotH) * currentDirective.BatteryEnergyWh
					if currentDirective.BatteryEnergyWh < 0 && state.slotDelivered > expectedDelivered {
						state.slotDelivered = expectedDelivered
					}
					if currentDirective.BatteryEnergyWh > 0 && state.slotDelivered < expectedDelivered {
						state.slotDelivered = expectedDelivered
					}
				}
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
		evActive := state.EVChargingW > evActiveThresholdW
		// CANONICAL "battery may not feed EV" accounting. The MPC's
		// NoBatteryToEV DP feasibility rule (mpc.go, see the
		// houseResidualW check inside the action loop) mirrors this
		// computation so the planner stops emitting allocations that
		// this clamp then has to censor. TODO(refactor): extract the
		// houseResidualW math + the (battW<0, evW>0) feasibility
		// predicate into a small helper consumed by both this clamp
		// and the DP rule, so a future change to the accounting can't
		// drift between plan and runtime.
		var plannedLoadpointEnergyWh float64
		for _, wh := range currentDirective.LoadpointEnergyWh {
			if wh > 0 {
				plannedLoadpointEnergyWh += wh
			}
		}
		surplusOnlyPlannedEV := state.EVSurplusOnlyReserveW > 0 && plannedLoadpointEnergyWh > 0
		if ((!state.BatteryCoversEV && evActive) || surplusOnlyPlannedEV) && targetTotalW < 0 {
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
			ceiling := surplus.chargeCeilingAfterEVReserveW()
			if targetTotalW > ceiling {
				targetTotalW = ceiling
			}
		}

		// Plan-grid soft reactive cap (charge-direction only).
		//
		// Catches the "cloud cuts PV mid-charge-slot, energy budget
		// chases plan via grid import" failure mode: plan thought
		// gridW ≈ 0 with PV doing the charging work, PV drops 3 kW,
		// live gridW imports — without this cap, `remainingWh × 3600
		// / remainingS` holds the planned charge power against the
		// real (now-grid-fed) PV deficit until the reactive replan in
		// mpc/service.go:266–290 fires (≥10 min later, gated by the
		// 500 Wh PV-error integral and the 60 s cooldown). The cap
		// compares the POST-DISPATCH projected grid against the plan,
		// then pulls the battery target toward "what target would make
		// projected gridW match plan". Floored at 0 so the cap can
		// never flip dispatch direction.
		//
		// CHARGE-ONLY by design. The mirror case (discharge slot,
		// live gridW more negative than plan) is intentionally NOT
		// clamped:
		//
		//   - Battery delivers the planned discharge Wh either way;
		//     the "extra" export to grid comes from load undershooting
		//     forecast, not from over-discharging. Backing off would
		//     leave Wh in the battery for a later slot the DP already
		//     evaluated and rejected — undermining the DP's choice.
		//   - The economics are asymmetric: extra import during a
		//     charge slot costs the operator (paying for energy the
		//     plan assumed PV would supply); extra export during a
		//     discharge slot is bonus revenue at the slot's chosen
		//     export price.
		//   - Discharge-direction divergence (live import > plan,
		//     e.g. load surged) is left to the reactive replan +
		//     downstream clamps (fuse, SoC floor, EV-discharge cap).
		//
		// The charging slot's opposite-direction case (live gridW
		// more negative than plan because PV came in higher than
		// forecast) is handled above by the PV surplus absorber
		// (dispatch.go ~line 866), which opportunistically *adds*
		// charge.
		//
		// 100 W deadband matches IdleGateThresholdW / evActiveThresholdW
		// elsewhere in this package — below it, the projected-grid
		// divergence is meter noise / smoothing residue and the energy
		// path keeps following plan.
		if currentDirective.HasPlannedGridW && targetTotalW > 0 {
			const planGridDeadband = 100.0
			projectedGridW := rawGridW + (targetTotalW - currentTotal)
			gridErr := projectedGridW - currentDirective.PlannedGridW
			if gridErr > planGridDeadband {
				adjusted := targetTotalW - gridErr
				// The back-off normally floors at 0 (charge → idle): on a
				// deliberate grid-charge slot the plan meant to import, so a
				// load surge must not flip it to discharge and undo the refill.
				// But on a planner_arbitrage charge-from-PV-surplus slot
				// (PlannedGridW below the grid-charge band — the DP only meant
				// to soak surplus, not buy from the grid), let the target go
				// negative so the battery covers the live load surge, driving
				// projected grid back toward PlannedGridW (~0). This is the
				// charge-side mirror of the discharge-slot cover-load carve-out;
				// downstream SoC floor / fuse guard / slew still bound the
				// discharge, and planHasNonDischargeIntent permits it for exactly
				// these slots. Operator report 2026-05-30.
				if adjusted < 0 && !coverLoadChargeSlot(state, currentDirective) {
					adjusted = 0
				}
				targetTotalW = adjusted
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
		// Only bias the grid signal (hold export back for the EV) when the EV
		// can actually use the reserve. When it can't — stopped AND surplus
		// below its start power — leave biasedGridW = gridW so the PI sees the
		// real export and the home battery absorbs the surplus instead of
		// reserving it for an EV that can't take it. Mirrors the charge
		// ceiling's evCanUseReserve() release so both hold-backs lift together.
		if state.EVSurplusOnlyReserveW > 0 && effectiveMode == ModeSelfConsumption &&
			surplus.evCanUseReserve() {
			reserveRemaining := surplus.evReserveRemainingW
			if gridW < -reserveRemaining {
				biasedGridW = gridW + reserveRemaining
			} else if gridW < 0 {
				biasedGridW = 0
			}
			// gridW >= 0: leave biasedGridW = gridW (import behavior).
		}

		activeGridTargetW := state.GridTargetW
		if effectiveMode == ModeSelfConsumption &&
			!noSelfDischarge &&
			state.SettlementAwareSelfConsumption &&
			minBatterySoC(onlineBats) >= settlementMinSoC {
			settlementTargetW := state.settlementGridTarget(time.Now(), gridW)
			if settlementTargetW < activeGridTargetW {
				activeGridTargetW = settlementTargetW
			}
		} else {
			state.resetSettlementAccounting()
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
			errW = biasedGridW - activeGridTargetW
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
		if !surplusActive && math.Abs(errW) < state.GridToleranceW &&
			!(noSelfDischarge && anyBatteryDischarging(onlineBats)) {
			return nil
		}

		// Outer PI — drives total correction we want across all batteries.
		// Site convention: gridW positive = too much import. Modes that may
		// discharge ask batteries for negative power; planner idle/charge
		// slots floor that negative target to 0 below.
		// PI setpoint = activeGridTargetW, measurement = gridW.
		// For PeakShaving we feed a slightly different measurement so the same PI works.
		var piMeasurement float64
		if effectiveMode == ModePeakShaving {
			piMeasurement = state.GridTargetW + errW
		} else {
			piMeasurement = biasedGridW
		}
		prevPISetpoint := state.PI.Setpoint
		if effectiveMode != ModePeakShaving {
			state.PI.Setpoint = activeGridTargetW
		}
		out := state.PI.Update(piMeasurement)
		state.PI.Setpoint = prevPISetpoint
		totalCorrection = out.Output

		// Live-meter clamp on the legacy PI path: plan decides charge or
		// discharge direction; the live error decides magnitude. Prevents
		// the load-twin over-prediction case where reactive PI commands a
		// discharge larger than what's needed to close errW. Scoped to
		// this default arm only — manualHold, useEnergyPath, and
		// plannerSelfIdleGate each have their own contracts that
		// intentionally cross the GridTargetW line.
		//
		// Derivation. With load and PV held constant within a tick,
		// gridW moves 1:1 with bat (conservation: grid = load + bat + pv).
		// So the new battery target lands gridW at GridTargetW when
		//
		//   idealTarget := currentTotal − errW
		//
		// The PI's request `targetTotal = currentTotal + totalCorrection`
		// can fall into three regions:
		//   - between currentTotal and idealTarget   → on the recovery
		//                                                path, pass through
		//   - past idealTarget (overshoot direction) → cap to idealTarget
		//   - past currentTotal in the OPPOSITE direction (wrong-way move,
		//     typically PI integrator windup from a prior mode)
		//                                              → hold at currentTotal
		//                                                until integrator
		//                                                unwinds
		//
		// History. The earlier formula `allowed = ±errW` (i.e. -errW for
		// the discharge arm, headroom-style for the charge arm) ignored
		// currentTotal entirely, which (a) pinned bat at exactly the
		// level reproducing the current grid error — a self-consistent
		// stuck state whenever load exceeded |errW|, the steady-state
		// case for any house with continuous load above the gap; and (b)
		// in its switch on `targetTotal > 0 / < 0` accidentally folded
		// the "PI overshooting during a natural recovery" case in with
		// the "wrong-direction windup" case, hard-cutting bat to 0 mid-
		// recovery and introducing visible flapping (commit 80456a1
		// dropped that branch but left the wrong-direction case
		// unprotected — Copilot caught the regression on PR #276).
		//
		// Deadband (state.GridToleranceW) already gated entry to this
		// arm at the abs(errW) < dead check above; no second deadband
		// haircut here.
		targetTotal := currentTotal + totalCorrection
		idealTarget := currentTotal - errW
		// correctionDir = sign(targetTotal − currentTotal); needs to
		// match sign(idealTarget − currentTotal) = sign(−errW). When
		// they disagree, PI is moving the wrong way (windup). Use a
		// small epsilon so PI noise near zero doesn't classify as
		// "wrong direction".
		const wrongDirEpsW = 1.0
		correctionDir := targetTotal - currentTotal
		var allowed float64
		switch {
		case errW > 0 && correctionDir > wrongDirEpsW:
			// Importing but PI wants to charge — wrong-direction windup.
			allowed = currentTotal
			// Actively unwind the integral. Without this the integral
			// stays load-bearing in the wrong direction across every
			// subsequent cycle and the controller is "stuck" until the
			// next opposite-signed error happens to drain it naturally
			// — that took ~3 min during the 2026-05-25 morning sunrise
			// after the prior evening's mode-switch windup. Decay to
			// half each cycle the clamp fires.
			state.PI.DecayIntegral(0.5)
		case errW < 0 && correctionDir < -wrongDirEpsW:
			// Exporting but PI wants to discharge — wrong-direction windup.
			allowed = currentTotal
			state.PI.DecayIntegral(0.5)
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
		if allowed != targetTotal {
			slog.Warn("dispatch: meter clamp reduced battery target",
				"requested_total_w", targetTotal,
				"clamped_total_w", allowed,
				"ideal_target_w", idealTarget,
				"grid_w", gridW,
				"grid_target_w", activeGridTargetW,
				"err_w", errW,
				"current_total_w", currentTotal,
				"deadband_w", state.GridToleranceW,
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
				ceiling := surplus.chargeCeilingAfterEVReserveW()
				if targetTotal2 > ceiling {
					totalCorrection = ceiling - currentTotal
				}
			} else if targetTotal2 < 0 && gridW <= 0 {
				// No-discharge floor: the EV reserve must only REDUCE
				// battery charging to free surplus — never push the
				// battery into discharge to manufacture the reserved
				// export. When PV is below the reserve, the within-reserve
				// grid bias (biasedGridW=0 in the −reserve..0 band) makes
				// the PI want to discharge toward the export target; left
				// unchecked it drains the pack to grid (observed on
				// Stefan's site: a plugged EV reserved 4.14 kW while PV was
				// 1.7 kW → battery discharged ~4 kW to grid, and the EV
				// still couldn't start). Floor at idle while the grid is
				// exporting/balanced. Household-import load coverage
				// (gridW > 0) is intentionally left untouched.
				totalCorrection = -currentTotal
			}
		}
		if noSelfDischarge {
			targetTotal2 := currentTotal + totalCorrection
			if targetTotal2 < 0 {
				totalCorrection = -currentTotal
			}
		}
		if noSelfCharge {
			targetTotal2 := currentTotal + totalCorrection
			if targetTotal2 > 0 {
				totalCorrection = -currentTotal
			}
		}
		// planner_self idle slot: cap charge to the threshold-filtered
		// PV surplus. Reactive PI may have commanded any positive
		// number from live near-balanced grid noise — only allow
		// absorption when there's genuine surplus that the planner
		// would also have chosen to capture. Discharge stays unconstrained
		// so live import is still covered.
		if plannerSelfIdleGate {
			chargeCeiling := plannerSelfIdleDesiredTotal(surplus, state)
			targetTotal2 := currentTotal + totalCorrection
			if targetTotal2 > chargeCeiling {
				totalCorrection = chargeCeiling - currentTotal
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
			h := store.DriverHealth(r.Driver)
			if h == nil || !h.IsOnline() {
				continue
			}
			group := state.InverterGroups[r.Driver]
			if group == "" {
				continue // untagged PV: no locality signal, treat as AC-bus
			}
			if r.SmoothedW >= 0 {
				continue // not generating right now
			}
			// PV is site-signed (negative = generating). Magnitude = surplus
			// potentially routable DC-direct to the same-group battery.
			groupPV[group] += -r.SmoothedW
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
		// Planned PV-export slots are explicitly trying to stop battery
		// motion and let the surplus cross the meter. Slew-limiting that
		// stop keeps a charging battery absorbing PV for multiple control
		// cycles, which visibly violates the smart self-consumption plan.
		// A direct move to 0 W is safe here: it only removes battery load /
		// discharge, and the fuse overflow guard below can still force
		// discharge if the site actually needs it.
		if (plannerSelfExportSurplusGate ||
			(manualHoldActive && math.Abs(manualHold.PowerW) < 1)) &&
			math.Abs(raw[i].TargetW) < 1 {
			raw[i].TargetW = 0
			continue
		}
		// Slew limiter is opt-out. Both inverter families ramp internally;
		// disabling the external slew lets PI's computed target reach the
		// inverter in one cycle, which the inverter then ramps at its own
		// safe rate. Saves the windup-recovery delay we observed on
		// 2026-05-25.
		if !state.SlewEnabled {
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
	return applyDispatchSafetyPipeline(raw, store, state, driverCapacities, fuseMaxW, dispatchSafetyOptions{
		manualHoldActive:  manualHoldActive,
		noSelfDischarge:   noSelfDischarge,
		updatePrevTargets: true,
		recordDispatch:    true,
	})
}

type dispatchSafetyOptions struct {
	manualHoldActive  bool
	noSelfDischarge   bool
	updatePrevTargets bool
	recordDispatch    bool
}

func applyDispatchSafetyPipeline(
	targets []DispatchTarget,
	store *telemetry.Store,
	state *State,
	driverCapacities map[string]float64,
	fuseMaxW float64,
	opts dispatchSafetyOptions,
) []DispatchTarget {
	// ---- Fuse guard (bidirectional, #145) ----
	targets = applyFuseGuard(targets, store, state, fuseMaxW)

	// ---- Plan/exec sign-mismatch floor (energy planner modes only) ----
	// Operator-report 2026-04-28 (08:00-08:15 CEST): planner_arbitrage
	// peak slot wanted battery_w = -2400 W (discharge to export at peak),
	// dispatch produced +1640..+1860 W (charged from PV surplus). PV got
	// swallowed by the battery instead of sold at 334 ore/kWh.
	//
	// Root cause was a code-path divergence elsewhere; this is the rail that
	// makes that whole class of bug a no-op:
	//
	//   plan says discharge, exec produces charge -> idle this tick
	//   plan says charge,    exec produces discharge -> idle this tick
	//
	// Only applied in the energy planner modes. Manual modes have no plan to
	// disagree with, and manual holds intentionally override planner intent.
	if !opts.manualHoldActive {
		targets = applyPlanSignFloor(targets, state)
	}
	if opts.noSelfDischarge {
		targets = floorNegativeTargets(targets)
	}

	// forceFuseDischarge runs LAST. A fuse overflow can demand a battery
	// target far beyond what slew would allow in one tick; slew-limiting that
	// response would leave the fuse violated for multiple ticks.
	targets = forceFuseDischarge(targets, store, state, driverCapacities, fuseMaxW)
	republishFuseEVCapAfterFuseDischarge(targets, store, state, fuseMaxW)
	recordDispatchTargets(targets, state, opts.updatePrevTargets, opts.recordDispatch)
	return targets
}

func republishFuseEVCapAfterFuseDischarge(targets []DispatchTarget, store *telemetry.Store, state *State, fuseMaxW float64) {
	// The joint allocator computes FuseEVMaxW assuming the battery target it
	// produced is what gets dispatched. forceFuseDischarge may flip that target
	// from charge to discharge, freeing additional fuse headroom for the EV.
	if state == nil || !state.FuseSaturated || state.EVChargingW <= 0 || fuseMaxW <= 0 {
		return
	}
	peakBindingW := fuseMaxW - state.fuseSafetyMarginW()
	peakBinding := state.PeakImportCeilingW > 0 && state.PeakImportCeilingW < peakBindingW
	if peakBinding {
		return
	}

	var currentBat, postBat float64
	seen := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		if _, ok := seen[t.Driver]; ok {
			continue
		}
		seen[t.Driver] = struct{}{}
		postBat += t.TargetW
		if r := store.Get(t.Driver, telemetry.DerBattery); r != nil {
			currentBat += r.SmoothedW
		}
	}

	ceilingW := state.effectiveImportCeilingW(fuseMaxW)
	var rawGridW float64
	if r := store.Get(state.SiteMeterDriver, telemetry.DerMeter); r != nil {
		rawGridW = r.SmoothedW
	}
	H := rawGridW - currentBat - state.EVChargingW
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

func recordDispatchTargets(targets []DispatchTarget, state *State, updatePrevTargets, recordDispatch bool) {
	if state == nil {
		return
	}
	if recordDispatch {
		now := time.Now()
		state.LastDispatch = &now
	}
	if updatePrevTargets {
		for _, t := range targets {
			state.PrevTargets[t.Driver] = t.TargetW
		}
	}
	state.LastTargets = targets
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
// protectiveCurtailLimitW returns (limit, true) when live state
// triggers the DC-link protection: SoC at or above the configured
// threshold AND PV surplus exceeds the safety margin. The limit
// shrinks PV output to live-load + margin so a sudden load step
// inside the margin lands without DC-link stress. Returns
// (0, false) when protection is disabled, off-threshold, or there
// is no PV-vs-load surplus to clamp.
func protectiveCurtailLimitW(state *State, store *telemetry.Store) (float64, bool) {
	if state == nil || store == nil || !state.DCLinkProtectionEnabled {
		return 0, false
	}
	socThresh := state.DCLinkProtectionSoCThreshold
	if socThresh <= 0 {
		socThresh = 0.80
	}
	margin := state.DCLinkProtectionMarginW
	if margin <= 0 {
		margin = 1000
	}

	// Aggregate-SoC gate: protect only when batteries can no longer
	// absorb the surplus themselves. Average across online batteries
	// weighted by capacity, mirroring how the rest of dispatch
	// reasons about the fleet.
	var sumSoCWh, totalCap float64
	for _, r := range store.ReadingsByType(telemetry.DerBattery) {
		h := store.DriverHealth(r.Driver)
		if h == nil || !h.IsOnline() || r.SoC == nil {
			continue
		}
		// Capacity must come from the per-driver map passed into
		// dispatch — but ComputePVCurtail is called without it.
		// Use a flat weight (1.0 per battery) here as the SoC-
		// average proxy; in practice the operator's batteries are
		// comparable in size, and the threshold check is coarse-
		// grained anyway (80 % vs 70 % SoC isn't a precision call).
		sumSoCWh += *r.SoC
		totalCap += 1
	}
	if totalCap == 0 {
		return 0, false
	}
	avgSoC := sumSoCWh / totalCap
	if avgSoC < socThresh {
		return 0, false
	}

	// PV-vs-load gate: only meaningful when PV is producing more
	// than the household consumes — otherwise there's no surplus
	// to curtail.
	var pvAbs float64
	for _, r := range store.ReadingsByType(telemetry.DerPV) {
		h := store.DriverHealth(r.Driver)
		if h == nil || !h.IsOnline() {
			continue
		}
		if r.SmoothedW < 0 {
			pvAbs += -r.SmoothedW
		}
	}
	loadW := siteLoadW(state, store)
	if pvAbs < loadW+2*margin {
		// Surplus already inside the safe margin — no need to engage.
		return 0, false
	}
	return loadW + margin, true
}

// siteLoadW reads the household load (W) from the site meter when
// available. Mirrors the formula main.go uses for status: load =
// gridW − battery − PV (site convention). Falls back to 0 on
// missing telemetry, which makes protectiveCurtailLimitW degrade
// safely to "don't engage" rather than to a bogus tiny limit.
func siteLoadW(state *State, store *telemetry.Store) float64 {
	if state == nil || store == nil || state.SiteMeterDriver == "" {
		return 0
	}
	mtr := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	if mtr == nil {
		return 0
	}
	gridW := mtr.SmoothedW
	var batW, pvW float64
	for _, r := range store.ReadingsByType(telemetry.DerBattery) {
		batW += r.SmoothedW
	}
	for _, r := range store.ReadingsByType(telemetry.DerPV) {
		pvW += r.SmoothedW
	}
	load := gridW - batW - pvW
	if load < 0 {
		return 0
	}
	return load
}

func ComputePVCurtail(state *State, store *telemetry.Store) []CurtailTarget {
	if state == nil {
		return nil
	}

	now := time.Now()

	// Operator-installed manual hold takes precedence over the planner
	// directive. Driver-scoped → cap only that driver. Site-aggregate
	// (Driver=="") → use LimitW as the site-wide cap and fall into the
	// same proportional allocation the planner path uses.
	hold, holdActive := state.GetPVManualHold(now)
	scopedDriver := ""

	var limit float64
	if holdActive {
		// Operator override — verbatim, no live recomputation.
		limit = hold.LimitW
		scopedDriver = hold.Driver
	} else if state.SlotDirective != nil {
		// The planner's PVLimitW is the GATING decision: > 0 means
		// curtail is economically warranted (negative export price,
		// PV exceeds planner's forecast consumption). The VALUE is a
		// stale 15-min forecast — recompute the cap live so it follows
		// load rises (self-consumption preserved when load grows mid-
		// slot) and dynamic absorption opportunities (battery SoC
		// headroom, EVs on PV charging mode). When live absorbable W
		// covers everything PV can produce, the curtail effectively
		// suppresses itself.
		if dir, ok := state.SlotDirective(now); ok {
			if dir.PVLimitW > 0 {
				if live, ok := liveCurtailLimitW(state, store); ok {
					limit = live
				} else {
					// Live state incomplete (e.g. meter offline) — fall
					// back to the planner's static cap.
					limit = dir.PVLimitW
				}
			}
		}
	}

	// DC-link protective curtail layered on top of any planner /
	// manual-hold limit. Engages independently of the planner when
	// SoC is near full + PV >> load. Honoured only when the operator
	// hasn't pinned a driver-scoped hold (which is an explicit
	// override), otherwise it composes as min(existing, protective)
	// so we never relax a stronger cap.
	if scopedDriver == "" {
		if protLimit, protActive := protectiveCurtailLimitW(state, store); protActive {
			if !holdActive && limit == 0 {
				// No other curtail source — protection is the trigger.
				limit = protLimit
			} else if protLimit < limit {
				// Existing curtail less restrictive than protection;
				// tighten to the protective value.
				limit = protLimit
			}
		}
	}

	// Decide which drivers should be curtailed this tick. A hold with
	// LimitW == 0 is still a valid cap (force PV off on the scoped
	// surface) — only release entirely when there is no active hold AND
	// the planner isn't asking for curtail.
	next := map[string]float64{}
	wantCurtail := holdActive || limit > 0
	if wantCurtail && store != nil && len(state.SupportsPVCurtail) > 0 {
		if scopedDriver != "" {
			// Driver-scoped hold: cap that one driver only, regardless
			// of its live |PV| (operator may want to force a verified-
			// off check even when the inverter is producing nothing).
			if state.SupportsPVCurtail[scopedDriver] {
				next[scopedDriver] = limit
			}
		} else {
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
				h := store.DriverHealth(r.Driver)
				if h == nil || !h.IsOnline() {
					continue
				}
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
			// If the (live) limit comfortably covers all curtail-capable
			// PV, the cap doesn't actually bind anything — skip emitting
			// any curtail target. The release path below will translate
			// previously-curtailed drivers to curtail_disable. Only
			// applies to planner-driven curtail; manual holds still go
			// out verbatim above.
			if !holdActive && total > 0 && limit >= total {
				// fall through with next stays empty.
			} else if total > 0 {
				for _, d := range drivers {
					next[d.name] = limit * (d.abs / total)
				}
			}
		}
	}

	// Release path. A previously-curtailed driver gets an explicit
	// `curtail_disable` (LimitW: 0) only when one of the following is
	// true:
	//   - the curtail directive has cleared (no slot, no manual hold)
	//   - the driver is no longer in `state.SupportsPVCurtail` (config
	//     change removed the opt-in)
	//   - the driver went offline (no harm — driver can't receive
	//     anyway, but keeps `state.LastCurtailedDrivers` clean)
	//
	// We deliberately do NOT release a driver just because it dropped
	// out of the live proportional allocation due to its own |PV|
	// crashing to ~0. That very thing often happens as a direct
	// consequence of our prior curtail (the inverter throttled PV down
	// to the cap, telemetry reports 0 generation, allocator excludes
	// it next tick). Emitting a release in that case publishes
	// `pplim arg=0` on Ferroamp's extapi — same wire bytes as the
	// release would have, opposite semantics — and locks the inverter
	// at 0 W PV until the operator clears it from the Ferroamp portal
	// (sticky-lock trap, 2026-05-27 incident; see #367 for the driver-
	// side hard-fail that paired with this dispatcher fix).
	//
	// While the directive is active and the driver is still online +
	// supported, the right behaviour is to leave the existing pplim
	// in place; the driver will get a fresh non-zero target as soon as
	// its live |PV| returns to a level the allocator can split.
	var out []CurtailTarget
	suppressedTrack := map[string]bool{}
	for d := range state.LastCurtailedDrivers {
		if _, stillActive := next[d]; stillActive {
			continue
		}
		if !wantCurtail {
			out = append(out, CurtailTarget{Driver: d, LimitW: 0})
			continue
		}
		if !state.SupportsPVCurtail[d] {
			out = append(out, CurtailTarget{Driver: d, LimitW: 0})
			continue
		}
		if store != nil {
			if h := store.DriverHealth(d); h == nil || !h.IsOnline() {
				out = append(out, CurtailTarget{Driver: d, LimitW: 0})
				continue
			}
		}
		// Online + supported + curtail still active + dropped from
		// allocation. Don't release — the inverter is presumably at
		// or near the previously-commanded cap and re-publishing 0
		// would trip the sticky-pplim trap. Keep the driver in
		// LastCurtailedDrivers so the next cycle's allocation decision
		// is taken against an accurate baseline.
		suppressedTrack[d] = true
	}
	for d, w := range next {
		// Skip vanishingly-small per-driver shares. Proportional
		// allocation can round a driver's share to ~0 when its live
		// |PV| is small relative to the site total, and a curtail
		// with power_w <= ~1 W lands at the driver as `pplim arg=0`
		// on Ferroamp — the same sticky-lock trap. Better to leave
		// the previous pplim in place for one cycle than risk it.
		if w <= curtailMinPerDriverW {
			continue
		}
		out = append(out, CurtailTarget{Driver: d, LimitW: w})
	}

	// Update LastCurtailedDrivers to include every driver that either
	// got a fresh non-zero target this tick OR was tracked through a
	// suppressed-release decision above.
	if len(out) == 0 && len(suppressedTrack) == 0 {
		state.LastCurtailedDrivers = nil
	} else {
		updated := make(map[string]bool, len(out)+len(suppressedTrack))
		for _, t := range out {
			if t.LimitW > 0 {
				updated[t.Driver] = true
			}
		}
		for d := range suppressedTrack {
			updated[d] = true
		}
		if len(updated) == 0 {
			state.LastCurtailedDrivers = nil
		} else {
			state.LastCurtailedDrivers = updated
		}
	}
	return out
}

// curtailMinPerDriverW is the lower bound on a per-driver curtail
// allocation. Anything at or below this is suppressed so we never
// publish a near-zero pplim that some inverters (Ferroamp) treat as a
// hard "limit to 0 W" sticky lock. Tuned conservatively — well below
// any realistic curtail target operators actually want.
const curtailMinPerDriverW = 1.0

// pvCurtailBatterySoCMax is the fractional SoC ceiling above which a battery
// is treated as having no curtail-absorption headroom. Below it, the
// battery's MaxChargeW (or MaxCommandW default) is added to the live
// curtail limit so PV stays uncapped while the battery can still take
// the energy. Hard-coded conservatively — the goal is to err on the
// side of preserving PV generation when there's anywhere meaningful
// to put it.
const pvCurtailBatterySoCMax = 0.99

// liveCurtailLimitW computes the cap PV may produce *right now* given
// the planner's decision that curtail is economically warranted for
// this slot. It rolls together three runtime quantities the planner
// itself doesn't see:
//
//  1. Live household load — load = grid - pv - battery (site sign).
//     If load grew beyond the planner's forecast (resistive heater,
//     midslot EV plug-in), the cap grows with it so self-consumption
//     stays the priority.
//
//  2. Battery absorption headroom — for every online battery with
//     SoC below `pvCurtailBatterySoCMax`, the per-driver MaxChargeW
//     (or MaxCommandW default) is added. PV can keep producing because
//     the dispatch loop will route the surplus into the battery.
//
//  3. EV PV-charging demand — state.EVSurplusOnlyReserveW carries the
//     aggregate W that surplus_only loadpoints are willing to take
//     from PV. Added directly.
//
// Returns (limit_w, ok). ok=false means the live state was too
// incomplete to make a decision (notably: no fresh site-meter
// reading), so the caller should fall back to the planner's static
// PVLimitW. When ok=true and the limit exceeds total live PV the
// curtail dispatch upstream skips curtail entirely (the cap doesn't
// bind anything).
func liveCurtailLimitW(state *State, store *telemetry.Store) (float64, bool) {
	if state == nil || store == nil {
		return 0, false
	}

	// Require a fresh site-meter reading. Without it we can't compute
	// live load and shouldn't be making live decisions — defer to the
	// planner's static value instead.
	var gridW float64
	if state.SiteMeterDriver == "" {
		return 0, false
	}
	if m := store.Get(state.SiteMeterDriver, telemetry.DerMeter); m != nil {
		gridW = m.RawW
	} else if m := store.Get(state.SiteMeterDriver, telemetry.DerBattery); m != nil {
		// Some site-meter drivers (e.g. ferroamp) emit grid flow on
		// the battery channel because the same driver also owns the
		// battery. Accept that as the meter reading.
		gridW = m.RawW
	} else {
		return 0, false
	}

	// Live PV (positive watts of generation).
	var pvW float64
	for _, r := range store.ReadingsByType(telemetry.DerPV) {
		h := store.DriverHealth(r.Driver)
		if h == nil || !h.IsOnline() {
			continue
		}
		if r.RawW < 0 {
			pvW += -r.RawW
		}
	}

	// Live battery aggregate (charge positive, discharge negative —
	// site sign, summed across all online batteries).
	var batW float64
	for _, r := range store.ReadingsByType(telemetry.DerBattery) {
		h := store.DriverHealth(r.Driver)
		if h == nil || !h.IsOnline() {
			continue
		}
		batW += r.RawW
	}

	// Power balance at the home node (all site-signed):
	//   load = grid - pv - battery
	// Where pv contributes generation (negative → adds to load),
	// battery contributes either as load (charging, +) or source
	// (discharging, −).
	liveLoadW := gridW - (-pvW) - batW
	if liveLoadW < 0 {
		liveLoadW = 0
	}

	// Battery headroom: for each online battery with SoC below the
	// ceiling, allow up to its per-driver MaxChargeW (falls back to
	// the global MaxCommandW default when no override is configured).
	var batHeadroomW float64
	for _, r := range store.ReadingsByType(telemetry.DerBattery) {
		h := store.DriverHealth(r.Driver)
		if h == nil || !h.IsOnline() {
			continue
		}
		if r.SoC == nil || *r.SoC >= pvCurtailBatterySoCMax {
			continue
		}
		capW := float64(MaxCommandW)
		if lim, ok := state.DriverLimits[r.Driver]; ok && lim.MaxChargeW > 0 {
			capW = lim.MaxChargeW
		}
		batHeadroomW += capW
	}

	// EV reserve: prefer the curtail-specific value (counts plugged-
	// but-stopped EVs with SoC headroom) over the dispatch reserve
	// (which excludes them on purpose). Falls back to the dispatch
	// reserve if main.go hasn't wired the new field yet.
	evReserveW := state.EVCurtailHeadroomW
	if evReserveW <= 0 {
		evReserveW = state.EVSurplusOnlyReserveW
	}
	if evReserveW < 0 {
		evReserveW = 0
	}

	return liveLoadW + batHeadroomW + evReserveW, true
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
	var currentTotal float64
	for _, b := range bats {
		currentTotal += b.currentW
	}
	desiredTotal := currentTotal + totalCorrection

	// Capability-aware reallocation. A battery that can't move in the
	// demanded direction this cycle (discharge when desiredTotal < 0, charge
	// when > 0) is excluded from the split and parked at 0; the split runs
	// over the capable subset only, so its share is absorbed by capable
	// siblings instead of leaking to the grid. No-op when every battery is
	// capable (the common case — all existing drivers report capable). When
	// NONE are capable, splitByCapacityAndPV(nil) returns nil and every
	// battery stays parked at 0 — an honest target (the residual goes to the
	// grid regardless, but we don't command a discharge no driver can honour).
	if desiredTotal != 0 {
		capable := make([]batteryInfo, 0, len(bats))
		parked := make([]DispatchTarget, 0)
		for _, b := range bats {
			blocked := b.dischargeBlocked
			if desiredTotal > 0 {
				blocked = b.chargeBlocked
			}
			if blocked {
				parked = append(parked, DispatchTarget{Driver: b.driver, TargetW: 0, Clamped: true})
			} else {
				capable = append(capable, b)
			}
		}
		if len(parked) > 0 {
			return append(splitByCapacityAndPV(capable, desiredTotal, groupPV), parked...)
		}
	}
	return splitByCapacityAndPV(bats, desiredTotal, groupPV)
}

// splitByCapacityAndPV distributes desiredTotal across bats: charging with
// PV-locality info prefers DC-local routing (#143); discharge, idle, or no
// PV locality falls back to a pure capacity-proportional split. Extracted
// from distributeProportional so the capability-aware path can run the exact
// same split over a subset of batteries.
func splitByCapacityAndPV(bats []batteryInfo, desiredTotal float64, groupPV map[string]float64) []DispatchTarget {
	var totalCap float64
	for _, b := range bats {
		totalCap += b.capacityWh
	}
	if totalCap <= 0 {
		return nil
	}

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

func anyBatteryDischarging(bats []batteryInfo) bool {
	for _, b := range bats {
		if b.currentW < -1 {
			return true
		}
	}
	return false
}

type surplusAccounting struct {
	rawGridW            float64
	effectiveGridW      float64
	currentBatteryW     float64
	evReserveRemainingW float64
	evActive            bool // EV is actually drawing current (not just plugged)
}

func newSurplusAccounting(rawGridW, effectiveGridW, currentBatteryW float64, state *State) surplusAccounting {
	return surplusAccounting{
		rawGridW:            rawGridW,
		effectiveGridW:      effectiveGridW,
		currentBatteryW:     currentBatteryW,
		evReserveRemainingW: evReserveRemainingW(state),
		evActive:            state != nil && state.EVChargingW > evActiveThresholdW,
	}
}

func evReserveRemainingW(state *State) float64 {
	if state == nil || state.EVSurplusOnlyReserveW <= 0 {
		return 0
	}
	remaining := state.EVSurplusOnlyReserveW - state.EVChargingW
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (a surplusAccounting) rawMeterWWithoutBattery() float64 {
	return a.rawGridW - a.currentBatteryW
}

func (a surplusAccounting) trueMeterExportWithoutBatteryW() float64 {
	exportW := -a.rawMeterWWithoutBattery()
	if exportW < 0 {
		return 0
	}
	return exportW
}

func (a surplusAccounting) idleChargeOnlySurplusW(thresholdW float64) float64 {
	chargeW := a.trueMeterExportWithoutBatteryW() - a.evReserveRemainingW
	if chargeW <= thresholdW {
		return 0
	}
	return chargeW
}

func (a surplusAccounting) effectiveChargeSurplusW() float64 {
	surplusW := -a.effectiveGridW + a.currentBatteryW
	if surplusW < 0 {
		return 0
	}
	return surplusW
}

// evCanUseReserve reports whether the surplus-only EV can actually consume
// the reserved export right now: either it's already drawing (evActive), or
// the available PV surplus is at least the reserved amount so the EV could
// start on it. When false (EV stopped AND surplus below its start power) the
// reserve is futile — holding it back would just export surplus the EV can't
// take — so callers release it and let the home battery absorb the surplus.
// This is what makes "surplus flows into the home battery when the EV can't
// assimilate it" work; the difference the EV DOES take is handled by the
// reserve tracking its actual draw (evReserveRemainingW shrinks as it ramps).
func (a surplusAccounting) evCanUseReserve() bool {
	if a.evReserveRemainingW <= 0 {
		return false
	}
	return a.evActive || a.effectiveChargeSurplusW() >= a.evReserveRemainingW
}

func (a surplusAccounting) chargeCeilingAfterEVReserveW() float64 {
	surplus := a.effectiveChargeSurplusW()
	if !a.evCanUseReserve() {
		return surplus // EV can't use the reserve → battery absorbs it all
	}
	ceiling := surplus - a.evReserveRemainingW
	if ceiling < 0 {
		return 0
	}
	return ceiling
}

func plannerSelfIdleDesiredTotal(surplus surplusAccounting, state *State) float64 {
	threshold := mpc.IdleGateThresholdW
	if state != nil && state.PVSurplusAbsorbThresholdW > 0 {
		threshold = state.PVSurplusAbsorbThresholdW
	}
	return surplus.idleChargeOnlySurplusW(threshold)
}

func floorNegativeTargets(targets []DispatchTarget) []DispatchTarget {
	for i := range targets {
		if targets[i].TargetW < 0 {
			targets[i].TargetW = 0
			targets[i].Clamped = true
		}
	}
	return targets
}

// coverLoadChargeSlot reports whether the current plan slot is a
// planner_arbitrage charge-from-PV-surplus slot: the DP meant to soak surplus
// (PlannedGridW below the grid-charge import band), NOT buy from the grid.
// Such a slot carries no hard charge commitment — when the forecast load is
// wrong and the site is importing, the battery should reactively discharge to
// cover it (the charge-side mirror of the discharge-slot cover-load carve-out).
//
// Three rails consult this so a legitimate cover-load discharge isn't undone:
//   - the soft cap (ComputeDispatch) lets the back-off go negative,
//   - planHasNonDischargeIntent doesn't block the reactive discharge,
//   - applyPlanSignFloor (via planSignIntent) doesn't treat it as a
//     plan/exec sign mismatch.
//
// A deliberate grid-charge slot (PlannedGridW ≥ band) is excluded: its
// realisable refill intent is preserved. Operator report 2026-05-30.
func coverLoadChargeSlot(state *State, dir SlotDirective) bool {
	const idleWhGate = 50.0         // a near-zero per-slot energy is idle, not charge
	const gridChargeImportW = 100.0 // PlannedGridW ≥ this ⇒ deliberate grid-charge
	return state != nil && state.Mode == ModePlannerArbitrage &&
		dir.HasPlannedGridW &&
		dir.BatteryEnergyWh > idleWhGate &&
		dir.PlannedGridW < gridChargeImportW
}

func planHasNonDischargeIntent(state *State) bool {
	if state == nil || !state.Mode.IsPlannerMode() {
		return false
	}
	// planner_self is pure reactive PI on grid=0 in non-idle slots —
	// the plannerSelfIdleGate ALONE decides whether to discharge. The
	// plan's per-slot charge intent (BatteryEnergyWh > 0) must NOT
	// gate discharge here: SC mode's contract is "always chase
	// grid=0" so a forecast miss that leaves the meter importing has
	// to be covered by the battery. Without this exemption, a slot
	// the planner forecast as +675 W of PV absorption would refuse to
	// discharge even when the cloud rolled in and the live meter is
	// importing 500 W — the symptom the operator hit on v0.79.5
	// before this carve-out.
	//
	// planner_passive_arbitrage was previously NOT in the carve-out to
	// protect deliberate grid-charge decisions: when the DP picks cheap
	// hours for refilling, a plan slot of "charge X Wh" is realisable
	// intent (not just a forecast that happened to land positive), and
	// overriding it with reactive discharge would undo that decision.
	// Operators who want strict "never grid-charge regardless of price"
	// should keep planner_self.
	//
	// However, that rationale only applies when the plan slot's intent
	// is to charge. When the slot is idle (battery_w ≈ 0, e.g. "export
	// the PV surplus") there is no protected charge decision — reactive
	// discharge is safe and correct. Without the carve-out for idle
	// slots, a forecast miss (PV overestimated, load underestimated)
	// leaves the site importing while batteries sit at 0 W. Found in
	// production v0.87.0: PV forecast off by 7×, plan idle, site
	// imported 648 W continuously through the slot.
	//
	// Fix: planner_passive_arbitrage now participates in the carve-out
	// for non-charge slots (BatteryEnergyWh ≤ idleWh). Charge slots
	// remain authoritative — their non-discharge block is preserved.
	if state.Mode == ModePlannerSelf {
		return false
	}
	const idleWh = 50.0
	const idleGridW = 100.0
	if state.SlotDirective != nil {
		if dir, ok := state.SlotDirective(time.Now()); ok {
			// For passive_arbitrage: only block reactive discharge when the
			// plan slot has explicit charge intent. Idle and discharge slots
			// get no non-discharge block — reactive discharge may cover load.
			if state.Mode == ModePlannerPassiveArbitrage {
				return dir.BatteryEnergyWh > idleWh
			}
			if state.Mode == ModePlannerArbitrage {
				// A charge-from-PV-surplus slot (coverLoadChargeSlot) and an
				// idle slot both carry no protected charge decision, so reactive
				// discharge may cover a forecast-missed load. Only a deliberate
				// grid-charge slot (BatteryEnergyWh > idleWh and not a PV-surplus
				// charge) keeps the non-discharge block.
				if coverLoadChargeSlot(state, dir) {
					return false
				}
				return dir.BatteryEnergyWh > idleWh
			}
			// planner_cheap (and any other planner mode): idle slots keep the
			// non-discharge block; only deliberate discharge slots are exempt.
			return dir.BatteryEnergyWh >= -idleWh
		}
	}
	if state.PlanTarget != nil {
		if modeStr, gridW, ok := state.PlanTarget(time.Now()); ok {
			switch Mode(modeStr) {
			case ModeCharge:
				return true
			case ModeSelfConsumption:
				// passive_arbitrage on a self_consumption slot: only block
				// reactive discharge when the plan's grid target is
				// import-directed (i.e. a deliberate grid-charge). Idle
				// export slots (gridW near zero or negative) are free.
				if state.Mode == ModePlannerPassiveArbitrage {
					return gridW > idleGridW
				}
				return gridW >= -idleGridW
			}
		}
	}
	return false
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
		if !ok {
			w = 1.0
		}
		totalW += w
	}
	if totalW <= 0 {
		return nil
	}
	var currentTotal float64
	for _, b := range bats {
		currentTotal += b.currentW
	}
	desiredTotal := currentTotal + totalCorrection

	out := make([]DispatchTarget, 0, len(bats))
	for _, b := range bats {
		w, ok := weights[b.driver]
		if !ok {
			w = 1.0
		}
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
	// Aggregate live battery power for the batteries this dispatch is
	// about to control so we can hold load+pv+uncontrolled-batteries
	// constant. Offline or otherwise untargeted batteries are already
	// reflected in currentGrid; subtracting them here without adding a
	// replacement target would double-remove their contribution and miss
	// fuse overages.
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
	// operator's tariff peak; export ceiling is the tighter of (fuse −
	// safety margin) and the operator's max_export_w protection limit —
	// set when an inverter trips on sustained export below the breaker
	// (recurring Ferroamp 0x8030 fault). Both share one enforcement surface.
	effImportW := state.effectiveImportCeilingW(fuseMaxW)
	effExportW := state.effectiveExportCeilingW(fuseMaxW)
	importOverage := predicted - effImportW
	exportOverage := -effExportW - predicted
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

// effectiveExportCeilingW returns the binding ceiling for grid export in
// watts: the fuse limit minus its safety margin, further capped by
// MaxExportW when the operator has opted into a site export protection
// limit (MaxExportW > 0). Mirrors effectiveImportCeilingW on the export
// side so the fuse guard scales battery discharge back below an inverter's
// sustained-export trip point, not just the physical breaker. MaxExportW
// is taken at face value (no extra safety subtraction) — the operator
// typed the protection limit; the fuse keeps its own independent margin.
func (s *State) effectiveExportCeilingW(fuseMaxW float64) float64 {
	eff := fuseMaxW - s.fuseSafetyMarginW()
	if eff < 0 {
		eff = 0
	}
	if s != nil && s.MaxExportW > 0 && s.MaxExportW < eff {
		eff = s.MaxExportW
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
// the call-site comment. Only active in planner_cheap / planner_arbitrage.
// planner_self is excluded because its non-idle slots are live-reactive
// self-consumption: plan sign must not prevent covering live import or
// absorbing live export. When the sum of post-distribute, post-clamp
// targets has the opposite sign to the active slot's plan intent, every
// target is forced to zero (idle).
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
	if len(targets) == 0 || state == nil || !state.Mode.IsPlannerMode() ||
		state.Mode == ModePlannerSelf {
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
				// A charge-from-PV-surplus slot has no hard charge commitment
				// (see coverLoadChargeSlot) — report idle intent so the sign
				// floor doesn't clamp a legitimate cover-load discharge.
				if coverLoadChargeSlot(state, dir) {
					return 0
				}
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
