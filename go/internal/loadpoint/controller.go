package loadpoint

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"
)

// Controller orchestrates one dispatch cycle for every configured
// loadpoint: observe driver telemetry, read the planner's per-slot
// energy budget, translate to an instantaneous W command, and send
// to the driver.
//
// Phase decisions (1Φ vs 3Φ) live IN THE DRIVER, not here. The
// controller's job is purely energy allocation: how many watts the
// MPC budget says we can pour into this loadpoint right now. The
// driver knows its own physical constraints (minimum amps, contactor
// switching latency, manufacturer's phaseMode wire format) and
// decides which phase configuration to use given the requested W,
// the operator's `phase_mode`/`phase_split_w`/`min_phase_hold_s`
// preferences, and the site's per-phase fuse ceiling — all of which
// are passed through in the `ev_set_current` command.
//
// Dependencies are injected as function types (not interfaces) to
// avoid pulling mpc and telemetry into loadpoint's import graph —
// mpc already imports loadpoint for its DP loadpoint_spec, so the
// cycle must go the other way. main.go wires short adapter closures
// from mpc.Service / telemetry.Store / drivers.Registry.
type Controller struct {
	manager *Manager
	plan    PlanFunc
	tel     TelemetryFunc
	send    SenderFunc

	// fuseEVMax is the joint fuse-budget allocator's verdict for how much
	// W this controller may command to the EV this tick. Set by the
	// dispatch package each control cycle; nil/zero-returning func means
	// "no fuse constraint" (loadpoint runs at its planner-determined
	// budget). When the function returns (cap, true) with cap > 0, the
	// controller clamps the planner's wantW to that cap so battery and
	// EV cooperatively share the site fuse.
	fuseEVMax func() (float64, bool)

	// siteSurplusForEVW returns the live PV surplus that this loadpoint
	// could legally claim under surplus_only — i.e. *what's left of PV
	// after house load*, regardless of what the home battery is
	// currently absorbing. The arithmetic lives in main.go because
	// it depends on per-site telemetry layout (pv driver, load driver,
	// battery drivers, site-meter driver). Returns (_, false) when any
	// of the inputs are stale; the controller then pauses rather than
	// guess, which is the conservative default for "never import".
	siteSurplusForEVW func() (float64, bool)

	// site is the grid-boundary fuse. Its values are passed through
	// to the driver in every ev_set_current cmd so the driver knows
	// the per-phase ceiling and the mains voltage. Zero MaxAmps
	// disables the per-phase fields in the cmd; the driver then
	// falls back to its own configured defaults.
	site SiteFuse

	// holds is the manual-override registry: per-loadpoint power +
	// phase parameters that win over the MPC-driven dispatch until
	// they expire. Used by the diagnostics endpoint
	// `POST /api/loadpoints/{id}/manual_hold` so an operator can pin
	// a specific amperage / phase configuration on the charger for
	// long enough to observe driver behaviour, without fighting the
	// 5-second control loop. Missing entries (or expired holds, which
	// `GetManualHold` lazily evicts) fall through to the normal
	// compute-from-plan path.
	holdMu sync.Mutex
	holds  map[string]ManualHold

	// surplusMu protects surplusWin + surplusPaused, the per-loadpoint
	// state that smooths the surplus_only pause/resume decision over
	// a small rolling window so brief PV dips don't cycle the EV
	// contactor. See computeSurplusCmd for the full rationale.
	surplusMu       sync.Mutex
	surplusWin      map[string]*surplusWindow
	surplusPaused   map[string]bool
	surplusPausedAt map[string]time.Time

	// vehicleStatus reports the matched vehicle driver + its
	// charging_state for a given loadpoint, so the controller can
	// trigger a charge_start command when the EV detached mid-
	// session ("Stopped") while we're trying to deliver power.
	// nil disables the wake feature.
	vehicleStatus func(loadpointID string) (driver, chargingState string, ok bool)

	// peakRemainingSurplusW returns the peak PV-minus-load surplus
	// expected for the rest of the local day, used by surplus_only
	// to decide whether to lock the loadpoint to 1Φ for the day.
	// When the highest forecasted surplus from now to end-of-day
	// can't sustain a 3Φ minimum, sticking to 3Φ would mean we
	// pause the EV for the rest of the day. Falling back to 1Φ
	// once is far better than flapping 1Φ ↔ 3Φ as cloud cover
	// shifts. nil disables this — the loadpoint then stays on 3Φ-
	// only forever (matching the conservative original behaviour).
	peakRemainingSurplusW func() (float64, bool)
	// nearTermPeakSurplusW returns the peak PV-minus-load surplus
	// over the next `window` from now (in plan slot resolution).
	// pickSurplusSteps consults this *before* the whole-day peak so
	// the LP can fall back to 1Φ when 3Φ isn't imminent — captures
	// "we have 3 kW now and won't see 4.1 kW for the next 30 min,
	// start charging at 1Φ instead of waiting." The day-peak gate
	// still applies for the longer-term "lock 1Φ for the whole day"
	// decision so a passing cloud doesn't commit the LP to 1Φ for
	// the entire afternoon. Optional; nil means no near-term gate.
	nearTermPeakSurplusW func(window time.Duration) (float64, bool)

	// nearTermLogLast throttles the "1Φ allowed (near-term 3Φ
	// unreachable)" log line to once per nearTermLogCooldown per
	// loadpoint so it doesn't spam every 5 s when the condition
	// holds for hours. Reset on day rollover via the existing
	// phase-lock release path.
	nearTermLogMu   sync.Mutex
	nearTermLogLast map[string]time.Time

	// fusePauseUntil holds the wall-clock time before which an LP must
	// stay paused after a fuse-clamp-induced full pause. Operator-stated
	// behaviour: ramp down if possible, pause if the fuse cap is below
	// the LP's min step, and keep paused for fusePauseCooldown so a
	// transient doesn't flap. Cleared lazily when the cooldown expires.
	fusePauseMu    sync.Mutex
	fusePauseUntil map[string]time.Time

	// phaseLockMu protects phaseLocked1P + phaseLockedAt + phaseSelected3P.
	// The 1Φ lock is sticky for the rest of the day so a slowly recovering
	// PV doesn't flip 1Φ ↔ 3Φ as clouds shift. It's automatically
	// cleared at the start of the next local day if the forecast
	// shows we'll see enough surplus to sustain 3Φ again — that's
	// the natural reset point that matches the operator's "look at
	// today vs tomorrow" mental model.
	// phaseSelected3P + phaseSelectedAt enforce the minimum-dwell rule
	// on the near-term gate: once a 3Φ-only ↔ 1Φ-allowed decision is
	// taken, hold it for at least phaseSwitchMinHold before the next
	// switch is allowed. Without this, a forecast peak hovering around
	// the 4140 W threshold flaps the step set every tick, which
	// cascades into Easee phaseMode flips + contactor cycles + battery
	// PI windup. Operator rule: at most one 1Φ↔3Φ switch per 30 min.
	// Cleared on day rollover via the same path as phaseLocked1P.
	phaseLockMu     sync.Mutex
	phaseLocked1P   map[string]bool
	phaseLockedAt   map[string]time.Time
	phaseSelected3P map[string]bool
	phaseSelectedAt map[string]time.Time

	// wakeMu protects the per-loadpoint last-wake timestamp used to
	// throttle charge_start retries. Tesla rate-limits BLE commands;
	// retrying every 5 s would just exhaust the radio.
	wakeMu        sync.Mutex
	wakeLast      map[string]time.Time
	wakeKickUntil map[string]time.Time
	wakeAttempts  map[string]int

	// batSoC reports the home battery's current state-of-charge (0..1
	// fraction) for the bat-SoC surplus-unlock feature. nil disables
	// the feature entirely — the LP behaves exactly as today.
	batSoC func() (float64, bool)

	// gridDeferredMu protects gridDeferred. The map is set by main.go's
	// MPC spec builder when it decides to suppress grid-funded EV
	// planning (because the deadline lies past the last published price
	// slot). Runtime dispatch reads it to ALSO enforce surplus-only
	// behaviour at the tick — so when forecast PV undershoots reality
	// the EV pauses rather than silently importing from grid against a
	// plan budget that assumed sun.
	gridDeferredMu sync.Mutex
	gridDeferred   map[string]bool

	// batSoCArmed tracks the per-LP arm/release state for the bat-SoC
	// hysteresis. batSoCNoPV counts consecutive ticks where the live
	// site surplus dropped to zero — a sustained no-PV run releases
	// the arm even when the home battery is still above the SoC
	// threshold, because battery discharge isn't surplus and we don't
	// want to ride a high-SoC arm through the night kicking the EV.
	batSoCArmedMu sync.Mutex
	batSoCArmed   map[string]bool
	batSoCNoPV    map[string]int
}

// batSoCPVGoneTicks is the consecutive-tick threshold (at the 5 s
// dispatch cadence ≈ 30 s) of zero/negative live surplus before the
// bat-SoC unlock disarms. Long enough to swallow a passing cloud
// without flap; short enough that evening transition releases promptly.
const batSoCPVGoneTicks = 6

// wakeKickDuration is how long a wake-kick forces the EV charger to
// signal min 3Φ current after a charge_start fires. The wallbox must
// actively present current (not 0 A) for the car to negotiate the new
// session — sending charge_start while Easee is at 0 A is futile.
// This briefly violates surplus_only's no-import rule, which is the
// price of recovering from a detached session without operator
// intervention. 30 s is enough for Tesla's BLE handshake plus a few
// seconds of pilot-signal stabilisation.
const wakeKickDuration = 30 * time.Second

// wakeBackoffAfter is the number of consecutive failed wake attempts
// (vehicle stays detached across the cooldown window) before we
// stretch the cooldown out. The car's BLE radio gets rate-limited
// after several rapid sends; pressing every 90 s indefinitely just
// hammers it without effect. Counter resets on the first
// `Charging` / `Starting` reading.
const wakeBackoffAfter = 5

// wakeBackoffCooldown is the stretched cooldown applied once
// wakeBackoffAfter is reached. Big enough that an operator who is
// ignoring the notification doesn't get hammered every 90 s; short
// enough that recovery is automatic if the car wakes on its own
// (e.g. user presses "Start" on the Tesla app).
const wakeBackoffCooldown = 10 * time.Minute

