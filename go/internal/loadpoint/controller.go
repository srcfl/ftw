package loadpoint

import (
	"context"
	"encoding/json"
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

	// wakeMu protects the per-loadpoint last-wake timestamp used to
	// throttle charge_start retries. Tesla rate-limits BLE commands;
	// retrying every 5 s would just exhaust the radio.
	wakeMu        sync.Mutex
	wakeLast      map[string]time.Time
	wakeKickUntil map[string]time.Time
}

// wakeKickDuration is how long a wake-kick forces the EV charger to
// signal min 3Φ current after a charge_start fires. The wallbox must
// actively present current (not 0 A) for the car to negotiate the new
// session — sending charge_start while Easee is at 0 A is futile.
// This briefly violates surplus_only's no-import rule, which is the
// price of recovering from a detached session without operator
// intervention. 15 s is enough for Tesla's BLE handshake plus a few
// seconds of pilot-signal stabilisation.
const wakeKickDuration = 30 * time.Second

// vehicleWakeCooldown caps how often we'll send a charge_start to the
// same loadpoint's matched vehicle. Tesla's BLE radio rate-limits
// "Command Disallowed" after a few rapid sends; 90 s gives the car
// time to actually transition out of Stopped before we poke again.
const vehicleWakeCooldown = 90 * time.Second

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
// controller only sees these three fields.
type EVSample struct {
	PowerW    float64
	SessionWh float64
	Connected bool
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
	// Detect the disconnected→connected edge (state.PluggedIn flips
	// from false to true) so we can reset session-scoped state
	// before the new session's first dispatch tick. Without this
	// the rolling-avg buffer keeps stale samples from the previous
	// session, biasing the first ~20 s of pause/resume decisions.
	wasPlugged := false
	if st, ok := c.manager.State(lpCfg.ID); ok {
		wasPlugged = st.PluggedIn
	}
	c.manager.Observe(lpCfg.ID, sample.Connected, sample.PowerW, sample.SessionWh)
	if !sample.Connected {
		c.resetSurplusSession(lpCfg.ID)
		return
	}
	if !wasPlugged {
		c.resetSurplusSession(lpCfg.ID)
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
		if lpCfg.SurplusOnly && holdW > 0 {
			clamped := c.computeSurplusCmd(lpCfg, holdW, sample.PowerW)
			if clamped < holdW {
				slog.Warn("loadpoint manual hold clamped by surplus_only",
					"lp", lpCfg.ID, "hold_w", holdW, "clamped_w", clamped)
				holdW = clamped
			}
		}
		cmd["power_w"] = holdW
		switch {
		case hold.PhaseMode != "":
			cmd["phase_mode"] = hold.PhaseMode
		case lpCfg.PhaseMode != "":
			cmd["phase_mode"] = lpCfg.PhaseMode
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
		// Wake-kick: when an auto-wake recently fired, force the
		// wallbox to signal at least min 3Φ current for a few
		// seconds so the car-side negotiation has something to land
		// on. This is the only thing that's empirically observed to
		// rescue a detached Tesla without operator intervention. The
		// kick window is bounded by wakeKickDuration; outside it the
		// normal surplus clamp resumes.
		if c.wakeKickActive(lpCfg.ID, now) {
			minKick := smallestNonZero(surplus3PhaseSteps(lpCfg))
			if minKick > 0 && cmdW < minKick {
				slog.Info("loadpoint wake-kick", "lp", lpCfg.ID,
					"prev_cmd_w", cmdW, "kick_w", minKick)
				cmdW = minKick
			}
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
		if lpCfg.SurplusOnly && cmdW > 0 {
			cmdW = c.computeSurplusCmd(lpCfg, cmdW, sample.PowerW)
		}
		cmd["power_w"] = cmdW
		// Pass operator's phase preferences through verbatim. The driver
		// reads these and decides 1Φ vs 3Φ based on its own knowledge of
		// charger min/max amps, phase-switch latency, and the requested W.
		if lpCfg.PhaseMode != "" {
			cmd["phase_mode"] = lpCfg.PhaseMode
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
	c.maybeWakeVehicle(ctx, now, lpCfg.ID, lpCfg, cmd)
}

func (c *Controller) maybeWakeVehicle(ctx context.Context, now time.Time, lpID string, lpCfg Config, cmd map[string]any) {
	if c == nil || c.vehicleStatus == nil || c.send == nil {
		return
	}
	pw, _ := cmd["power_w"].(float64)
	wantWake := pw > 0
	if lpCfg.SurplusOnly {
		wantWake = true
	}
	if !wantWake {
		return
	}
	driver, state, ok := c.vehicleStatus(lpID)
	if !ok || driver == "" {
		return
	}
	// Only Stopped / Disconnected / Complete are "needs wake" states.
	// Charging / Starting / NoPower mean the car is doing the right
	// thing on its own.
	switch state {
	case "Stopped", "Disconnected", "Complete":
	default:
		return
	}
	c.wakeMu.Lock()
	if c.wakeLast == nil {
		c.wakeLast = map[string]time.Time{}
		c.wakeKickUntil = map[string]time.Time{}
	}
	last := c.wakeLast[lpID]
	if !last.IsZero() && now.Sub(last) < vehicleWakeCooldown {
		c.wakeMu.Unlock()
		return
	}
	c.wakeLast[lpID] = now
	// Also arm the wake-kick window so the next few dispatch ticks
	// force the wallbox to signal current — without it the BLE
	// charge_start lands on a 0 A wallbox and the car has nothing
	// to negotiate with.
	c.wakeKickUntil[lpID] = now.Add(wakeKickDuration)
	c.wakeMu.Unlock()

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
//  4. When not paused, snap the lower of (planner wantW, avg surplus)
//     to a 3Φ-eligible step. Snapping to *avg* rather than *instant*
//     keeps the setpoint steady through brief dips — the home battery
//     fills the gap reactively for ~1-2 min, which is the user-
//     authorised smoothing budget.
func (c *Controller) computeSurplusCmd(lpCfg Config, wantW, currentEvW float64) float64 {
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
	steps3 := surplus3PhaseSteps(lpCfg)
	minStep3 := smallestNonZero(steps3)

	paused, pausedAt := c.getSurplusPause(lpCfg.ID)
	now := time.Now()
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

// resetSurplusSession drops the per-loadpoint rolling buffer + paused
// flag. Called on a plug-in edge (or unplug) so a new charging session
// starts with a clean view of surplus rather than inheriting the
// previous session's last samples — important when the car was
// unplugged for hours and the cached buffer is meaningless.
func (c *Controller) resetSurplusSession(id string) {
	c.surplusMu.Lock()
	defer c.surplusMu.Unlock()
	delete(c.surplusWin, id)
	delete(c.surplusPaused, id)
	delete(c.surplusPausedAt, id)
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