// vehicleWakeCooldown caps how often we'll send a charge_start to the
// same loadpoint's matched vehicle. Tesla's BLE radio is shared with
// every other proxy poll the driver does (vehicle_data, charge_amps,
// wake_up); poking it every 90 s on top of routine 60 s polls quickly
// pushes it into "Command Disallowed" rate-limits, after which all
// proxy reads start failing and the picker sees stale data. 5 min
// gives the radio room to breathe between active wake attempts —
// the wallbox-cycle fired on each wake is what actually rescues
// detached sessions; charge_start is the secondary signal.
const vehicleWakeCooldown = 5 * time.Minute

// vehicleWakeTimeout caps the wake-send roundtrip when wakeVehicleAuto
// is invoked fire-and-forget on a background goroutine (e.g. from the
// wallbox-delivering rising edge in tickOne). Without it a stuck
// vehicle-proxy HTTP call would leak the goroutine until the process
// exits. 30 s is comfortably longer than the Tesla proxy's own ~15 s
// host timeout while still bounded enough that a leaked routine per
// cooldown window is the worst case.
const vehicleWakeTimeout = 30 * time.Second

// surplusWindowSize is the length of the rolling-average buffer used
// for surplus_only pause/resume decisions. At a 5 s tick this is ~20 s
// of smoothing — long enough to ride out single-tick cloud transients
// without committing to a stale view of the world.
const surplusWindowSize = 4

// surplusResumeMarginW is added to the 3Φ minimum step before we will
// resume a paused surplus_only loadpoint. Prevents oscillation right
// at the threshold (snap_to_min ↔ pause).
const surplusResumeMarginW = 200.0

// surplusMinPauseHold is the minimum dwell time once a surplus_only
// loadpoint has been paused. Easee documents ~30 s minimum on/off
// for the contactor; this floor keeps us comfortably above it even
// if the rolling-avg crosses the resume threshold quickly. The
// rolling-avg already smooths transients; this is a hard contactor-
// protection backstop on top.
const surplusMinPauseHold = 35 * time.Second

// nearTermLogCooldown caps the rate of the "1Φ allowed (near-term 3Φ
// unreachable)" log line so a long morning with sustained low surplus
// produces one line per 10 min per LP instead of one per 5 s tick.
const nearTermLogCooldown = 10 * time.Minute

// phaseSwitchMinHold is the minimum dwell between 1Φ↔3Φ switches on
// the near-term gate. Operator rule: at most one switch per 30 min.
// The Easee contactor is rated for limited switching cycles; on a
// borderline-PV day a frequent 1Φ↔3Φ flip would burn through them
// quickly and inject load-step transients into the battery PI loop
// every couple of minutes. 30 min lets the forecast machinery commit
// to a verdict before the next reconsider.
const phaseSwitchMinHold = 30 * time.Minute

// fusePauseCooldown is how long an LP stays at 0 W after a fuse-over-
// limit force-pause. Long enough that whatever house load caused the
// overload (oven, EV from another LP, sauna…) has time to clear or for
// the operator to notice; short enough that a transient inrush doesn't
// stop charging for the rest of the day. 5 min is the operator-stated
// preference; chosen vs. e.g. 1 min because the typical "did the oven
// kick on?" disturbance lasts longer than a minute.
const fusePauseCooldown = 5 * time.Minute

// defaultPhaseSplitW mirrors loadpoint.Config.PhaseSplitW's default —
// 3680 W is a 16 A 1Φ ceiling at 230 V. Kept in sync with the comment
// on Config.PhaseSplitW.
const defaultPhaseSplitW = 3680.0

// surplusWindow is a fixed-size ring buffer of recent surplus samples
// for one loadpoint. Average is computed over the live samples (n may
// be < surplusWindowSize during the first few ticks of a session).
type surplusWindow struct {
	buf  [surplusWindowSize]float64
	n    int
	head int
}

func (w *surplusWindow) push(v float64) float64 {
	w.buf[w.head] = v
	w.head = (w.head + 1) % surplusWindowSize
	if w.n < surplusWindowSize {
		w.n++
	}
	var sum float64
	for i := 0; i < w.n; i++ {
		sum += w.buf[i]
	}
	return sum / float64(w.n)
}

// ManualHold pins a loadpoint to a specific dispatch payload until
// ExpiresAt. PowerW is sent verbatim; PhaseMode / PhaseSplitW /
// MinPhaseHoldS / Voltage / MaxAmpsPerPhase override the loadpoint's
// configured defaults — but ONLY when explicitly set on the hold.
// Zero values mean "no override" and the controller falls back to
// the loadpoint's PhaseMode/PhaseSplitW/MinPhaseHoldS and the wired
// SiteFuse for voltage / max_amps_per_phase / site_phases. This
// preserves the per-phase fuse clamp on minimal holds (e.g. just
// `{power_w, hold_s}`) — without the fall-through, the driver would
// silently fall back to its 230 V × 16 A defaults, which on a
// non-standard site could exceed the actual fuse.
type ManualHold struct {
	PowerW          float64
	PhaseMode       string
	PhaseSplitW     float64
	MinPhaseHoldS   int
	Voltage         float64
	MaxAmpsPerPhase float64
	SitePhases      int
	ExpiresAt       time.Time
}

// Directive is the loadpoint-relevant slice of mpc.SlotDirective.
// The mpc package defines the full type with BatteryEnergyWh etc;
// the controller only needs the slot window and per-loadpoint Wh
// budget, so we don't pull in the whole struct.
type Directive struct {
	SlotStart         time.Time
	SlotEnd           time.Time
	LoadpointEnergyWh map[string]float64
}

// EVSample is the loadpoint-relevant slice of telemetry.DerReading
// for a DerEV entry — power, cumulative session energy, plug state.
// Chargers like Easee don't expose the vehicle's BMS SoC, so the
// controller only sees these four fields.
//
// RequestActive is true when the vehicle is (or could imminently be)
// drawing current. Drivers that can distinguish "throttled to 0" from
// "the car has explicitly refused" set this to false on the latter
// (e.g. CTEK NCRQ state). Drivers without that signal leave it true.
// The loadpoint manager uses it to detect car-side completion via the
// NCRQCompletionThreshold timer.
type EVSample struct {
	PowerW        float64
	SessionWh     float64
	Connected     bool
	RequestActive bool
}

// PlanFunc returns the current-slot directive for now, or (_, false)
// when no plan is available (stale, missing, out of horizon).
type PlanFunc func(now time.Time) (Directive, bool)

// TelemetryFunc returns the latest EV reading for a driver. The
// second return is false when the driver hasn't produced a reading
// yet.
type TelemetryFunc func(driver string) (EVSample, bool)

// SenderFunc forwards a JSON command payload to a driver. Matches
// drivers.Registry.Send.
type SenderFunc func(ctx context.Context, driver string, payload []byte) error

// NewController wires the dependencies. Passing nil for plan, tel,
// or send disables the corresponding step — useful in tests.
func NewController(mgr *Manager, plan PlanFunc, tel TelemetryFunc, send SenderFunc) *Controller {
	return &Controller{manager: mgr, plan: plan, tel: tel, send: send}
}

// SetFuseEVMax wires the joint allocator's verdict from control.State.
// Called once at startup from main.go. The returned (cap_w, true) is
// honored as a hard upper bound on this tick's EV command; (_, false)
// means no constraint. Pass nil to disable.
func (c *Controller) SetFuseEVMax(f func() (float64, bool)) {
	if c == nil {
		return
	}
	c.fuseEVMax = f
}

// SetSiteSurplusForEV wires a per-tick "PV surplus available to the
// EV" reader for the surplus_only clamp. The function returns total
// W the EV could safely claim without forcing site import — typically
// `(-pvW - houseLoadW)` since that's PV-minus-load regardless of how
// the home battery is currently splitting it. Called once at startup
// from main.go. Pass nil to disable, in which case surplus_only is
// enforced only by the MPC plan (no live clamp).
func (c *Controller) SetSiteSurplusForEV(f func() (float64, bool)) {
	if c == nil {
		return
	}
	c.siteSurplusForEVW = f
}

// SetVehicleStatus wires the matched-vehicle reader used by the auto-
// wake path. The function takes a loadpoint id and returns the
// matched vehicle driver name + its current `charging_state` (one of
// `Charging` / `Starting` / `Stopped` / `Disconnected` / `Complete`),
// or (_, _, false) if no online vehicle is paired to the loadpoint.
// Called once at startup from main.go. Pass nil to disable auto-
// wake, in which case the operator must manually start charging
// from the Tesla app after a session detach.
func (c *Controller) SetVehicleStatus(f func(loadpointID string) (driver, chargingState string, ok bool)) {
	if c == nil {
		return
	}
	c.vehicleStatus = f
}

// SetPeakRemainingSurplusW wires the forecast-based "best surplus
// we'll see for the rest of the day" reader used by surplus_only's
// 1Φ-lock decision. Typical implementation in main.go iterates the
// MPC plan's remaining slots until local end-of-day and returns the
// max(−pvW − loadW). Pass nil to disable the 1Φ fallback — the
// loadpoint then stays 3Φ-only and pauses on low-PV days, which is
// the conservative original behaviour.
func (c *Controller) SetPeakRemainingSurplusW(f func() (float64, bool)) {
	if c == nil {
		return
	}
	c.peakRemainingSurplusW = f
}

// applyFuseClampAndCooldown enforces the joint fuse allocator's
// FuseEVMax cap on a requested wattage and tracks the operator-stated
// pause-cooldown semantics:
//
//   - In cooldown: force 0 regardless of wantW.
//   - cap unset / cap >= wantW: pass-through.
//   - cap < wantW but a snap-step exists at ≤ cap: ramp down to that step.
//   - cap < min step (or snap returns 0): force 0 AND arm
//     fusePauseUntil[lpID] = now + fusePauseCooldown.
//
// Both tickOne branches (manual hold + normal MPC dispatch) call this
// just before writing cmd["power_w"] so the protection is uniform:
// neither a sticky operator hold nor a stale MPC budget can drive
// the fuse over limit, and the cooldown prevents fuse flap when a
// transient overload clears momentarily.
func (c *Controller) applyFuseClampAndCooldown(now time.Time, lpCfg Config, wantW float64) float64 {
	if c == nil {
		return wantW
	}
	c.fusePauseMu.Lock()
	until, has := c.fusePauseUntil[lpCfg.ID]
	if has && !now.Before(until) {
		// Cooldown elapsed; release lazily so subsequent ticks see no
		// hold and can re-attempt charging.
		delete(c.fusePauseUntil, lpCfg.ID)
		has = false
	}
	c.fusePauseMu.Unlock()
	if has {
		return 0
	}
	if c.fuseEVMax == nil {
		return wantW
	}
	cap, ok := c.fuseEVMax()
	if !ok || cap < 0 {
		return wantW
	}
	if wantW <= cap {
		return wantW
	}
	// Need to ramp down. Snap to the largest allowed step ≤ cap.
	snapped := SnapChargeW(cap, lpCfg.MinChargeW, lpCfg.MaxChargeW, lpCfg.AllowedStepsW)
	if snapped > 0 && snapped >= lpCfg.MinChargeW {
		slog.Info("loadpoint fuse-clamp: ramped down",
			"lp", lpCfg.ID, "want_w", wantW, "fuse_cap_w", cap, "snapped_w", snapped)
		return snapped
	}
	// Cap is below the LP's min step → pause + arm cooldown.
	c.fusePauseMu.Lock()
	if c.fusePauseUntil == nil {
		c.fusePauseUntil = map[string]time.Time{}
	}
	c.fusePauseUntil[lpCfg.ID] = now.Add(fusePauseCooldown)
	c.fusePauseMu.Unlock()
	slog.Warn("loadpoint fuse-clamp: paused for cooldown",
		"lp", lpCfg.ID, "want_w", wantW, "fuse_cap_w", cap,
		"min_step_w", lpCfg.MinChargeW,
		"cooldown_s", int(fusePauseCooldown.Seconds()))
	return 0
}

// SetNearTermPeakSurplusW wires the short-horizon "peak surplus over
// the next window" reader used by pickSurplusSteps to decide whether
// a 3Φ start is imminent. Typical implementation iterates the MPC
// plan's slots starting now and walks until `now + window`, returning
// the max(−pvW − loadW) seen. Pass nil to keep the original "wait for
// today's day-peak forecast" behaviour.
func (c *Controller) SetNearTermPeakSurplusW(f func(window time.Duration) (float64, bool)) {
	if c == nil {
		return
	}
	c.nearTermPeakSurplusW = f
}

// SetBatSoCProvider wires the home-battery SoC reader used by the
// bat-SoC surplus-unlock feature. The function returns the current
// SoC as a 0..1 fraction. (_, false) disables the feature for this
// tick; nil disables it permanently. Called once at startup from
// main.go.
func (c *Controller) SetBatSoCProvider(f func() (float64, bool)) {
	if c == nil {
		return
	}
	c.batSoC = f
}

// evalBatSoCArm decides whether the bat-SoC surplus unlock is armed
// for the given loadpoint this tick. Arms when SoC is at/above the
// threshold AND there's live PV to grab (battery discharge alone is
// not surplus — that's just self-consumption or arbitrage the planner
// is already orchestrating). Releases when SoC drops below
// (threshold − BatSoCUnlockHystPp), or after a sustained run of
// zero/negative live surplus (batSoCPVGoneTicks).
//
// Returns false when no threshold is configured or the bat_soc reader
// is missing.
func (c *Controller) evalBatSoCArm(lpID string, threshold float64) bool {
	if c == nil || c.batSoC == nil || threshold <= 0 {
		return false
	}
	// Read the live inputs (bat SoC + site surplus) OUTSIDE the arm
	// mutex — siteSurplusForEVW is a closure wired in main.go that
	// itself calls back into AnyLoadpointSurplusActive, which needs
	// to acquire batSoCArmedMu. Calling it under that lock would
	// recursively self-deadlock the dispatch loop (debugged: every
	// tickOne hung on the first LP after we shipped this feature).
	// Order: gather facts → take lock → mutate the small state map.
	soc, socOK := c.batSoC()
	pvGone := true
	if c.siteSurplusForEVW != nil {
		if s, ok := c.siteSurplusForEVW(); ok && s > 0 {
			pvGone = false
		}
	}

	c.batSoCArmedMu.Lock()
	defer c.batSoCArmedMu.Unlock()
	if c.batSoCArmed == nil {
		c.batSoCArmed = map[string]bool{}
		c.batSoCNoPV = map[string]int{}
	}
	prev := c.batSoCArmed[lpID]
	if !socOK {
		// Stale telemetry: don't change the arm state. A momentary
		// blip shouldn't release the unlock during peak surplus.
		return prev
	}
	if pvGone {
		c.batSoCNoPV[lpID]++
	} else {
		c.batSoCNoPV[lpID] = 0
	}
	socPct := soc * 100
	armed := prev
	switch {
	case socPct < threshold-BatSoCUnlockHystPp:
		armed = false
	case socPct >= threshold && !pvGone:
		armed = true
	case c.batSoCNoPV[lpID] >= batSoCPVGoneTicks:
		// SoC may still be high but PV has been gone long enough that
		// staying armed would just trickle from battery/grid via the
		// surplus path's auto-wake. Release.
		armed = false
	}
	c.batSoCArmed[lpID] = armed
	return armed
}

// SetGridDeferred records that MPC has suppressed grid-funded EV
// planning for this LP (because the deadline lies past published
// prices). Surplus dispatch semantics apply at runtime too: the EV's
// commanded W is snapped to live surplus only, with no grid import,
// regardless of what the cached MPC plan budget says. Cleared when
// MPC's next replan finds prices for the deadline window. Safe to
// call concurrently with the dispatch tick.
func (c *Controller) SetGridDeferred(lpID string, deferred bool) {
	if c == nil {
		return
	}
	c.gridDeferredMu.Lock()
	defer c.gridDeferredMu.Unlock()
	if c.gridDeferred == nil {
		c.gridDeferred = map[string]bool{}
	}
	if deferred {
		c.gridDeferred[lpID] = true
	} else {
		delete(c.gridDeferred, lpID)
	}
}

// gridDeferredFor reads the per-LP deferral flag set by main.go's MPC
// spec builder. Read-only accessor used inside surplusActive.
func (c *Controller) gridDeferredFor(lpID string) bool {
	if c == nil {
		return false
	}
	c.gridDeferredMu.Lock()
	defer c.gridDeferredMu.Unlock()
	return c.gridDeferred[lpID]
}

// surplusActive reports whether surplus-only dispatch semantics apply
// to this loadpoint right now. True when ANY of:
//   - the operator's configured SurplusOnly flag is on
//   - MPC has deferred grid-funded planning (forecast-vs-real divergence
//     guard: even if the cached plan said "charge 2 kW now", live PV
//     might have collapsed since the last replan)
//   - the bat-SoC unlock is armed for this LP
//
// The caller passes the loadpoint's schedule so we read the threshold
// without re-locking the Manager.
func (c *Controller) surplusActive(lpCfg Config, sched Schedule) bool {
	if lpCfg.SurplusOnly {
		return true
	}
	if c.gridDeferredFor(lpCfg.ID) {
		return true
	}
	return c.evalBatSoCArm(lpCfg.ID, sched.SurplusUnlockBatSoCPct)
}

// AnyLoadpointSurplusActive reports whether any configured loadpoint
// is currently treating PV surplus as priority — via the configured
// SurplusOnly flag, the MPC grid-deferral flag, or a runtime-armed
// bat-SoC unlock. main.go's siteSurplusForEVW reader uses this to
// zero out the home-battery's PV-charge contribution from the EV's
// apparent surplus, which prevents the EV from stealing PV that the
// planner already routed to the home battery (the flap-avoidance rule).
//
// Safe to call before Tick has ever run — returns false in that case.
func (c *Controller) AnyLoadpointSurplusActive() bool {
	if c == nil || c.manager == nil {
		return false
	}
	for _, cfg := range c.manager.Configs() {
		if cfg.SurplusOnly {
			return true
		}
		if c.gridDeferredFor(cfg.ID) {
			return true
		}
		sched, _ := c.manager.GetSchedule(cfg.ID)
		if sched.SurplusUnlockBatSoCPct > 0 {
			c.batSoCArmedMu.Lock()
			armed := c.batSoCArmed[cfg.ID]
			c.batSoCArmedMu.Unlock()
			if armed {
				return true
			}
		}
	}
	return false
}

// RefreshVehicle sends a one-off wake command to the vehicle driver
// bound to the given loadpoint, bypassing the auto-wake cooldown.
// Used by the API when the operator edits the schedule — wakes
// Tesla / BMW / whichever vehicle driver is bound so the next poll
// surfaces any vehicle-side limit / SoC / connection changes
// immediately rather than waiting up to the next natural wake
// window.
//
// Sends the generic `wake_up` action (cross-driver protocol — any
// vehicle driver implements it against its own back-end). Distinct
// from `charge_start` (still used by the auto-wake loop when it's
// trying to convince a detached car to actually start drawing
// current) — `wake_up` is purely a telemetry refresh, no charge
// side effects. Returns nil if no vehicle driver is bound or the
// controller isn't fully wired (no-op). Errors from the send hop
// are returned for the caller to surface to the operator.
func (c *Controller) RefreshVehicle(ctx context.Context, lpID string) error {
	if c == nil || c.vehicleStatus == nil || c.send == nil {
		return nil
	}
	driver, _, ok := c.vehicleStatus(lpID)
	if !ok || driver == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"action": "wake_up"})
	if err != nil {
		return err
	}
	// Reset the auto-wake throttle so a manual refresh doesn't leave
	// the LP in a long backoff afterwards — the operator just told us
	// they want a fresh read, no reason to apply 90s cooldown to the
	// next legitimate auto-wake.
	c.wakeMu.Lock()
	if c.wakeLast == nil {
		c.wakeLast = map[string]time.Time{}
		c.wakeKickUntil = map[string]time.Time{}
		c.wakeAttempts = map[string]int{}
	}
	delete(c.wakeAttempts, lpID)
	c.wakeLast[lpID] = time.Now()
	c.wakeMu.Unlock()
	slog.Info("loadpoint manual wake (schedule edit)", "lp", lpID, "vehicle_driver", driver)
	return c.send(ctx, driver, payload)
}

// ForceStart outcome sentinels. The API layer maps each to a distinct
// HTTP status: NotReady → 503, LoadpointNotFound → 404 "lp", NoVehicle
// → 422 "no vehicle bound", any other error → 502 "driver send".
// Distinguishing these is what the previous (string, error) two-value
// return tried to encode via empty strings — sentinels make it
// type-checked.
var (
	ErrForceStartNotReady       = errors.New("loadpoint controller not ready (missing vehicleStatus/send wiring)")
	ErrForceStartLoadpointGone  = errors.New("loadpoint not found")
	ErrForceStartNoVehicleBound = errors.New("no vehicle driver bound to loadpoint")
)

// ForceStartVehicle sends a generic `charge_start` to the loadpoint's
// bound vehicle driver immediately, bypassing the auto-wake's
// `vehicleWakeCooldown` + `wakeBackoffCooldown` throttle. Used by the
// operator-driven "force start" API path when the auto-wake has
// backed off and the operator wants to break the backoff (typical case:
// car was unresponsive earlier, has since become reachable, the 10-min
// stretched cooldown still has minutes to run, operator wants charging
// to resume now).
//
// Side effects on success (after the send returns nil), in order:
//  1. Reset the wake-attempt counter so subsequent auto-wakes start
//     from a fresh cooldown.
//  2. Bump wakeLast so the auto-wake loop will not duplicate this
//     send on the same tick.
//  3. Arm the wake-kick window so the next few dispatch ticks force
//     the wallbox to signal at least min current — without it a
//     successful charge_start lands on a 0 A wallbox and the car has
//     nothing to negotiate with.
//  4. Send the generic `charge_start` action (cross-driver protocol —
//     Tesla, BMW, Audi drivers all implement it against their own
//     back-ends).
//
// If the send fails, the throttle resets and wake-kick arming still
// stand — that's intentional: the operator's intent was to break the
// backoff, and a failed send shouldn't punish a subsequent retry.
//
// Returns the driver name actually targeted plus the outcome:
//
//	("", ErrForceStartNotReady)        — controller missing wiring
//	("", ErrForceStartLoadpointGone)   — no such loadpoint id
//	("", ErrForceStartNoVehicleBound)  — loadpoint exists, no vehicle
//	(driver, sendErr)                  — send hop returned an error
//	(driver, nil)                      — sent
func (c *Controller) ForceStartVehicle(ctx context.Context, lpID string) (string, error) {
	if c == nil || c.vehicleStatus == nil || c.send == nil {
		return "", ErrForceStartNotReady
	}
	if c.manager != nil {
		if _, ok := c.manager.State(lpID); !ok {
			return "", ErrForceStartLoadpointGone
		}
	}
	driver, _, ok := c.vehicleStatus(lpID)
	if !ok || driver == "" {
		return "", ErrForceStartNoVehicleBound
	}
	now := time.Now()
	c.wakeMu.Lock()
	if c.wakeLast == nil {
		c.wakeLast = map[string]time.Time{}
		c.wakeKickUntil = map[string]time.Time{}
		c.wakeAttempts = map[string]int{}
	}
	delete(c.wakeAttempts, lpID)
	c.wakeLast[lpID] = now
	c.wakeKickUntil[lpID] = now.Add(wakeKickDuration)
	c.wakeMu.Unlock()
	payload, err := json.Marshal(map[string]any{"action": "charge_start"})
	if err != nil {
		slog.Warn("loadpoint force-start: payload marshal", "lp", lpID, "err", err)
		return driver, err
	}
	sendErr := c.send(ctx, driver, payload)
	if sendErr != nil {
		slog.Warn("loadpoint force-start (operator) — send failed",
			"lp", lpID, "vehicle_driver", driver, "err", sendErr)
	} else {
		slog.Info("loadpoint force-start (operator) — sent",
			"lp", lpID, "vehicle_driver", driver)
	}
	return driver, sendErr
}

// wakeVehicleAuto sends a `wake_up` to the loadpoint's bound vehicle
// driver, gated by the same `vehicleWakeCooldown` (5 min) the
// charge_start auto-wake loop uses. Used for event-triggered wakes
// (e.g. the wallbox-delivering rising edge) where we want fresh
// vehicle state but the trigger can flap — don't storm the BLE radio.
// No-op when no vehicle is bound; logs but does not return errors
// (the trigger is opportunistic; failure is fine).
//
// Callers commonly invoke this as `go wakeVehicleAuto(...)` with a
// background context, so the send is bounded internally by
// `vehicleWakeTimeout` to keep a stuck HTTP roundtrip from leaking the
// goroutine. The driver's own HTTP client also has a timeout — this is
// just a belt-and-braces ceiling at the caller boundary.
func (c *Controller) wakeVehicleAuto(ctx context.Context, lpID string, reason string) {
	if c == nil || c.vehicleStatus == nil || c.send == nil {
		return
	}
	driver, _, ok := c.vehicleStatus(lpID)
	if !ok || driver == "" {
		return
	}
	now := time.Now()
	c.wakeMu.Lock()
	if c.wakeLast == nil {
		c.wakeLast = map[string]time.Time{}
		c.wakeKickUntil = map[string]time.Time{}
		c.wakeAttempts = map[string]int{}
	}
	if last, has := c.wakeLast[lpID]; has && now.Sub(last) < vehicleWakeCooldown {
		c.wakeMu.Unlock()
		return
	}
	c.wakeLast[lpID] = now
	c.wakeMu.Unlock()
	payload, err := json.Marshal(map[string]any{"action": "wake_up"})
	if err != nil {
		return
	}
	slog.Info("loadpoint auto-wake (vehicle telemetry refresh)",
		"lp", lpID, "vehicle_driver", driver, "reason", reason)
	sendCtx, cancel := context.WithTimeout(ctx, vehicleWakeTimeout)
	defer cancel()
	if err := c.send(sendCtx, driver, payload); err != nil {
		slog.Warn("loadpoint auto-wake send failed",
			"lp", lpID, "vehicle_driver", driver, "err", err)
	}
}

// IsBatSoCArmed reports whether the bat-SoC surplus unlock is
// currently armed for the given loadpoint. Surfaced so main.go can
// thread this runtime state into the MPC LoadpointSpec.SurplusOnly
// — without it, MPC plans battery→EV transfers freely while the
// dispatch layer's bat-SoC arming refuses to execute them, producing
// misleading "battery discharges to feed EV" entries in the plan UI
// that never actually happen.
//
// Returns false if the controller is nil, no arm map yet exists, or
// the LP id isn't tracked. Safe to call concurrently with Tick.
func (c *Controller) IsBatSoCArmed(lpID string) bool {
	if c == nil {
		return false
	}
	c.batSoCArmedMu.Lock()
	defer c.batSoCArmedMu.Unlock()
	if c.batSoCArmed == nil {
		return false
	}
	return c.batSoCArmed[lpID]
}

// SetSiteFuse installs the grid-boundary fuse so the controller can
// pass voltage + per-phase amperage to drivers in every command.
// Called once at startup from main.go after config load. A zero-value
// fuse causes the controller to omit those fields, which leaves the
// driver to use its own defaults.
func (c *Controller) SetSiteFuse(f SiteFuse) {
	if c == nil {
		return
	}
	c.site = f
}

// SetManualHold pins the given loadpoint to a fixed dispatch payload
// until h.ExpiresAt. tickOne checks the hold on every cycle and emits
// the held values verbatim — bypassing the MPC budget translation —
// until the hold expires (then the controller resumes normal
// dispatch on the next cycle). Useful for diagnostics: hold a
// specific amperage on the charger long enough to observe driver
// behaviour without fighting the 5-second control tick.
//
// A zero ExpiresAt clears any hold for this loadpoint (same as
// ClearManualHold). Setting a hold for an unknown loadpoint ID is
// silently allowed — the hold has no effect because tickOne only
// runs for configured loadpoints.
func (c *Controller) SetManualHold(id string, h ManualHold) {
	if c == nil {
		return
	}
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	if c.holds == nil {
		c.holds = map[string]ManualHold{}
	}
	if h.ExpiresAt.IsZero() {
		delete(c.holds, id)
		return
	}
	c.holds[id] = h
}

// ClearManualHold removes any active hold for the given loadpoint,
// regardless of expiry. Idempotent.
func (c *Controller) ClearManualHold(id string) {
	if c == nil {
		return
	}
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	delete(c.holds, id)
}

// GetManualHold returns the current hold for a loadpoint. The bool
// is false when no hold is active. Expired holds are not returned —
// they're lazily evicted on the next read.
func (c *Controller) GetManualHold(id string, now time.Time) (ManualHold, bool) {
	if c == nil {
		return ManualHold{}, false
	}
	c.holdMu.Lock()
	defer c.holdMu.Unlock()
	h, ok := c.holds[id]
	if !ok {
		return ManualHold{}, false
	}
	if !now.Before(h.ExpiresAt) {
		delete(c.holds, id)
		return ManualHold{}, false
	}
	return h, true
}

// Tick runs one dispatch cycle for every configured loadpoint.
// Safe to call even when no loadpoints are configured. Idempotent —
// calling it twice in the same moment produces the same commands.
//
// Behaviour:
//
//  1. Read latest charger telemetry for this driver.
//  2. Feed the observation to the Manager (plug state, session Wh,
//     inferred SoC).
//  3. For unplugged loadpoints: skip command entirely.
//  4. For plugged loadpoints: ask the plan for this slot's Wh
//     allocation and translate to a W command via the energy-
//     allocation contract (remaining_wh × 3600 / remaining_s).
//  5. Send `ev_set_current` with that W plus the operator's phase
//     preferences and the site's fuse parameters; the driver picks
//     phases and converts W→A given that it knows the voltage.
func (c *Controller) Tick(ctx context.Context, now time.Time) {
	if c == nil || c.manager == nil {
		return
	}
	if c.plan == nil {
		return
	}
	for _, lpCfg := range c.manager.Configs() {
		c.tickOne(ctx, now, lpCfg)
	}
}

func (c *Controller) tickOne(ctx context.Context, now time.Time, lpCfg Config) {
	var sample EVSample
	if c.tel != nil {
		sample, _ = c.tel(lpCfg.DriverName)
	}
	// Resolve the schedule once per tick — used for bat-SoC unlock
	// (surplusActive) below. Zero value when no schedule is set,
	// which makes evalBatSoCArm a no-op.
	var sched Schedule
	if c.manager != nil {
		sched, _ = c.manager.GetSchedule(lpCfg.ID)
	}
	surplusOn := c.surplusActive(lpCfg, sched)
	// Detect the disconnected→connected edge (state.PluggedIn flips
	// from false to true) so we can reset session-scoped state
	// before the new session's first dispatch tick. Without this
	// the rolling-avg buffer keeps stale samples from the previous
	// session, biasing the first ~20 s of pause/resume decisions.
	// Also detect the not-delivering→delivering edge separately so
	// we can wake the bound vehicle for fresh telemetry the moment
	// the wallbox actually starts pushing current — the planner
	// otherwise has to wait for the next periodic vehicle poll
	// (or the proxy's cache to refresh on its own) to learn the
	// car's current charge_limit_pct + SoC, which costs accuracy
	// on the first ~15 min of a new charging session.
	wasPlugged := false
	wasDelivering := false
	if st, ok := c.manager.State(lpCfg.ID); ok {
		wasPlugged = st.PluggedIn
		wasDelivering = st.CurrentPowerW >= DeliveringW
	}
	c.manager.Observe(lpCfg.ID, sample.Connected, sample.PowerW, sample.SessionWh, sample.RequestActive)
	if !sample.Connected {
		c.resetSurplusSession(lpCfg.ID)
		return
	}
	if !wasPlugged {
		c.resetSurplusSession(lpCfg.ID)
	}
	// Wallbox just started delivering current: fire a wake at the
	// bound vehicle so the next vehicle-driver poll comes back with
	// fresh charging_state / charge_limit_pct / SoC. Gated by
	// vehicleWakeCooldown inside wakeVehicleAuto so a flapping
	// pause/resume cycle can't storm the BLE radio. Fire-and-forget
	// on a background goroutine so the dispatch tick isn't blocked
	// by the wake HTTP roundtrip.
	if sample.PowerW >= DeliveringW && !wasDelivering {
		go c.wakeVehicleAuto(context.Background(), lpCfg.ID, "delivering_edge")
	}

	cmd := map[string]any{"action": "ev_set_current"}
	if hold, ok := c.GetManualHold(lpCfg.ID, now); ok {
		// Manual override active — skip MPC translation. The hold's
		// non-zero fields override the loadpoint config + site fuse;
		// zero/empty fields fall through to the normal defaults so a
		// minimal hold (just `power_w`) still carries the per-phase
		// fuse clamp inputs the driver needs to stay safe.
		holdW := hold.PowerW
		// Surplus-only is a hard promise even against a diagnostic
		// hold: if the operator left surplus_only on while pinning a
		// manual amperage, we still refuse to import grid for the EV.
		// The hold's other fields (phase mode, hold time) are still
		// honoured — only the W setpoint is clamped, and we log it so
		// the operator notices the conflict.
		if surplusOn && holdW > 0 {
			clamped := c.computeSurplusCmd(now, lpCfg, holdW, sample.PowerW)
			if clamped < holdW {
				slog.Warn("loadpoint manual hold clamped by surplus_only",
					"lp", lpCfg.ID, "hold_w", holdW, "clamped_w", clamped)
				holdW = clamped
			}
		}
		// Fuse protection: even an operator-pinned hold must not bust
		// the fuse. Apply the joint allocator's FuseEVMax cap and the
		// pause-cooldown guard before sending. A sticky 11 kW Start
		// hold + house drawing 7 A on one phase = fuse trip without
		// this clamp.
		holdW = c.applyFuseClampAndCooldown(now, lpCfg, holdW)
		cmd["power_w"] = holdW
		// Phase mode: explicit hold > explicit LP config > surplus
		// default ("auto") > driver default. Same surplus-active fallback
		// as the non-hold branch so a sticky Start hold on a 1380 W
		// surplus slot actually delivers instead of dying at the Easee
		// driver's unset → "3p" interpretation.
		switch {
		case hold.PhaseMode != "":
			cmd["phase_mode"] = hold.PhaseMode
		case lpCfg.PhaseMode != "":
			cmd["phase_mode"] = lpCfg.PhaseMode
		case surplusOn:
			cmd["phase_mode"] = "auto"
		}
		switch {
		case hold.PhaseSplitW > 0:
			cmd["phase_split_w"] = hold.PhaseSplitW
		case lpCfg.PhaseSplitW > 0:
			cmd["phase_split_w"] = lpCfg.PhaseSplitW
		}
		switch {
		case hold.MinPhaseHoldS > 0:
			cmd["min_phase_hold_s"] = hold.MinPhaseHoldS
		case lpCfg.MinPhaseHoldS > 0:
			cmd["min_phase_hold_s"] = lpCfg.MinPhaseHoldS
		}
		switch {
		case hold.Voltage > 0:
			cmd["voltage"] = hold.Voltage
		case c.site.Voltage > 0:
			cmd["voltage"] = c.site.Voltage
		}
		switch {
		case hold.MaxAmpsPerPhase > 0:
			cmd["max_amps_per_phase"] = hold.MaxAmpsPerPhase
		case c.site.MaxAmps > 0:
			cmd["max_amps_per_phase"] = c.site.MaxAmps
		}
		switch {
		case hold.SitePhases > 0:
			cmd["site_phases"] = hold.SitePhases
		case c.site.MaxAmps > 0:
			cmd["site_phases"] = c.site.Phases()
		}
	} else {
		cmdW, planReady := c.computeCommand(now, lpCfg, sample.PowerW)
		if !planReady {
			// No plan budget for this loadpoint right now — explicit
			// 0 W standdown so the charger pauses cleanly.
			cmdW = 0
		}
		// Surplus-only live clamp: regardless of what the MPC slot
		// budget said for this 15-minute window, the EV must not
		// import grid right now. We smooth the pause/resume decision
		// across the last `surplusWindowSize` ticks (≈ 20 s) so a
		// single cloud transient doesn't cycle the contactor, and we
		// snap the setpoint to 3Φ-eligible steps so a brief deficit
		// doesn't drop the charger to 1Φ (a phase swap is far more
		// wear-inducing than holding 3Φ at a slightly lower current).
		// Any short-term gap between the smoothed setpoint and live
		// PV is naturally absorbed by the home battery via the
		// reactive self_consumption PI in dispatch.go — that's the
		// "battery smooths PV transients for ~1-2 min" path.
		//
		// Opportunistic start: when surplus_only is on but the MPC
		// has no plan budget for this LP (cmdW = 0) — typical when
		// the LP isn't in the planner's view because the vehicle
		// driver is offline / has no SoC, or the user has no
		// deadline set — fall through to the surplus clamp anyway
		// with the LP's MaxChargeW as the requested ceiling. The
		// clamp returns 0 if there's no PV surplus, or a snapped
		// step otherwise. Without this, surplus_only LPs without an
		// active vehicle telemetry source (Easee + no Tesla, or
		// any third-party EV without a Go-side vehicle driver)
		// would never start because (a) MPC won't allocate without
		// a target, (b) auto-wake requires vehicleStatus, (c) the
		// surplus clamp was previously gated on cmdW > 0. The
		// wallbox will silently report what the EV does (op_mode
		// stays at 2 if the car declines) without grid import.
		if surplusOn {
			wantW := cmdW
			if wantW <= 0 {
				wantW = lpCfg.MaxChargeW
			}
			cmdW = c.computeSurplusCmd(now, lpCfg, wantW, sample.PowerW)
		}
		// Wake-kick AFTER the surplus clamp: when an auto-wake just
		// fired and the surplus clamp paused us to 0, force the
		// wallbox to signal at least min 3Φ current for a few
		// seconds so the car-side negotiation has something to land
		// on. This is the only thing that's empirically observed to
		// rescue a detached Tesla without operator intervention. The
		// kick window is bounded by wakeKickDuration; outside it the
		// normal surplus clamp resumes. Brief grid import here is the
		// price of recovering from a detached session.
		if c.wakeKickActive(lpCfg.ID, now) {
			// Honour the surplus_only phase lock: when we've fallen
			// back to 1Φ for the day, the kick should use the 1Φ
			// minimum (1380 W) rather than 3Φ (4140 W). pickSurplusSteps
			// already returns the right step set for the current
			// lock state.
			minKick := smallestNonZero(c.pickSurplusSteps(now, lpCfg))
			if minKick > 0 && cmdW < minKick {
				slog.Info("loadpoint wake-kick", "lp", lpCfg.ID,
					"prev_cmd_w", cmdW, "kick_w", minKick)
				cmdW = minKick
			}
		}
		// Fuse protection: applied LAST (after MPC budget, surplus
		// clamp, wake-kick) so all upstream sources see their nominal
		// wantW; only the actual ceiling we send to the wallbox is
		// reduced when the fuse demands it. Partial ramp-downs are
		// immediate; only "cap below min step → must pause" arms the
		// 5-min cooldown.
		cmdW = c.applyFuseClampAndCooldown(now, lpCfg, cmdW)
		cmd["power_w"] = cmdW
		// Pass operator's phase preferences through verbatim. The driver
		// reads these and decides 1Φ vs 3Φ based on its own knowledge of
		// charger min/max amps, phase-switch latency, and the requested W.
		// Override to "1p" when surplus_only locked the loadpoint to 1Φ
		// for the day — without this, Easee (and similar) ignore the
		// low-power request and stay on 3Φ at zero amps because their
		// pick_phases respects an operator-set "3p" lock over the
		// dispatch's wantW. The 1Φ lock IS the operator's intent for
		// this day.
		//
		// Default to "auto" when surplus is active and the operator
		// didn't explicitly pick a phase mode. The Easee driver
		// (drivers/easee_cloud.lua:105) interprets an UNSET phase_mode
		// as "3p" — silently locking the wallbox to 3Φ and rejecting
		// any 1Φ-eligible step the near-term branch passes through.
		// Operators who picked surplus_only have clearly opted into
		// "react to PV"; dynamic phase switching is the only way to
		// actually deliver low-power surplus to the EV.
		phaseMode := lpCfg.PhaseMode
		if c.surplusLockedTo1P(lpCfg.ID) {
			phaseMode = "1p"
		} else if surplusOn && (phaseMode == "" || phaseMode == "auto") {
			// Honour the 30-min near-term dwell decision ONLY when the
			// operator hasn't explicitly pinned a phase. Explicit "1p"
			// or "3p" is a fixed-install contract: a 1Φ-only home or a
			// 3Φ-only contactor preference. Overriding it via dwell
			// would let a near-term forecast flip the install's
			// physical configuration, exactly the wear case the dwell
			// was meant to avoid.
			//
			// When phase_mode is unset or "auto", pin the driver to
			// the dwell decision across the dwell window so a transient
			// cmd_w=0 (pause) doesn't make the driver's "auto" snap to
			// 1Φ and flip the contactor. Without this,
			// pickSurplusSteps' step-list lock is silently bypassed
			// every time the surplus clamp pauses.
			if dwell := c.dwellSelectedPhaseMode(lpCfg.ID); dwell != "" {
				phaseMode = dwell
			} else if phaseMode == "" {
				phaseMode = "auto"
			}
		}
		if phaseMode != "" {
			cmd["phase_mode"] = phaseMode
		}
		if lpCfg.PhaseSplitW > 0 {
			cmd["phase_split_w"] = lpCfg.PhaseSplitW
		}
		if lpCfg.MinPhaseHoldS > 0 {
			cmd["min_phase_hold_s"] = lpCfg.MinPhaseHoldS
		}
		// Pass the site fuse so the driver can compute the per-phase
		// ceiling using the actual mains voltage instead of hard-coding
		// 230 V × 16 A. Drivers that don't support phase switching can
		// safely ignore these fields.
		if c.site.MaxAmps > 0 {
			cmd["max_amps_per_phase"] = c.site.MaxAmps
			cmd["site_phases"] = c.site.Phases()
		}
		if v := c.site.Voltage; v > 0 {
			cmd["voltage"] = v
		}
	}

	payload, err := json.Marshal(cmd)
	if err != nil {
		return
	}
	if c.send == nil {
		return
	}
	if err := c.send(ctx, lpCfg.DriverName, payload); err != nil {
		slog.Warn("loadpoint dispatch", "lp", lpCfg.ID,
			"driver", lpCfg.DriverName, "err", err)
	}

	// Auto-wake: if the matched vehicle reports `Stopped` /
	// `Disconnected` / `Complete` — i.e. it detached mid-session and
	// won't draw — send a generic `charge_start` action to the
	// matched vehicle driver. Driver-agnostic: the controller doesn't
	// know which vehicle protocol is behind it, only that whichever
	// driver published the latest DerVehicle reading should accept
	// this action. Today only `tesla_vehicle.lua` implements it (via
	// TeslaBLEProxy → BLE charge_start); future drivers (BMW, Audi,
	// Polestar, etc.) inherit auto-wake by implementing the same
	// action in their own `driver_command`. Throttled by
	// vehicleWakeCooldown so a flapping radio doesn't get rate-
	// limited. Fires under two conditions:
	//
	//   1. We just commanded power_w > 0 (normal post-restart wake).
	//   2. Loadpoint is surplus_only — even if cmd power_w is 0
	//      because the surplus clamp paused us. This breaks the
	//      chicken-and-egg of "can't see surplus without EV
	//      drawing, can't command power without surplus" by
	//      periodically poking the car so it negotiates with Easee.
	//      The 90 s cooldown caps Tesla BLE wear; if the car is
	//      genuinely asleep at night the proxy returns 503 and we
	//      back off.
	// Forced-wake belongs to the configured surplus_only contract — the
	// "kick the EV periodically so the car negotiates with the wallbox
	// even when the surplus clamp paused us" behaviour. The bat-SoC
	// unlock is opportunistic and tick-level; it must not poke a sleeping
	// car at night just because the bat is full. Pass the configured flag,
	// not the runtime-armed one.
	c.maybeWakeVehicle(ctx, now, lpCfg, lpCfg.SurplusOnly, cmd)
}

func (c *Controller) maybeWakeVehicle(ctx context.Context, now time.Time, lpCfg Config, surplusOn bool, cmd map[string]any) {
	lpID := lpCfg.ID
	if c == nil || c.vehicleStatus == nil || c.send == nil {
		return
	}
	pw, _ := cmd["power_w"].(float64)
	wantWake := pw > 0
	if surplusOn {
		wantWake = true
	}
	if !wantWake {
		return
	}
	driver, state, ok := c.vehicleStatus(lpID)
	if !ok || driver == "" {
		return
	}
	// "Complete" is intentionally NOT in the wake set: the car says
	// charging is done because it reached its OWN charge_limit.
	// Trying to wake it would mean fighting the user's in-app
	// limit, which they often set lower than our target_soc_pct
	// (e.g. limit 60% to preserve battery health while we plan to
	// 100%). Treat Complete as "session intentionally finished" and
	// reset the failure counter so a real detach later starts
	// fresh.
	switch state {
	case "Charging", "Starting", "Complete":
		c.wakeMu.Lock()
		delete(c.wakeAttempts, lpID)
		c.wakeMu.Unlock()
		return
	case "Stopped", "Disconnected":
	default:
		return
	}
	c.wakeMu.Lock()
	if c.wakeLast == nil {
		c.wakeLast = map[string]time.Time{}
		c.wakeKickUntil = map[string]time.Time{}
		c.wakeAttempts = map[string]int{}
	}
	last := c.wakeLast[lpID]
	cooldown := vehicleWakeCooldown
	attempts := c.wakeAttempts[lpID]
	if attempts >= wakeBackoffAfter {
		cooldown = wakeBackoffCooldown
	}
	if !last.IsZero() && now.Sub(last) < cooldown {
		c.wakeMu.Unlock()
		return
	}
	c.wakeLast[lpID] = now
	c.wakeAttempts[lpID] = attempts + 1
	// Also arm the wake-kick window so the next few dispatch ticks
	// force the wallbox to signal current — without it the BLE
	// charge_start lands on a 0 A wallbox and the car has nothing
	// to negotiate with.
	c.wakeKickUntil[lpID] = now.Add(wakeKickDuration)
	stretched := attempts+1 == wakeBackoffAfter
	c.wakeMu.Unlock()
	if stretched {
		slog.Warn("loadpoint auto-wake giving up on fast retries",
			"lp", lpID, "vehicle_driver", driver,
			"attempts", attempts+1,
			"next_attempt_in", wakeBackoffCooldown,
			"hint", "vehicle won't accept charge_start — needs manual wake from the operator's car app or a plug-cycle")
	}

	payload, err := json.Marshal(map[string]any{"action": "charge_start"})
	if err != nil {
		return
	}
	slog.Info("loadpoint auto-wake", "lp", lpID, "vehicle_driver", driver,
		"vehicle_state", state, "cmd_w", pw)
	if err := c.send(ctx, driver, payload); err != nil {
		slog.Warn("loadpoint auto-wake failed", "lp", lpID,
			"vehicle_driver", driver, "err", err)
	}

	// Wallbox session-cycle: when Tesla is in a "Stopped" state that
	// rejects software charge_start ("requested" rejection from the
	// car's own state machine), the only way to break out is to make
	// the wallbox open and re-close its contactor — which Tesla
	// interprets as a plug-cycle and accepts as a fresh session
	// boundary. ev_pause + brief delay + ev_resume on the EV charger
	// driver does exactly this. Driver-agnostic: any EV charger
	// driver implementing the standard ev_pause / ev_resume actions
	// gets this for free.
	//
	// Runs in a goroutine so the dispatch tick doesn't block on the
	// pause→sleep→resume sequence (~3 s).
	if lpCfg.DriverName != "" && c.send != nil {
		go func(driverName string) {
			pauseCmd, _ := json.Marshal(map[string]any{"action": "ev_pause"})
			resumeCmd, _ := json.Marshal(map[string]any{"action": "ev_resume"})
			if err := c.send(context.Background(), driverName, pauseCmd); err != nil {
				slog.Warn("loadpoint wallbox-cycle pause failed",
					"lp", lpID, "driver", driverName, "err", err)
				return
			}
			slog.Info("loadpoint wallbox-cycle: paused", "lp", lpID, "driver", driverName)
			// 3 s is enough for Tesla to register the contactor
			// open as a plug-cycle. Shorter risks the car missing
			// the transition; longer eats into the wake-kick
			// window and prolongs the grid-import.
			time.Sleep(3 * time.Second)
			if err := c.send(context.Background(), driverName, resumeCmd); err != nil {
				slog.Warn("loadpoint wallbox-cycle resume failed",
					"lp", lpID, "driver", driverName, "err", err)
				return
			}
			slog.Info("loadpoint wallbox-cycle: resumed", "lp", lpID, "driver", driverName)
		}(lpCfg.DriverName)
	}
}

// computeSurplusCmd applies the surplus_only live clamp to the
// planner-derived wantW, with rolling-average pause/resume hysteresis
// and a 3Φ-only step floor (when phase mode allows). Returns the
// adjusted command in W; 0 means "pause this tick".
//
// Inputs:
//   - lpCfg.SurplusOnly is assumed true by the caller
//   - wantW is the planner's setpoint after the fuse + planner clamps
//   - currentEvW is the EV's live draw (site sign, +)
//
// Behaviour:
//
//  1. Read the live site grid power. No reading → 0 (conservative —
//     we promised surplus_only and can't verify).
//  2. Compute instant surplus = currentEvW + max(0, -gridW). This is
//     the W we could deliver to the EV without crossing into import.
//  3. Push the instant surplus into the rolling window; the average
//     drives pause/resume. Pause only when avg drops below the 3Φ
//     minimum step; resume only when avg ≥ that minimum + a margin
//     (so we don't oscillate at the boundary).
//  4. When not paused, snap the lower of (planner wantW, INSTANT surplus)
//     to a 3Φ-eligible step. Pause/resume uses the rolling avg (so we
//     don't cycle the contactor on transients) but the magnitude
//     tracks instant — using avg for magnitude lags reality on a
//     dropping cloud front and the difference leaks straight into
//     grid import. The home battery's reactive PI in self_consumption
//     fills sub-tick gaps. See the long-form rationale immediately
//     above the `target := wantW` block in the function body.
//
// `now` is the dispatch tick's time, threaded from Tick → tickOne so the
// pause/resume timestamps stay consistent with the rest of the cycle and
// tests can drive it deterministically with a fixed clock.
//
// TODO(multi-loadpoint): siteSurplusForEVW is currently a site-wide PV-
// minus-house surplus, not a per-loadpoint allowance. With a single EV
// loadpoint that's correct (the closure already nets the EV's own draw
// out). With two or more EV loadpoints active concurrently, each
// controller tick will clamp to the same full-site surplus and they
// collectively over-allocate, breaking the never-import promise. Fix
// when a second loadpoint actually exists: switch to a site-level
// allocator that hands each loadpoint a remaining-budget reading.
func (c *Controller) computeSurplusCmd(now time.Time, lpCfg Config, wantW, currentEvW float64) float64 {
	if c == nil {
		return wantW
	}
	if c.siteSurplusForEVW == nil {
		// No live surplus reader wired (test paths). Fall back to
		// instant clamp.
		return wantW
	}
	surplusW, ok := c.siteSurplusForEVW()
	if !ok {
		// Live reading missing or stale — pause rather than risk grid import.
		return 0
	}
	// NaN/Inf guards: bad telemetry must not poison the rolling buffer
	// (the comparisons in the pause/resume hysteresis would all
	// evaluate false against NaN, silently disabling the surplus
	// clamp). Treat any non-finite reading as "no surplus".
	if math.IsNaN(surplusW) || math.IsInf(surplusW, 0) {
		return 0
	}
	// surplusW is the EV-available PV surplus, computed by main.go's
	// closure as `−gridW + batW + evW`. By the site convention's
	// identity (`loadW = gridW − batW − pvW − evW`) this equals
	// `−pvW − loadW`, i.e. PV-magnitude minus house load — invariant
	// under whatever the home battery is currently doing with the
	// surplus. The EV's own draw is part of that closure already, so
	// we don't add currentEvW here.
	instant := surplusW
	if instant < 0 {
		instant = 0
	}
	avg := c.recordSurplus(lpCfg.ID, instant)

	// Pick the step set: 3Φ-only by default, but fall back to all
	// allowed steps (which lets the driver hand the wallbox a 1Φ-
	// eligible amperage) when the forecast says we won't see enough
	// surplus to sustain 3Φ for the rest of the day. The lock is
	// sticky: once we've gone 1Φ for the session we stay 1Φ to
	// avoid cycling the contactor across the phase-mode boundary
	// each time clouds shift.
	steps := c.pickSurplusSteps(now, lpCfg)
	minStep := smallestNonZero(steps)

	// Compatibility variables for the rest of the function — the
	// pause/resume hysteresis and wake-kick reuse these names.
	steps3 := steps
	minStep3 := minStep

	paused, pausedAt := c.getSurplusPause(lpCfg.ID)
	if paused {
		// Hold a paused contactor for at least surplusMinPauseHold
		// (Easee min on/off is ~30 s) before considering resume,
		// regardless of how fast the rolling avg recovers.
		held := now.Sub(pausedAt) >= surplusMinPauseHold
		if held && avg >= minStep3+surplusResumeMarginW {
			paused = false
		}
	} else {
		if minStep3 > 0 && avg < minStep3 {
			paused = true
			pausedAt = now
		}
	}
	c.setSurplusPause(lpCfg.ID, paused, pausedAt)

	if paused {
		return 0
	}
	// Setpoint tracks INSTANT surplus, not the rolling average. The
	// avg smooths the pause/resume decision so we don't cycle the
	// contactor on transients (that's the user-stated intent: "avg
	// over 4 ticks to determine pause"), but using avg for the
	// setpoint magnitude lags reality — on a slowly dropping cloud
	// front the EV would hold its previous draw for ~20 s while
	// live PV had already fallen below it, and the difference
	// leaks straight into grid import. Tracking instant keeps the
	// no-import promise tight; the home battery's reactive PI in
	// self_consumption fills sub-tick gaps.
	target := wantW
	if instant < target {
		target = instant
	}
	return SnapChargeW(target, lpCfg.MinChargeW, lpCfg.MaxChargeW, steps3)
}

// pickSurplusSteps returns the step set surplus_only should snap to
// for this loadpoint. Default is 3Φ-eligible only (the no-flap rule);
// when the day's peak forecast surplus can't sustain a 3Φ minimum,
// we fall back to all allowed steps and STICK there for the session
// — re-upgrading would just cycle the contactor when clouds shift.
//
// `now` is the dispatch tick's time, threaded down from Tick →
// computeSurplusCmd / wake-kick so day-rollover unlock + lock-set
// timestamps share the same clock as the rest of the cycle and tests
// can drive it deterministically.
func (c *Controller) pickSurplusSteps(now time.Time, lpCfg Config) []float64 {
	if c == nil {
		return surplus3PhaseSteps(lpCfg)
	}
	steps3 := surplus3PhaseSteps(lpCfg)
	minStep3 := smallestNonZero(steps3)

	c.phaseLockMu.Lock()
	locked := c.phaseLocked1P[lpCfg.ID]
	lockedAt := c.phaseLockedAt[lpCfg.ID]
	// Day rollover: if a 1Φ lock was set on a previous local day
	// AND the new day's forecast shows enough surplus to sustain
	// 3Φ, clear the lock. This is the operator's "fresh start each
	// morning" expectation — we re-evaluate on day boundaries
	// rather than punishing today's bad weather forever.
	if locked && minStep3 > 0 && c.peakRemainingSurplusW != nil &&
		!sameLocalDay(lockedAt, now) {
		if peak, ok := c.peakRemainingSurplusW(); ok && peak >= minStep3 {
			delete(c.phaseLocked1P, lpCfg.ID)
			delete(c.phaseLockedAt, lpCfg.ID)
			delete(c.phaseSelected3P, lpCfg.ID)
			delete(c.phaseSelectedAt, lpCfg.ID)
			locked = false
			c.phaseLockMu.Unlock()
			slog.Info("loadpoint surplus_only unlocked: new day with sufficient PV forecast",
				"lp", lpCfg.ID, "peak_remaining_surplus_w", peak, "min_3p_step_w", minStep3)
			return steps3
		}
	}
	c.phaseLockMu.Unlock()

	if locked {
		// All allowed steps — driver picks the phase.
		return lpCfg.AllowedStepsW
	}
	if minStep3 <= 0 {
		return steps3
	}

	// Live-surplus override: when live PV surplus right now already
	// covers the 3Φ minimum AND there's no prior phase decision today
	// (first session of the day or first start after a day rollover),
	// pick 3Φ-only immediately. Forecast-based gating below is meant
	// to handle the "cloudy day" case where today's PV will never
	// reach 4 kW; it should NOT make us start in 1Φ when the sun is
	// right here and the operator just plugged in.
	//
	// Gated on "no prior decision" so we don't flap mid-session: once
	// we've committed to a phase, the dwell-hold + forecast logic
	// downstream keep us there for the session.
	if minStep3 > 0 && c.siteSurplusForEVW != nil {
		c.phaseLockMu.Lock()
		_, hasPrev := c.phaseSelected3P[lpCfg.ID]
		c.phaseLockMu.Unlock()
		if !hasPrev {
			if liveSurplus, ok := c.siteSurplusForEVW(); ok &&
				!math.IsNaN(liveSurplus) && !math.IsInf(liveSurplus, 0) &&
				liveSurplus >= minStep3 {
				c.phaseLockMu.Lock()
				if c.phaseSelected3P == nil {
					c.phaseSelected3P = map[string]bool{}
					c.phaseSelectedAt = map[string]time.Time{}
				}
				c.phaseSelected3P[lpCfg.ID] = true
				c.phaseSelectedAt[lpCfg.ID] = now
				c.phaseLockMu.Unlock()
				slog.Info("loadpoint surplus_only: 3Φ at session start (live surplus override)",
					"lp", lpCfg.ID, "live_surplus_w", liveSurplus, "min_3p_step_w", minStep3)
				return steps3
			}
		}
	}

	// Near-term gate: even if today's whole-day peak forecast will
	// reach 3Φ minimum eventually, if the next 30 min won't, return
	// 1Φ-allowed steps NOW so the LP captures the surplus that's
	// here today instead of waiting for a peak that's hours away.
	// Day-lock NOT set here — this is a "transient cloud" not a
	// "low-PV day" verdict, so a later 3Φ window during the same
	// day can still trigger a contactor cycle into 3Φ.
	//
	// Minimum-dwell guard: a forecast peak hovering around the 4140 W
	// threshold would otherwise flap the step set every tick, which
	// cascades into Easee phaseMode flips + contactor cycles + battery
	// PI windup. Operator rule: at most one 1Φ↔3Φ switch per
	// phaseSwitchMinHold. The prior decision is held until the dwell
	// elapses; on day rollover the selection is cleared so a fresh
	// morning gets a fresh forecast verdict.
	if c.nearTermPeakSurplusW != nil {
		const nearTermWindow = 30 * time.Minute
		nearPeak, peakOK := c.nearTermPeakSurplusW(nearTermWindow)

		c.phaseLockMu.Lock()
		prevSelected3P, hasPrev := c.phaseSelected3P[lpCfg.ID]
		prevAt := c.phaseSelectedAt[lpCfg.ID]
		if hasPrev && !sameLocalDay(prevAt, now) {
			delete(c.phaseSelected3P, lpCfg.ID)
			delete(c.phaseSelectedAt, lpCfg.ID)
			hasPrev = false
		}
		var selected3P bool
		var recordDecision bool
		switch {
		case !peakOK && !hasPrev:
			// No forecast yet and no prior decision: conservative
			// fall-through to the whole-day branch below.
			c.phaseLockMu.Unlock()
			goto afterNearTerm
		case !peakOK:
			// No forecast this tick — honour the prior decision.
			selected3P = prevSelected3P
		case !hasPrev:
			// First decision today — go with the forecast verdict.
			selected3P = nearPeak >= minStep3
			recordDecision = true
		case now.Sub(prevAt) < phaseSwitchMinHold:
			// Dwell window not yet elapsed — hold the prior decision.
			selected3P = prevSelected3P
		default:
			// Dwell elapsed — re-decide from the forecast.
			selected3P = nearPeak >= minStep3
			recordDecision = selected3P != prevSelected3P
		}
		if recordDecision {
			if c.phaseSelected3P == nil {
				c.phaseSelected3P = map[string]bool{}
				c.phaseSelectedAt = map[string]time.Time{}
			}
			c.phaseSelected3P[lpCfg.ID] = selected3P
			c.phaseSelectedAt[lpCfg.ID] = now
		}
		c.phaseLockMu.Unlock()

		if !selected3P {
			c.nearTermLogMu.Lock()
			lastFor, has := c.nearTermLogLast[lpCfg.ID]
			fireLog := !has || now.Sub(lastFor) > nearTermLogCooldown
			if fireLog {
				if c.nearTermLogLast == nil {
					c.nearTermLogLast = map[string]time.Time{}
				}
				c.nearTermLogLast[lpCfg.ID] = now
			}
			c.nearTermLogMu.Unlock()
			if fireLog {
				slog.Info("loadpoint surplus: 1Φ steps allowed (near-term 3Φ unreachable)",
					"lp", lpCfg.ID, "near_term_peak_w", nearPeak, "min_3p_step_w", minStep3,
					"window", nearTermWindow.String(), "dwell_hold", phaseSwitchMinHold.String())
			}
			return lpCfg.AllowedStepsW
		}
	}
afterNearTerm:

	if c.peakRemainingSurplusW == nil {
		return steps3
	}
	peak, ok := c.peakRemainingSurplusW()
	if !ok {
		return steps3
	}
	if peak >= minStep3 {
		return steps3
	}
	// The day-long 1Φ lock is a commitment that belongs to *configured*
	// surplus_only operators: they've opted into "no grid import for this
	// LP", and a low-PV day means trickle-charge instead of pausing.
	// The bat-SoC unlock is opportunistic and tick-level — it should not
	// inherit a day-long phase lock just because it was armed once. Skip
	// the lock-set when SurplusOnly isn't actually configured; return all
	// allowed steps so the driver can pick whichever phase the live
	// surplus suits this tick.
	if !lpCfg.SurplusOnly {
		return lpCfg.AllowedStepsW
	}
	// Lock to 1Φ for the rest of the day.
	c.phaseLockMu.Lock()
	if c.phaseLocked1P == nil {
		c.phaseLocked1P = map[string]bool{}
		c.phaseLockedAt = map[string]time.Time{}
	}
	c.phaseLocked1P[lpCfg.ID] = true
	c.phaseLockedAt[lpCfg.ID] = now
	delete(c.phaseSelected3P, lpCfg.ID)
	delete(c.phaseSelectedAt, lpCfg.ID)
	c.phaseLockMu.Unlock()
	slog.Info("loadpoint surplus_only locked to 1Φ for the day",
		"lp", lpCfg.ID, "peak_remaining_surplus_w", peak, "min_3p_step_w", minStep3)
	return lpCfg.AllowedStepsW
}

// surplusLockedTo1P reports whether the surplus_only 1Φ lock is
// currently active for the given loadpoint. Read-only accessor.
func (c *Controller) surplusLockedTo1P(id string) bool {
	if c == nil {
		return false
	}
	c.phaseLockMu.Lock()
	defer c.phaseLockMu.Unlock()
	return c.phaseLocked1P[id]
}

// dwellSelectedPhaseMode returns the phase_mode string ("1p"/"3p")
// implied by the near-term dwell selection for this loadpoint, or the
// empty string when no dwell decision is on file (first-tick, fresh
// day, no near-term peak source). Used by the dispatch command builder
// to override the operator's "auto" so the driver doesn't auto-flip
// phase on a transient cmd_w=0 (pause), which would defeat the
// 30-min minimum-dwell guarantee.
func (c *Controller) dwellSelectedPhaseMode(id string) string {
	if c == nil {
		return ""
	}
	c.phaseLockMu.Lock()
	defer c.phaseLockMu.Unlock()
	sel, ok := c.phaseSelected3P[id]
	if !ok {
		return ""
	}
	if sel {
		return "3p"
	}
	return "1p"
}

// sameLocalDay reports whether two time.Time values fall on the
// same calendar day in the local timezone. Used by the 1Φ phase
// lock to decide when to re-evaluate against the new day's forecast.
func sameLocalDay(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	la := a.Local()
	lb := b.Local()
	return la.Year() == lb.Year() && la.Month() == lb.Month() && la.Day() == lb.Day()
}

// surplus3PhaseSteps returns AllowedStepsW filtered down to entries
// at or above the loadpoint's PhaseSplitW (default 3680 W) — i.e. the
// steps the driver will deliver on 3Φ. 0 is always included so
// "pause" is still a representable command.
//
// When PhaseMode is "1p" we don't filter — the operator explicitly
// locked the install to 1Φ, so a 3Φ-only set would just wedge the
// loadpoint at 0.
func surplus3PhaseSteps(lpCfg Config) []float64 {
	if lpCfg.PhaseMode == "1p" {
		return lpCfg.AllowedStepsW
	}
	split := lpCfg.PhaseSplitW
	if split <= 0 {
		split = defaultPhaseSplitW
	}
	out := []float64{0}
	for _, s := range lpCfg.AllowedStepsW {
		if s == 0 {
			continue
		}
		if s >= split {
			out = append(out, s)
		}
	}
	return out
}

func smallestNonZero(steps []float64) float64 {
	var min float64
	for _, s := range steps {
		if s <= 0 {
			continue
		}
		if min == 0 || s < min {
			min = s
		}
	}
	return min
}

// wakeKickActive reports whether the wake-kick window for this
// loadpoint is currently in force.
func (c *Controller) wakeKickActive(id string, now time.Time) bool {
	if c == nil {
		return false
	}
	c.wakeMu.Lock()
	defer c.wakeMu.Unlock()
	t, ok := c.wakeKickUntil[id]
	return ok && now.Before(t)
}

// IsWakeKickActive is the public accessor mirroring wakeKickActive.
// Main wires it into the per-tick reserve calc so the home battery
// doesn't grab a freed PV surplus during the gap between the wake-kick
// commanding the wallbox to offer current and the EV actually starting
// to draw — the wake-kick window is the operator-correct "ramp" period
// where the EV's share of surplus should be held even when its
// instantaneous CurrentPowerW is still 0.
func (c *Controller) IsWakeKickActive(id string, now time.Time) bool {
	return c.wakeKickActive(id, now)
}

// resetSurplusSession drops the per-loadpoint rolling buffer + paused
// flag. Called on a plug-in edge (or unplug) so a new charging session
// starts with a clean view of surplus rather than inheriting the
// previous session's last samples — important when the car was
// unplugged for hours and the cached buffer is meaningless.
func (c *Controller) resetSurplusSession(id string) {
	c.surplusMu.Lock()
	delete(c.surplusWin, id)
	delete(c.surplusPaused, id)
	delete(c.surplusPausedAt, id)
	c.surplusMu.Unlock()
	// Phase lock survives plug cycles — it's a per-day decision,
	// not per-session. The day-rollover check in pickSurplusSteps
	// is the only natural reset point.
}

func (c *Controller) recordSurplus(id string, sample float64) float64 {
	c.surplusMu.Lock()
	defer c.surplusMu.Unlock()
	if c.surplusWin == nil {
		c.surplusWin = map[string]*surplusWindow{}
	}
	w, ok := c.surplusWin[id]
	if !ok {
		w = &surplusWindow{}
		c.surplusWin[id] = w
	}
	return w.push(sample)
}

func (c *Controller) getSurplusPause(id string) (bool, time.Time) {
	c.surplusMu.Lock()
	defer c.surplusMu.Unlock()
	return c.surplusPaused[id], c.surplusPausedAt[id]
}

func (c *Controller) setSurplusPause(id string, paused bool, at time.Time) {
	c.surplusMu.Lock()
	defer c.surplusMu.Unlock()
	if c.surplusPaused == nil {
		c.surplusPaused = map[string]bool{}
		c.surplusPausedAt = map[string]time.Time{}
	}
	c.surplusPaused[id] = paused
	if paused {
		c.surplusPausedAt[id] = at
	} else {
		delete(c.surplusPausedAt, id)
	}
}

// computeCommand resolves the W setpoint for a plugged loadpoint.
// Returns (0, false) when the planner has no allocation for this
// slot — caller commands an explicit 0 W standdown rather than
// leaving the charger riding the previous setpoint.
//
// The returned W is the CONTINUOUS energy-budget translation; the
// driver may further snap to its own discrete amperage steps and
// will clamp to the per-phase fuse ceiling derived from the
// `voltage` + `max_amps_per_phase` cmd fields.
func (c *Controller) computeCommand(now time.Time, lpCfg Config, currentPowerW float64) (float64, bool) {
	if c.plan == nil {
		return 0, false
	}
	d, ok := c.plan(now)
	if !ok {
		return 0, false
	}
	budgetWh, hasBudget := d.LoadpointEnergyWh[lpCfg.ID]
	if !hasBudget {
		return 0, false
	}
	remainingS := d.SlotEnd.Sub(now).Seconds()
	elapsed := d.SlotEnd.Sub(d.SlotStart).Seconds() - remainingS
	if elapsed < 0 {
		elapsed = 0
	}
	alreadyWh := currentPowerW * elapsed / 3600.0
	remainingWh := budgetWh - alreadyWh
	wantW := EnergyBudgetToPowerW(remainingWh, remainingS)
	// Joint fuse allocator (dispatch.go) caps EV demand when battery + EV
	// would together bust the fuse. Honour it before snapping to the
	// charger's discrete steps so the snap chooses a level under the cap.
	if c.fuseEVMax != nil {
		if cap, ok := c.fuseEVMax(); ok && cap >= 0 && wantW > cap {
			wantW = cap
		}
	}
	// Clamp to the loadpoint's static MaxChargeW (configured cap; the
	// driver's per-phase fuse clamp is the ultimate safety stop).
	return SnapChargeW(wantW, lpCfg.MinChargeW, lpCfg.MaxChargeW, lpCfg.AllowedStepsW), true
}
