package loadpoint

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// Controller turns one plan slot into an ev_set_current for each
// loadpoint. Phase choice stays in the driver. Dependencies are
// function values because mpc already imports this package.
type Controller struct {
	manager *Manager
	plan    PlanFunc
	tel     TelemetryFunc
	send    SenderFunc
	// sendOutcome matches Registry.SendWithOutcome. Production wires it so
	// the ev_set_current verdict closes Core health inside the registry's
	// per-driver actor, before that actor accepts another EV command.
	sendOutcome OutcomeSenderFunc
	// sendCycle carries private continuation identity beside the payload. The
	// driver never sees it; Registry.runLoop uses it to order pause/resume.
	sendCycle CycleSenderFunc

	// dispatchOutcome files the result of the periodic ev_set_current, and
	// only that command. See DispatchOutcomeFunc for what it deliberately
	// does not see. nil disables the reporting entirely.
	dispatchOutcome DispatchOutcomeFunc
	// driverOnline is Core's shared DriverHealth.IsOnline gate. A charger
	// excluded after refused commands must not keep reaching send and clear
	// its own refusal record before the retry window opens. nil keeps the
	// controller usable without Core health wiring in narrow tests.
	driverOnline func(driver string) bool
	// commandTimeout bounds synchronous sends so one slow charger cannot hold
	// the sequential loadpoint tick and starve every charger after it. The
	// registry owns cross-command serialization and lifecycle cancellation.
	commandTimeout  time.Duration
	wallboxCycleSeq atomic.Uint64
	resumeOffers    map[string]resumeOffer // owned by the dispatch tick
	energySamples   map[string]*meteredEnergy

	// fuseEVMax is this tick's EV share of the site fuse. nil means no cap.
	fuseEVMax func() (float64, bool)

	// perPhaseMeterAmps returns the live site-meter per-phase currents (L1,
	// L2, L3). Wired from telemetry; nil disables the per-phase fuse clamp.
	perPhaseMeterAmps func() (l1, l2, l3 float64, ok bool)
	// fusePhaseCapA holds the per-loadpoint per-phase EV current cap carried
	// between ticks by the reactive per-phase fuse clamp (tickOne goroutine).
	fusePhaseCapA map[string]float64

	// siteSurplusForEVW is live PV left after house load and battery soak.
	// A false reading pauses surplus_only rather than guessing an import.
	siteSurplusForEVW func() (float64, bool)

	// site is the grid fuse passed through on every ev_set_current.
	// Zero MaxAmps leaves the driver's own defaults.
	site SiteFuse
	// siteMu guards site against concurrent SetSiteFuse hot-reloads
	// (config-reload goroutine) vs the tick goroutine's reads.
	siteMu sync.RWMutex

	// holds win over the plan until they expire or the operator stops them.
	holdMu          sync.Mutex
	holds           map[string]ManualHold
	manualRestored  map[string]bool
	manualBindings  map[string]manualSessionBinding
	manualPersistMu sync.Mutex
	// manualIdleSince starts when a held car stops requesting current.
	// SessionCompletionTimeout then drops the hold, so Stop is not required.
	manualIdleMu    sync.Mutex
	manualIdleSince map[string]time.Time
	// manualHoldSaver, if non-nil, persists operator (Persistent) manual
	// holds so they survive a reboot / firmware update and the EV keeps
	// charging across the restart. Called on Set (persistent) and Clear.
	// Timed/diagnostic holds are intentionally NOT persisted (ephemeral).
	manualHoldSaver func(id string, h ManualHold, cleared bool)

	// batteryBoost leases are explicit, time-bounded authorisations for the
	// home battery to cover this loadpoint's charging draw. They are separate
	// from manual holds: a hold pins charger power, while a boost only relaxes
	// the normally-on battery-to-EV block and never bypasses charger, fuse, or
	// battery safety clamps. See battery_boost.go.
	batteryBoostMu      sync.Mutex
	batteryBoost        map[string]BatteryBoostLease
	batteryBoostStatus  map[string]BatteryBoostStatus
	batteryBoostSaver   func(id string, lease BatteryBoostLease, cleared bool)
	batteryBoostSafety  BatteryBoostSafetyFunc
	batteryBoostStopped func(id string, reason BatteryBoostStopReason)

	// surplusMu guards the pause/resume window so a brief PV dip does not
	// cycle the contactor.
	surplusMu       sync.Mutex
	surplusWin      map[string]*surplusWindow
	surplusPaused   map[string]bool
	surplusPausedAt map[string]time.Time
	// surplusStepW is the last applied surplus_only step per loadpoint.
	// Used for asymmetric step smoothing: down-steps track instant surplus
	// (no-import promise), up-steps are gated on the rolling average so a
	// transient surplus spike can't ratchet the EV up a step it can't hold
	// (which would whipsaw the home battery's PI). See computeSurplusCmd.
	surplusStepW map[string]float64

	// vehicleStatus is the bound vehicle driver and its charging state.
	// nil disables auto-wake.
	vehicleStatus        func(loadpointID string) (driver, chargingState string, ok bool)
	vehicleRefreshTarget func(loadpointID string) (string, error)
	vehicleChargeState   func(loadpointID string) (VehicleChargeState, bool)

	// peakRemainingSurplusW is the best PV-minus-load surplus left today.
	// Below the 3Φ minimum, surplus_only locks 1Φ for the day. nil keeps 3Φ.
	peakRemainingSurplusW func() (float64, bool)
	// nearTermPeakSurplusW is that peak over the next window. It may
	// allow 1Φ before the day lock does, so a cloud does not pin 1Φ
	// for the afternoon. nil skips the near-term gate.
	nearTermPeakSurplusW func(window time.Duration) (float64, bool)

	// nearTermLogLast throttles the 1Φ-allowed log to nearTermLogCooldown.
	nearTermLogMu   sync.Mutex
	nearTermLogLast map[string]time.Time

	// fusePauseUntil keeps a loadpoint at 0 W for fusePauseCooldown after
	// the fuse cap falls below its minimum step.
	fusePauseMu    sync.Mutex
	fusePauseUntil map[string]time.Time

	// phaseLockMu guards the day-long 1Φ lock and the 30 min phase dwell.
	// Without the dwell, a forecast near 4140 W flips the Easee contactor
	// every tick and winds up the battery PI.
	phaseLockMu     sync.Mutex
	phaseLocked1P   map[string]bool
	phaseLockedAt   map[string]time.Time
	phaseSelected3P map[string]bool
	phaseSelectedAt map[string]time.Time

	// wakeMu guards wake timestamps. Tesla rate-limits BLE, so a 5 s retry
	// would exhaust the radio.
	wakeMu        sync.Mutex
	wakeLast      map[string]time.Time
	wakeKickUntil map[string]time.Time
	wakeAttempts  map[string]int

	// batSoC reports the home battery's current state-of-charge (0..1
	// fraction) for the bat-SoC surplus-unlock feature. nil disables
	// the feature entirely — the LP behaves exactly as today.
	batSoC func() (float64, bool)

	// gridDeferredMu guards loadpoints whose deadline sits past published
	// prices. Those ticks follow surplus, so a sunny plan cannot import
	// after the forecast misses.
	gridDeferredMu sync.Mutex
	gridDeferred   map[string]bool

	// batSoCArmed is the surplus-unlock hysteresis. batSoCNoPV counts
	// ticks with no live PV so a full battery does not kick the EV
	// through the night.
	batSoCArmedMu sync.Mutex
	batSoCArmed   map[string]bool
	batSoCNoPV    map[string]int
}

// batSoCPVGoneTicks is about 30 s of no live PV before the bat-SoC
// unlock releases. Shorter flaps on a cloud. Longer kicks the EV into the evening.
const batSoCPVGoneTicks = 6

// wakeKickDuration is how long the wallbox must offer current after
// charge_start. Easee at 0 A gives the car nothing to negotiate.
// The window may import. That is the cost of an unattended recover.
const wakeKickDuration = 30 * time.Second

// wakeBackoffAfter is failed wakes before the cooldown stretches.
// The BLE radio rate-limits a steady 90 s poke. Charging or Starting resets it.
const wakeBackoffAfter = 5

// wakeBackoffCooldown is the slow retry after wakeBackoffAfter.
const wakeBackoffCooldown = 10 * time.Minute

// vehicleWakeCooldown is the gap between charge_start attempts.
// The wallbox cycle is what recovers a detached session. charge_start
// on top of the proxy's own polls trips Tesla's command limit.
const vehicleWakeCooldown = 5 * time.Minute

// vehicleWakeTimeout bounds a fire-and-forget wake so a stuck proxy
// call cannot leak the goroutine. The proxy's own timeout is about 15 s.
const vehicleWakeTimeout = 30 * time.Second

// surplusWindowSize is the pause/resume average, about 20 s at a 5 s tick.
const surplusWindowSize = 4

// surplusResumeMarginW keeps resume from oscillating on the minimum step.
const surplusResumeMarginW = 200.0

// surplusMinPauseHold stays above Easee's ~30 s contactor minimum.
const surplusMinPauseHold = 35 * time.Second

// nearTermLogCooldown is one 1Φ-allowed log per 10 min per loadpoint.
const nearTermLogCooldown = 10 * time.Minute

// phaseSwitchMinHold is one 1Φ/3Φ change per 30 min. Faster burns the
// Easee contactor and winds up the battery PI.
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

// MaxManualHold bounds a timed manual hold so a forgotten diagnostic
// override cannot indefinitely displace planned dispatch. Persistent
// operator holds are exempt: they end on Stop or unplug, never on time.
// One constant for every door that installs a hold — the HTTP route and
// the app session validate against this same number.
const MaxManualHold = 30 * time.Minute

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
	// Persistent marks an operator "Start" / amp-slider override that
	// never expires on time — it is released only by ClearManualHold
	// ("Stop") or on unplug. A persistent hold carries a zero ExpiresAt;
	// the flag is what distinguishes it from the zero-ExpiresAt "clear"
	// sentinel that SetManualHold honours.
	Persistent bool

	// ReleaseAtSoC (0–1) turns the hold into "charge now, then back to
	// the plan": once the loadpoint's estimated (or BMS-anchored) SoC
	// reaches this fraction, the controller clears the hold and the
	// same tick falls through to automatic surplus/plan dispatch.
	// Zero keeps the legacy contract — pinned until Stop or unplug.
	// Persisted with the hold, so a restart mid-boost keeps the
	// release target.
	ReleaseAtSoC float64

	// StartedAt remains the first request time when the current changes.
	StartedAt time.Time
	// UpdatedAt identifies the latest choice, including a current change.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Directive is the loadpoint-relevant slice of mpc.SlotDirective.
// The mpc package defines the full type with BatteryEnergyWh etc;
// the controller only needs the slot window and per-loadpoint Wh
// budget, so we don't pull in the whole struct.
type Directive struct {
	SlotStart          time.Time
	SlotEnd            time.Time
	LoadpointEnergyWh  map[string]float64
	LoadpointMaxPowerW map[string]float64
}

// EVSample is the loadpoint-relevant slice of telemetry.DerReading
// for a DerEV entry — power, cumulative session energy, plug state.
// Chargers like Easee don't expose the vehicle's BMS SoC, so the
// controller only sees these four fields.
//
// RequestActive is true when the vehicle is (or could imminently be)
// drawing current. Drivers that can distinguish "throttled to 0" from
// "the vehicle has explicitly stopped requesting current" set this to
// false on the latter. Drivers without that distinction leave it
// true. The loadpoint manager uses it to detect vehicle-side
// completion via the SessionCompletionTimeout timer.
type EVSample struct {
	// ConnectionUnknown is a socket transition without fresh physical status.
	// It revokes hardware proof but must not imply a physical unplug.
	ConnectionUnknown    bool
	ConnectionGeneration uint64 // process-local transport epoch, not durable session proof
	PowerW               float64
	SessionWh            float64
	SessionWhUnavailable bool
	PowerUnavailable     bool
	PowerAt              time.Time
	PowerMaxAge          time.Duration
	EnergyAt             time.Time
	Connected            bool
	RequestActive        bool
	DeviceID             string
	SessionID            string
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

// OutcomeSenderFunc forwards one command and runs outcome inside the
// per-driver command owner after any default recovery, before its next queued
// command. Only periodic ev_set_current uses this path.
type OutcomeSenderFunc func(ctx context.Context, driver string, payload []byte, outcome func(error)) error

// CycleSenderFunc sends one automatic wallbox-cycle step with an out-of-band
// continuation id owned by the registry actor.
type CycleSenderFunc func(ctx context.Context, driver string, payload []byte, cycleID uint64) error

// DispatchOutcomeFunc reports the periodic ev_set_current only. A charger
// that polls and refuses that command must drop out of the plan. Standdown,
// vehicle charge_start, contactor cycle, and operator force-start are not
// evidence the charger rejects a setpoint. The callback does no I/O.
type DispatchOutcomeFunc func(driver string, err error, now time.Time)

// NewController wires the dependencies. Passing nil for plan, tel,
// or send disables the corresponding step — useful in tests.
func NewController(mgr *Manager, plan PlanFunc, tel TelemetryFunc, send SenderFunc) *Controller {
	return &Controller{
		manager:            mgr,
		plan:               plan,
		tel:                tel,
		send:               send,
		batteryBoost:       map[string]BatteryBoostLease{},
		batteryBoostStatus: map[string]BatteryBoostStatus{},
	}
}

// SetDispatchOutcome wires the reporter for refused EV dispatch commands.
// Pass nil to disable — the controller then behaves exactly as it did before,
// logging the refusal and moving on.
func (c *Controller) SetDispatchOutcome(f DispatchOutcomeFunc) {
	if c == nil {
		return
	}
	c.dispatchOutcome = f
}

// SetOutcomeSender installs the registry-owned completion path for periodic
// charger dispatch. Pass nil to keep the direct SenderFunc test path.
func (c *Controller) SetOutcomeSender(send OutcomeSenderFunc) {
	if c == nil {
		return
	}
	c.sendOutcome = send
}

// SetCycleSender installs the registry-owned pause/resume continuation path.
// Pass nil to keep the plain SenderFunc path used by narrow controller tests.
func (c *Controller) SetCycleSender(send CycleSenderFunc) {
	if c == nil {
		return
	}
	c.sendCycle = send
}

// SetDriverOnline wires Core's per-driver control eligibility. Returning
// false preserves observation and schedule state but emits no command or
// wake side effect for that loadpoint.
func (c *Controller) SetDriverOnline(f func(driver string) bool) {
	if c == nil {
		return
	}
	c.driverOnline = f
}

func (c *Controller) driverCanDispatch(driver string) bool {
	return c.driverOnline == nil || c.driverOnline(driver)
}

const defaultDriverCommandTimeout = 2 * time.Second

// SetCommandTimeout sets the per-send ceiling for periodic charger dispatch
// and the async wallbox cycle. Vehicle commands keep their caller deadline,
// capped by vehicleWakeTimeout. Non-positive values restore the safe default.
func (c *Controller) SetCommandTimeout(timeout time.Duration) {
	if c == nil {
		return
	}
	c.commandTimeout = timeout
}

func (c *Controller) sendDispatchWithDeadline(ctx context.Context, driver string, payload []byte) error {
	if c == nil || c.send == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := c.commandTimeout
	if timeout <= 0 {
		timeout = defaultDriverCommandTimeout
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.send(cmdCtx, driver, payload)
}

// sendVehicle keeps vehicle operations out of the short periodic-dispatch
// deadline. The API supplies a 15-second caller deadline for operator work;
// background wake paths receive the 30-second vehicleWakeTimeout cap.
func (c *Controller) sendVehicle(ctx context.Context, driver string, payload []byte) error {
	if c == nil || c.send == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	vehicleCtx, cancel := context.WithTimeout(ctx, vehicleWakeTimeout)
	defer cancel()
	return c.send(vehicleCtx, driver, payload)
}

func (c *Controller) sendOutcomeWithDeadline(
	ctx context.Context,
	driver string,
	payload []byte,
	outcome func(error),
) error {
	if c == nil || c.send == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := c.commandTimeout
	if timeout <= 0 {
		timeout = defaultDriverCommandTimeout
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if c.sendOutcome != nil {
		return c.sendOutcome(cmdCtx, driver, payload, outcome)
	}
	err := c.send(cmdCtx, driver, payload)
	if outcome != nil {
		outcome(err)
	}
	return err
}

func (c *Controller) sendCycleWithDeadline(
	ctx context.Context,
	driver string,
	payload []byte,
	cycleID uint64,
) error {
	if c == nil || c.send == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := c.commandTimeout
	if timeout <= 0 {
		timeout = defaultDriverCommandTimeout
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if c.sendCycle != nil {
		return c.sendCycle(cmdCtx, driver, payload, cycleID)
	}
	return c.send(cmdCtx, driver, payload)
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

// SetPerPhaseMeterAmps wires the live site-meter per-phase current reader
// used by the per-phase fuse clamp.
func (c *Controller) SetPerPhaseMeterAmps(f func() (l1, l2, l3 float64, ok bool)) {
	if c == nil {
		return
	}
	c.perPhaseMeterAmps = f
}

// SetSiteSurplusForEV wires a per-tick "PV surplus available to the
// EV" reader for the surplus_only clamp. The function returns watts
// the EV may claim this tick: leftover after house load, minus
// PV-soak (SurplusAvailableForEVW). Called once at startup from
// main.go. Pass nil to disable, in which case surplus_only is
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

// SetVehicleRefreshTarget selects an authorized telemetry recipient from
// configuration. It must not require a fresh SoC or permit charging.
func (c *Controller) SetVehicleRefreshTarget(f func(string) (string, error)) {
	if c != nil {
		c.vehicleRefreshTarget = f
	}
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

// applyFuseClampAndCooldown caps wantW at the joint fuse share.
// Below the minimum step it pauses for fusePauseCooldown, so a
// transient does not flap. A hold cannot override it.

// fusePhaseMarginA is the per-phase amp headroom held below the breaker.
const fusePhaseMarginA = 1.0

// fusePhaseStepUpA is how fast the per-phase EV cap recovers per tick once a
// phase has headroom — gentle enough not to re-trip, quick enough to ramp back.
const fusePhaseStepUpA = 1.0

// nextFusePhaseCapA reactively caps the EV's per-phase offer so the worst
// measured site-meter phase stays at or below (fuseA - marginA). Reductions are
// immediate (the full overage) to protect the breaker; ramp-up is gradual
// (stepA/tick). prevCapA <= 0 means "uninitialised" -> start from the full fuse.
// Pure for testability.
func nextFusePhaseCapA(prevCapA, worstMeterA, fuseA, marginA, stepA float64) float64 {
	if fuseA <= 0 {
		return prevCapA
	}
	limit := fuseA - marginA
	capA := prevCapA
	if capA <= 0 {
		capA = fuseA
	}
	switch {
	case worstMeterA > limit:
		capA -= worstMeterA - limit
	case worstMeterA+stepA <= limit:
		capA += stepA
	}
	if capA < 0 {
		capA = 0
	}
	if capA > fuseA {
		capA = fuseA
	}
	return capA
}

// applyPerPhaseFuseClamp caps one phase. The site-total fuse guard
// misses a phase that is already near the breaker (L1 at 18 A on a 16 A fuse).
func (c *Controller) applyPerPhaseFuseClamp(lpCfg Config, cmd map[string]any) {
	if c == nil || c.perPhaseMeterAmps == nil {
		return
	}
	fuseA := c.siteFuse().MaxAmps
	if fuseA <= 0 {
		return
	}
	l1, l2, l3, ok := c.perPhaseMeterAmps()
	if !ok {
		return
	}
	worst := math.Max(l1, math.Max(l2, l3))
	if c.fusePhaseCapA == nil {
		c.fusePhaseCapA = map[string]float64{}
	}
	capA := nextFusePhaseCapA(c.fusePhaseCapA[lpCfg.ID], worst, fuseA, fusePhaseMarginA, fusePhaseStepUpA)
	c.fusePhaseCapA[lpCfg.ID] = capA
	if capA >= fuseA {
		return
	}
	if existing, has := cmd["max_amps_per_phase"].(float64); !has || capA < existing {
		cmd["max_amps_per_phase"] = capA
		slog.Info("loadpoint per-phase fuse clamp",
			"lp", lpCfg.ID, "worst_phase_a", worst, "ev_cap_a", capA, "fuse_a", fuseA)
	}
}

// The reason is "", "fuse_limit", or "fuse_cooldown".
func (c *Controller) applyFuseClampAndCooldown(now time.Time, lpCfg Config, wantW float64) (float64, string) {
	if c == nil {
		return wantW, ""
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
		return 0, "fuse_cooldown"
	}
	if c.fuseEVMax == nil {
		return wantW, ""
	}
	cap, ok := c.fuseEVMax()
	if !ok || cap < 0 {
		return wantW, ""
	}
	if wantW <= cap {
		return wantW, ""
	}
	// Need to ramp down. Snap to the largest allowed step ≤ cap.
	snapped := floorChargeW(cap, lpCfg.MinChargeW, lpCfg.MaxChargeW, lpCfg.AllowedStepsW)
	if snapped > 0 && snapped >= lpCfg.MinChargeW {
		slog.Info("loadpoint fuse-clamp: ramped down",
			"lp", lpCfg.ID, "want_w", wantW, "fuse_cap_w", cap, "snapped_w", snapped)
		return snapped, "fuse_limit"
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
	return 0, "fuse_cooldown"
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
// (threshold − BatSoCUnlockHyst), or after a sustained run of
// zero/negative live surplus (batSoCPVGoneTicks).
//
// Returns false when no threshold is configured or the bat_soc reader
// is missing.
func (c *Controller) evalBatSoCArm(lpID string, threshold float64) bool {
	if c == nil || c.batSoC == nil || threshold <= 0 {
		return false
	}
	// Read surplus before batSoCArmedMu. The closure calls back into
	// AnyLoadpointSurplusActive, which takes the same lock.
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
	armed := prev
	switch {
	case soc < threshold-BatSoCUnlockHyst:
		armed = false
	case soc >= threshold && !pvGone:
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

// GridDeferred reports whether MPC has deferred grid-funded planning
// for this loadpoint (target deadline past the published price
// horizon). Read by the API layer so the deferral is visible to the
// operator instead of looking like a PV-only mode nobody chose.
func (c *Controller) GridDeferred(lpID string) bool {
	return c.gridDeferredFor(lpID)
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

// surplusActive is true when live surplus replaces the plan: SurplusOnly,
// a grid-deferred loadpoint, or an armed bat-SoC unlock. A schedule
// target is a floor instead. See surplusAddsToPlan.
func (c *Controller) surplusActive(lpCfg Config, sched Schedule) bool {
	if lpCfg.SurplusOnly {
		return true
	}
	// A deadline may need grid. Do not snap that plan down to live PV.
	if sched.HasTarget() {
		return false
	}
	if c.gridDeferredFor(lpCfg.ID) {
		return true
	}
	return c.evalBatSoCArm(lpCfg.ID, sched.SurplusUnlockBatSoC)
}

// surplusAddsToPlan reports whether spare PV may be added ON TOP of the
// plan this tick: a schedule target is set, SurplusOnly is off, and the
// bat-SoC unlock is armed. The plan's grid charge is the floor and the
// command becomes max(plan, surplus); surplus never throttles the plan
// (the 2026-05-30 directive above still holds). This is what the
// Scheduled tab's "Also charge from PV surplus" + "Home battery ≥ %"
// controls mean, since the UI always saves them together with a target
// (#1060).
//
// Exactly one of surplusActive and surplusAddsToPlan evaluates the arm on
// a given tick, so its hysteresis counters advance once per tick.
func (c *Controller) surplusAddsToPlan(lpCfg Config, sched Schedule) bool {
	if lpCfg.SurplusOnly || !sched.HasTarget() {
		return false
	}
	return c.evalBatSoCArm(lpCfg.ID, sched.SurplusUnlockBatSoC)
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
		if sched.SurplusUnlockBatSoC > 0 {
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

// RefreshVehicle requests telemetry only. The driver retains its durable
// wake limit; refreshing a goal never resets the charge_start cooldown.
func (c *Controller) RefreshVehicle(ctx context.Context, lpID string) error {
	if c == nil || c.vehicleRefreshTarget == nil || c.send == nil {
		return nil
	}
	driver, err := c.vehicleRefreshTarget(lpID)
	if err != nil {
		return err
	}
	if driver == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"action": "wake_up"})
	if err != nil {
		return err
	}
	return c.sendVehicle(ctx, driver, payload)
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

// ForceStartVehicle sends charge_start now, past the auto-wake backoff.
// A failed send still clears the backoff and arms the wake-kick: the
// operator asked to retry, and a 0 A wallbox cannot complete the start.
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
	sendErr := c.sendVehicle(ctx, driver, payload)
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
	if err := c.sendVehicle(ctx, driver, payload); err != nil {
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
// The arm is raw state: it says nothing about whether surplus replaces
// the plan or adds to it. main.go only marks the planner spec
// surplus-only when the loadpoint has no schedule target (the case
// where the arm replaces the plan, surplusActive); under a target the
// arm adds to the plan (surplusAddsToPlan) and the planner must keep
// planning the grid charge the deadline needs (#1060).
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
	c.siteMu.Lock()
	c.site = f
	c.siteMu.Unlock()
}

// siteFuse returns a snapshot of the site fuse under read lock, safe
// against a concurrent SetSiteFuse hot-reload.
func (c *Controller) siteFuse() SiteFuse {
	c.siteMu.RLock()
	defer c.siteMu.RUnlock()
	return c.site
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
// ClearManualHold) UNLESS h.Persistent is set, in which case it
// installs a never-expiring override. Setting a hold for an unknown
// loadpoint ID is silently allowed — the hold has no effect because
// tickOne only runs for configured loadpoints.
func (c *Controller) SetManualHold(id string, h ManualHold) {
	if c == nil {
		return
	}
	c.manualPersistMu.Lock()
	defer c.manualPersistMu.Unlock()
	c.markManualExplicit(id)
	if h.PowerW > 0 && c.manager != nil {
		c.manager.RetryCharging(id)
	}
	c.holdMu.Lock()
	if c.holds == nil {
		c.holds = map[string]ManualHold{}
	}
	cleared := false
	if h.ExpiresAt.IsZero() && !h.Persistent {
		delete(c.holds, id)
		cleared = true
	} else {
		if h.StartedAt.IsZero() {
			h.StartedAt = time.Now()
		}
		h.UpdatedAt = time.Now()
		c.holds[id] = h
	}
	saver := c.manualHoldSaver
	c.holdMu.Unlock()
	c.resetManualIdle(id)
	// Persist outside the lock (saver may do disk I/O). Only persistent
	// operator holds survive a restart; clearing or a timed hold writes the
	// "cleared" sentinel so a stale persistent hold isn't resurrected.
	if saver != nil {
		if cleared || !h.Persistent {
			saver(id, ManualHold{}, true)
		} else {
			saver(id, h, false)
		}
	}
}

// ClearManualHold removes any active hold for the given loadpoint,
// regardless of expiry. Idempotent.
func (c *Controller) ClearManualHold(id string) {
	if c == nil {
		return
	}
	c.manualPersistMu.Lock()
	defer c.manualPersistMu.Unlock()
	c.clearManualHoldLocked(id)
}

// releaseManualHoldIfCurrent applies a tick's decision only to the request
// it read. A newer Pause, Start or slider change keeps its own command.
func (c *Controller) releaseManualHoldIfCurrent(id string, expected ManualHold) bool {
	if c == nil {
		return false
	}
	c.manualPersistMu.Lock()
	defer c.manualPersistMu.Unlock()
	c.holdMu.Lock()
	current, found := c.holds[id]
	c.holdMu.Unlock()
	if !found || current != expected {
		return false
	}
	c.clearManualHoldLocked(id)
	return true
}

// clearManualHoldLocked requires manualPersistMu. Keep removal and its save
// ordered with explicit commands and session restoration.
func (c *Controller) clearManualHoldLocked(id string) {
	first := c.markManualExplicit(id)
	c.holdMu.Lock()
	_, existed := c.holds[id]
	delete(c.holds, id)
	saver := c.manualHoldSaver
	c.holdMu.Unlock()
	// Only persist the clear if a hold actually existed — ClearManualHold is
	// called on every unplugged tick, and we must not hammer the store.
	if saver != nil && (existed || first) {
		saver(id, ManualHold{}, true)
	}
	// The auto-release idle timer is meaningless without a hold.
	c.resetManualIdle(id)
}

// manualHoldIdleFor returns how long the loadpoint has continuously seen
// the vehicle "not requesting current" while a manual hold is active,
// starting the timer on the first such tick. Used to debounce the
// auto-release of a hold once the car is full / declines.
func (c *Controller) manualHoldIdleFor(id string, now time.Time) time.Duration {
	c.manualIdleMu.Lock()
	defer c.manualIdleMu.Unlock()
	if c.manualIdleSince == nil {
		c.manualIdleSince = map[string]time.Time{}
	}
	since, ok := c.manualIdleSince[id]
	if !ok {
		c.manualIdleSince[id] = now
		return 0
	}
	return now.Sub(since)
}

// resetManualIdle clears the auto-release idle timer for a loadpoint —
// called when the vehicle resumes requesting current or the hold is
// removed, so the next idle spell is timed from a clean start.
func (c *Controller) resetManualIdle(id string) {
	c.manualIdleMu.Lock()
	delete(c.manualIdleSince, id)
	c.manualIdleMu.Unlock()
}

// SetManualHoldSaver wires the persistence callback for operator manual
// holds. Pass nil to disable. Wire it AFTER restoring persisted holds at
// startup so the restore doesn't immediately re-write what it just read.
func (c *Controller) SetManualHoldSaver(fn func(id string, h ManualHold, cleared bool)) {
	if c == nil {
		return
	}
	c.holdMu.Lock()
	c.manualHoldSaver = fn
	c.holdMu.Unlock()
}

// GetManualHold returns the current hold for a loadpoint. The bool
// is false when no hold is active. Time-bounded holds past their
// ExpiresAt are lazily evicted on the next read. A Persistent hold
// (operator "Start" / amp-slider override) never expires on time and
// is only released by ClearManualHold ("Stop") or on unplug (tickOne).
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
	if !h.Persistent && !now.Before(h.ExpiresAt) {
		delete(c.holds, id)
		return ManualHold{}, false
	}
	return h, true
}

// Tick runs one dispatch cycle for every configured loadpoint.
func (c *Controller) Tick(ctx context.Context, now time.Time) {
	c.TickWithDispatch(ctx, now, true)
}

// TickWithDispatch runs the normal observation/state cycle while making the
// final actuation permission explicit. When dispatchAllowed is false, charger
// telemetry still updates Manager state and persistent schedules/holds remain
// intact, but a connected loadpoint receives an explicit 0 W standdown and no
// wake or contactor-cycle side effects are emitted. The caller owns the site
// freshness decision so EV and storage share one pre-dispatch safety boundary.
func (c *Controller) TickWithDispatch(ctx context.Context, now time.Time, dispatchAllowed bool) {
	if c == nil || c.manager == nil || c.tel == nil {
		return
	}
	// Stop every affected charger before observing any session. Observation
	// may restore or sync state.db; a stalled disk must not delay standdown,
	// including the chargers after the one whose checkpoint is blocked.
	type observation struct {
		config Config
		sample EVSample
	}
	var observations []observation
	for _, cfg := range c.manager.Configs() {
		sample, observed := c.tel(cfg.DriverName)
		if !observed {
			continue
		}
		observations = append(observations, observation{cfg, sample})
		// Only a stale site meter stands chargers down. An old charger power
		// reading from a live driver is not a hardware risk: the fuse clamps
		// work from the site meter, and energy accounting already treats the
		// gap as unmeasured. Easee reports power only when it changes, so
		// steady charging looked stale after three minutes and Core stopped
		// the car every few minutes.
		if sample.Connected && !sample.ConnectionUnknown && !dispatchAllowed && c.driverCanDispatch(cfg.DriverName) {
			c.standDownBeforeStorage(ctx, now, cfg, sample)
		}
	}
	if c.plan == nil && dispatchAllowed {
		return
	}
	for _, observed := range observations {
		c.tickOne(ctx, now, observed.config, observed.sample, dispatchAllowed)
	}
}

func (c *Controller) standDownBeforeStorage(ctx context.Context, now time.Time, cfg Config, sample EVSample) {
	var manualUpdatedAt time.Time
	if hold, held := c.GetManualHold(cfg.ID, now); held {
		manualUpdatedAt = hold.UpdatedAt
	}
	c.manager.setCommandedForManual(cfg.ID, 0, "site_meter_stale", manualUpdatedAt)
	if c.send == nil {
		return
	}
	// A safety withdrawal does not report a normal dispatch outcome: the
	// driver-health owner already handles the unavailable measurement.
	if err := c.sendDispatchWithDeadline(ctx, cfg.DriverName, []byte(`{"action":"ev_set_current","power_w":0}`)); err != nil {
		slog.Warn("loadpoint safety standdown", "lp", cfg.ID, "driver", cfg.DriverName, "err", err)
	} else {
		c.resumeAfterZeroOffer(ctx, cfg, sample, 0, now)
	}
}

func (c *Controller) tickOne(ctx context.Context, now time.Time, lpCfg Config, sample EVSample, dispatchAllowed bool) {
	c.observeEnergy(lpCfg, sample, now)
	c.manager.observeConnectionProof(lpCfg.ID, sample.ConnectionGeneration, sample.ConnectionUnknown)
	if sample.ConnectionUnknown {
		delete(c.resumeOffers, lpCfg.ID)
		c.restoreManualHoldForSession(lpCfg.ID)
		return
	}
	// Resolve the schedule once per tick — used for the bat-SoC unlock
	// (surplusActive / surplusAddsToPlan) and the phase decision below.
	// Zero value when no schedule is set, which makes evalBatSoCArm a
	// no-op.
	var sched Schedule
	if c.manager != nil {
		sched, _ = c.manager.GetSchedule(lpCfg.ID)
	}
	// surplusOn: surplus REPLACES the plan (surplus-only semantics).
	// surplusAdds: surplus is ADDED on top of a scheduled plan. Never
	// both true.
	surplusOn := c.surplusActive(lpCfg, sched)
	surplusAdds := c.surplusAddsToPlan(lpCfg, sched)
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
	// As of last tick, were we pausing this surplus loadpoint below its
	// floor? (computeSurplusCmd for THIS tick runs later.) A charger that
	// reports "not requesting current" while we're withholding power is
	// responding to our own pause, not declining — so it must not count
	// toward session completion, and the same signal drives the resume
	// wallbox-cycle in maybeWakeVehicle below.
	enteringSurplusPaused, _ := c.getSurplusPause(lpCfg.ID)
	selfWithheld := surplusOn && enteringSurplusPaused
	c.manager.SetSurplusWithheld(lpCfg.ID, selfWithheld)
	c.manager.ObserveSample(lpCfg.ID, sample)
	c.restoreManualHoldForSession(lpCfg.ID)
	c.evaluateBatteryBoost(lpCfg.ID, now, sample.Connected, dispatchAllowed && !sample.PowerUnavailable)
	if !sample.Connected {
		delete(c.resumeOffers, lpCfg.ID)
		c.resetSurplusSession(lpCfg.ID)
		// Release any manual override when the vehicle unplugs. A
		// persistent "Start" hold (zero ExpiresAt) would otherwise
		// survive the session and silently re-apply to the next car;
		// "until Stop or unplug" is the operator's mental model.
		c.ClearManualHold(lpCfg.ID)
		return
	}
	if !wasPlugged {
		c.resetSurplusSession(lpCfg.ID)
	}
	if !c.driverCanDispatch(lpCfg.DriverName) {
		// Keep the observation above: the UI and session state should still
		// show the connected car. Only actuation pauses while Core holds the
		// driver out of control. In particular, do not report a synthetic
		// outcome here; driverActuationTracker.update owns the timed retry.
		return
	}
	if !dispatchAllowed {
		// Standdown already ran before storage. Keep observations and goals,
		// but do not advance completion timers or perform wake side effects.
		return
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

	// Auto-release an operator manual hold once the vehicle has stopped
	// requesting current (it hit its own charge limit / is full). Without
	// this a "Start" hold pins the wallbox at a fixed amperage and the
	// loadpoint shows "charging" at 0 W until the operator presses Stop.
	// Debounced by SessionCompletionTimeout so a brief ramp/handshake dip
	// — or a car that hasn't woken yet — doesn't drop the hold early. Only
	// trips for drivers that distinguish "explicitly not requesting" from
	// "throttled to 0" (RequestActive); others leave it true and never
	// auto-release. Done before the dispatch branch below so the freed
	// tick falls straight through to automatic (surplus/plan) dispatch.
	if hold, held := c.GetManualHold(lpCfg.ID, now); held && hold.PowerW > 0 {
		if !sample.RequestActive {
			if c.manualHoldIdleFor(lpCfg.ID, now) >= SessionCompletionTimeout && c.releaseManualHoldIfCurrent(lpCfg.ID, hold) {
				slog.Info("loadpoint manual hold auto-released — vehicle stopped requesting current (full/declined)",
					"lp", lpCfg.ID, "idle", SessionCompletionTimeout)
			}
		} else {
			c.resetManualIdle(lpCfg.ID)
		}
	}

	// Release a "charge now" hold at its target SoC. The operator asked
	// for immediate charge up to a level, not a pin-forever: clearing
	// here lets this same tick fall straight through to automatic
	// surplus/plan dispatch instead of holding the wallbox at a fixed
	// amperage the rest of the session.
	if hold, held := c.GetManualHold(lpCfg.ID, now); held && hold.ReleaseAtSoC > 0 {
		if st, ok := c.manager.State(lpCfg.ID); ok && st.CurrentSoC >= hold.ReleaseAtSoC && c.releaseManualHoldIfCurrent(lpCfg.ID, hold) {
			slog.Info("loadpoint manual hold released — charge-now target reached",
				"lp", lpCfg.ID, "soc", st.CurrentSoC, "release_at_soc", hold.ReleaseAtSoC)
		}
	}

	cmd := map[string]any{"action": "ev_set_current"}
	// cmdReason names the branch that decides this tick's power_w; every
	// clamp that overrides the value overrides the reason with it. Fed
	// to Manager.SetCommanded after the last clamp has spoken.
	cmdReason := ""
	var manualCommandUpdatedAt time.Time
	if hold, ok := c.GetManualHold(lpCfg.ID, now); ok {
		cmdReason = "manual_hold"
		manualCommandUpdatedAt = hold.UpdatedAt
		// Manual override active — skip MPC translation. The hold's
		// non-zero fields override the loadpoint config + site fuse;
		// zero/empty fields fall through to the normal defaults so a
		// minimal hold (just `power_w`) still carries the per-phase
		// fuse clamp inputs the driver needs to stay safe.
		holdW := clampManualPower(lpCfg, hold, c.siteFuse())
		if holdW != hold.PowerW {
			cmdReason = "charger_limit"
		}
		// An explicit manual hold ("Start" / amp slider) takes priority
		// over surplus_only: when the operator deliberately pins a charge
		// rate we honour it even if that means importing from the grid.
		// surplus_only still governs *automatic* dispatch (the non-hold
		// branch below) — releasing the hold ("Stop") drops straight back
		// into PV-surplus-only charging. The fuse clamp below is the one
		// guard a manual hold can never override. (surplusOn still drives
		// the phase-mode fallback further down.)
		// Fuse protection: even an operator-pinned hold must not bust
		// the fuse. Apply the joint allocator's FuseEVMax cap and the
		// pause-cooldown guard before sending. A sticky 11 kW Start
		// hold + house drawing 7 A on one phase = fuse trip without
		// this clamp.
		var fuseReason string
		holdW, fuseReason = c.applyFuseClampAndCooldown(now, lpCfg, holdW)
		if fuseReason != "" {
			cmdReason = fuseReason
		}
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
		site := c.siteFuse()
		switch {
		case hold.Voltage > 0:
			cmd["voltage"] = hold.Voltage
		case site.Voltage > 0:
			cmd["voltage"] = site.Voltage
		}
		switch {
		case hold.MaxAmpsPerPhase > 0:
			cmd["max_amps_per_phase"] = hold.MaxAmpsPerPhase
		case site.MaxAmps > 0:
			cmd["max_amps_per_phase"] = site.MaxAmps
		}
		switch {
		case hold.SitePhases > 0:
			cmd["site_phases"] = hold.SitePhases
		case site.MaxAmps > 0:
			cmd["site_phases"] = site.Phases()
		}
	} else {
		cmdW, planReady, fuseCapped := c.computeCommand(now, lpCfg, sample.PowerW)
		cmdReason = "plan"
		if fuseCapped {
			cmdReason = "fuse_limit"
		}
		if !planReady {
			// No plan budget for this loadpoint right now — explicit
			// 0 W standdown so the charger pauses cleanly.
			cmdW = 0
			cmdReason = "no_plan_budget"
		}
		finishW, finishing := c.vehicleCompletionOffer(lpCfg, now)
		if finishing {
			cmdW = finishW
			cmdReason = "vehicle_limit_completion"
		}
		// Surplus replaces the slot budget. A missing plan still tries
		// MaxChargeW, or a charger with no vehicle driver never starts.
		if surplusOn {
			wantW := cmdW
			if wantW <= 0 {
				wantW = lpCfg.MaxChargeW
			}
			cmdW = c.computeSurplusCmd(now, lpCfg, wantW, sample.PowerW)
			if cmdW > 0 {
				cmdReason = "pv_surplus"
			} else {
				cmdReason = "pv_surplus_pause"
			}
		}
		// Spare PV may raise a scheduled charge. It must not lower it.
		if surplusAdds {
			surplusW := c.computeSurplusCmd(now, lpCfg, lpCfg.MaxChargeW, sample.PowerW)
			if surplusW > cmdW {
				cmdW = surplusW
				cmdReason = "pv_surplus"
			}
		}
		// After the surplus clamp, a wake-kick still offers the minimum
		// step. A 0 A wallbox cannot finish the session the wake started.
		if c.wakeKickActive(lpCfg.ID, now) {
			minKick := smallestNonZero(c.pickSurplusSteps(now, lpCfg))
			if minKick > 0 && cmdW < minKick {
				slog.Info("loadpoint wake-kick", "lp", lpCfg.ID,
					"prev_cmd_w", cmdW, "kick_w", minKick)
				cmdW = minKick
				cmdReason = "wake_kick"
			}
		}
		if finishing && finishW == 0 {
			cmdW, cmdReason = 0, "vehicle_not_requesting"
			if state, ok := c.manager.State(lpCfg.ID); ok && state.GoalComplete {
				cmdReason = "vehicle_complete"
			}
		}
		// Fuse protection: applied LAST (after MPC budget, surplus
		// clamp, wake-kick) so all upstream sources see their nominal
		// wantW; only the actual ceiling we send to the wallbox is
		// reduced when the fuse demands it. Partial ramp-downs are
		// immediate; only "cap below min step → must pause" arms the
		// 5-min cooldown.
		var fuseReason string
		cmdW, fuseReason = c.applyFuseClampAndCooldown(now, lpCfg, cmdW)
		if fuseReason != "" {
			cmdReason = fuseReason
		}
		cmd["power_w"] = cmdW
		// Easee treats an unset phase_mode as 3p and then ignores a 1Φ step.
		// A schedule still overrides the surplus 1Φ lock, or a deadline
		// charge stays near 3.7 kW on a cloudy day.
		phaseMode := resolvePhaseMode(
			lpCfg.PhaseMode,
			sched.HasTarget(),
			c.surplusLockedTo1P(lpCfg.ID),
			surplusOn,
			c.dwellSelectedPhaseMode(lpCfg.ID),
		)
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
		site := c.siteFuse()
		if site.MaxAmps > 0 {
			cmd["max_amps_per_phase"] = site.MaxAmps
			cmd["site_phases"] = site.Phases()
		}
		if v := site.Voltage; v > 0 {
			cmd["voltage"] = v
		}
	}

	// A manual diagnostic may lower a site limit, but never replace the
	// installation's voltage, phase count or fuse with a larger offer.
	c.applyInstallationLimits(cmd)
	c.applyPerPhaseFuseClamp(lpCfg, cmd)
	if applyCurrentCeiling(cmd) {
		cmdReason = "fuse_limit"
	}
	// Tell the manager what was ordered, after every clamp has spoken.
	// The interruption latch reads this to keep a pause the box chose —
	// plan slot, Stop hold, surplus clamp — from ever reading as a
	// charge that failed.
	var offerW float64
	var haveOffer bool
	if w, ok := cmd["power_w"].(float64); ok {
		offerW, haveOffer = w, true
		c.manager.setCommandedForManual(lpCfg.ID, w, cmdReason, manualCommandUpdatedAt)
	}
	payload, err := json.Marshal(cmd)
	if err != nil {
		return
	}
	if c.send == nil {
		return
	}
	// Resume only a zero offer from this transport and plug session. A failed
	// optional resume keeps its retry state even after recording positive W.
	if haveOffer && offerW > 0 {
		c.resumeAfterZeroOffer(ctx, lpCfg, sample, offerW, now)
	}
	// The one command whose outcome decides whether core can actuate this
	// charger. A charger that answers every poll and refuses this holds
	// whatever current it last accepted, and the plan goes on counting the
	// EV load it is not drawing — the storage bug #800 fixed, one wire over.
	sendErr := c.sendOutcomeWithDeadline(ctx, lpCfg.DriverName, payload, func(err error) {
		if c.dispatchOutcome != nil {
			c.dispatchOutcome(lpCfg.DriverName, err, now)
		}
	})
	if sendErr != nil {
		slog.Warn("loadpoint dispatch", "lp", lpCfg.ID,
			"driver", lpCfg.DriverName, "err", sendErr)
	}
	if sendErr != nil {
		// Never start wake work from a command that did not complete. In
		// particular, a timeout may still be unwinding inside the registry;
		// its per-driver owner restores default before another command runs.
		return
	}
	if haveOffer && offerW <= 0 {
		c.resumeAfterZeroOffer(ctx, lpCfg, sample, 0, now)
	}
	if !c.driverCanDispatch(lpCfg.DriverName) {
		// The outcome callback can close Core's health gate synchronously.
		// Stop this tick here as well: charge_start and the wallbox cycle are
		// wake side effects of a dispatch that Core has just rejected.
		return
	}

	// Wake on Stopped or Disconnected, including a surplus pause at 0 W.
	// Otherwise the car never draws, so surplus never appears. The
	// bat-SoC unlock must not poke a sleeping car, so this is the
	// configured SurplusOnly flag, not the armed tick.
	c.maybeWakeVehicle(ctx, now, lpCfg, lpCfg.SurplusOnly, sample.RequestActive, selfWithheld, cmd)
}

func (c *Controller) maybeWakeVehicle(ctx context.Context, now time.Time, lpCfg Config, surplusOn, chargerRequesting, selfPaused bool, cmd map[string]any) {
	lpID := lpCfg.ID
	if c == nil || c.send == nil {
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
	var driver, state string
	var ok bool
	if c.vehicleStatus != nil {
		driver, state, ok = c.vehicleStatus(lpID)
	}
	if !ok || driver == "" {
		// No vehicle-API binding (e.g. a bare CTEK with no Tesla/cloud
		// vehicle driver). We can't read a vehicle charging state, but the
		// charger itself reports "not requesting current" (NCRQ) via
		// request_active. When a surplus loadpoint is being offered power
		// yet the charger sits in NCRQ from our own earlier sub-floor
		// pause, cycle the contactor so the vehicle renegotiates — there's
		// no vehicle driver for charge_start to land on, so the wallbox
		// cycle is the only lever. Throttled to once per cooldown.
		if c.shouldKickWallboxForResume(now, lpID, surplusOn, chargerRequesting, selfPaused, pw) {
			slog.Info("loadpoint wallbox-cycle: charger not requesting while surplus offered — renegotiating",
				"lp", lpID, "driver", lpCfg.DriverName)
			c.cycleWallbox(lpID, lpCfg.DriverName)
		}
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
	if err := c.sendVehicle(ctx, driver, payload); err != nil {
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
	c.cycleWallbox(lpID, lpCfg.DriverName)
}

// wallboxCycleGap is the dwell between ev_pause and ev_resume in a contactor
// cycle. 3 s is enough for a car to register the contactor open as a
// plug-cycle (shorter risks the car missing the transition; longer eats into
// the wake-kick window and prolongs grid import). A var, not a const, so tests
// can shrink it.
var wallboxCycleGap = 3 * time.Second

// cycleWallbox opens and re-closes the charger's contactor (ev_pause →
// ev_resume), which a car interprets as a plug-cycle and accepts as a fresh
// session boundary — the only software lever that drags a vehicle out of a
// "Stopped"/NCRQ state that rejects charge_start. Driver-agnostic: any EV
// charger driver implementing ev_pause / ev_resume gets it. Runs in a
// goroutine so the dispatch tick doesn't block on the pause→sleep→resume.
func (c *Controller) cycleWallbox(lpID, driverName string) {
	if driverName == "" || c.send == nil {
		return
	}
	gap := wallboxCycleGap
	cycleID := c.wallboxCycleSeq.Add(1)
	go func() {
		pauseCmd, _ := json.Marshal(map[string]any{"action": "ev_pause"})
		resumeCmd, _ := json.Marshal(map[string]any{"action": "ev_resume"})
		if !c.driverCanDispatch(driverName) {
			return
		}
		pauseErr := c.sendCycleWithDeadline(context.Background(), driverName, pauseCmd, cycleID)
		if pauseErr != nil {
			slog.Warn("loadpoint wallbox-cycle pause failed",
				"lp", lpID, "driver", driverName, "err", pauseErr)
			return
		}
		slog.Info("loadpoint wallbox-cycle: paused", "lp", lpID, "driver", driverName)
		time.Sleep(gap)
		if !c.driverCanDispatch(driverName) {
			return
		}
		resumeErr := c.sendCycleWithDeadline(context.Background(), driverName, resumeCmd, cycleID)
		if resumeErr != nil {
			slog.Warn("loadpoint wallbox-cycle resume failed",
				"lp", lpID, "driver", driverName, "err", resumeErr)
			return
		}
		slog.Info("loadpoint wallbox-cycle: resumed", "lp", lpID, "driver", driverName)
	}()
}

// shouldKickWallboxForResume decides whether to fire a contactor cycle to drag
// an offline charger (no vehicle-API binding) out of a self-induced NCRQ once
// PV surplus has recovered. Returns true only when we're a surplus loadpoint
// offering power (offeredW > 0) into a charger that isn't requesting current
// because WE paused it (selfPaused) — and at most once per vehicleWakeCooldown
// per loadpoint so a flapping resume can't storm the contactor.
func (c *Controller) shouldKickWallboxForResume(now time.Time, lpID string, surplusOn, chargerRequesting, selfPaused bool, offeredW float64) bool {
	if !surplusOn || !selfPaused || chargerRequesting || offeredW <= 0 {
		return false
	}
	c.wakeMu.Lock()
	defer c.wakeMu.Unlock()
	if c.wakeLast == nil {
		c.wakeLast = map[string]time.Time{}
		c.wakeKickUntil = map[string]time.Time{}
		c.wakeAttempts = map[string]int{}
	}
	if last := c.wakeLast[lpID]; !last.IsZero() && now.Sub(last) < vehicleWakeCooldown {
		return false
	}
	c.wakeLast[lpID] = now
	return true
}

// computeSurplusCmd clamps wantW to live surplus. The rolling average
// decides pause and resume. The magnitude tracks the instant value, or
// a falling cloud imports before the average catches up. 0 pauses.
//
// TODO: siteSurplusForEVW is one site budget. Two surplus loadpoints
// each take all of it and can import.
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
		// Reset the step memory so the next resume ramps up fresh under
		// the avg gate rather than from a stale pre-pause level.
		c.setSurplusStepW(lpCfg.ID, 0)
		return 0
	}
	// Setpoint magnitude: down-steps track INSTANT surplus (keeps the
	// no-import promise tight — on a dropping cloud front the EV must shed
	// load immediately or it leaks straight into grid import). Up-steps are
	// gated on the rolling AVERAGE.
	target := wantW
	if instant < target {
		target = instant
	}
	snapped := SnapChargeW(target, lpCfg.MinChargeW, lpCfg.MaxChargeW, steps3)

	// Asymmetric step smoothing (operator report 2026-05-30). surplusW counts
	// the home battery's current charge power as EV-available, so a single-
	// tick wobble (the battery briefly backing off, a cloud edge, the load
	// twitching) would ratchet the EV UP a step it can't hold — it collapses
	// the next tick, and the repeated multi-kW load swing whipsaws the home
	// battery's reactive PI into integrator windup, so the battery stops
	// delivering its planned discharge (EV ↔ battery limit cycle). Requiring
	// the smoothed average to ALSO clear the higher step lets the EV ramp up
	// only on a sustained rise. Down-steps stay instant (above), so the
	// no-import guarantee is unaffected.
	prev := c.getSurplusStepW(lpCfg.ID)
	if snapped > prev {
		avgTarget := wantW
		if avg < avgTarget {
			avgTarget = avg
		}
		avgSnapped := SnapChargeW(avgTarget, lpCfg.MinChargeW, lpCfg.MaxChargeW, steps3)
		if avgSnapped < snapped {
			snapped = avgSnapped
			if snapped < prev {
				snapped = prev // never force a down-step on an up-tick
			}
		}
	}
	c.setSurplusStepW(lpCfg.ID, snapped)
	return snapped
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

	// 3Φ-only charger: an operator who pinned "3p" is telling us the
	// wallbox can't fall back to single-phase (e.g. CTEK Chargestorm —
	// 3Φ, 6 A minimum, no phase-switch register). Never offer a 1Φ-
	// eligible step and never commit the day-long 1Φ lock; the charger
	// simply pauses below the 3Φ minimum and charges in 3Φ steps above
	// it. Without this the surplus 1Φ fallback hands such a charger a
	// ~1380 W offer it can only answer by writing 0 A → it never charges
	// on any day the PV forecast can't sustain 3Φ. Clear any stale lock
	// so flipping the config to "3p" takes effect immediately.
	if lpCfg.PhaseMode == "3p" {
		c.phaseLockMu.Lock()
		delete(c.phaseLocked1P, lpCfg.ID)
		delete(c.phaseLockedAt, lpCfg.ID)
		c.phaseLockMu.Unlock()
		return steps3
	}

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
// resolvePhaseMode picks the phase_mode sent to the driver.
// A schedule wins, or a deadline charge is stuck on the 1Φ lock.
// Then the day lock, then the 30 min dwell, then the operator pin.
func resolvePhaseMode(operatorMode string, scheduleActive, surplusLocked1P, surplusOn bool, dwell string) string {
	switch {
	case scheduleActive:
		if operatorMode == "" {
			return "auto"
		}
		return operatorMode
	case surplusLocked1P:
		// A 3Φ-only charger (operator pinned "3p") physically cannot
		// trickle at 1Φ, so the surplus 1Φ lock must never override it —
		// defends against a stale lock left from a previous "auto"/"1p"
		// config. pickSurplusSteps also refuses to set the lock for a
		// "3p" loadpoint, so this is belt-and-braces.
		if operatorMode == "3p" {
			return "3p"
		}
		return "1p"
	case surplusOn && (operatorMode == "" || operatorMode == "auto"):
		if dwell != "" {
			return dwell
		}
		if operatorMode == "" {
			return "auto"
		}
		return operatorMode
	default:
		return operatorMode
	}
}

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

func (c *Controller) getSurplusStepW(id string) float64 {
	c.surplusMu.Lock()
	defer c.surplusMu.Unlock()
	return c.surplusStepW[id]
}

func (c *Controller) setSurplusStepW(id string, w float64) {
	c.surplusMu.Lock()
	defer c.surplusMu.Unlock()
	if c.surplusStepW == nil {
		c.surplusStepW = map[string]float64{}
	}
	c.surplusStepW[id] = w
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
// The third return is true when the joint fuse allocator's cap, not
// the plan budget, bounded the result — the caller records that as the
// commanded reason so a fuse-starved 0 W never reads as "the plan
// chose 0" (#1009).
func (c *Controller) computeCommand(now time.Time, lpCfg Config, currentPowerW float64) (float64, bool, bool) {
	if c.plan == nil {
		return 0, false, false
	}
	d, ok := c.plan(now)
	if !ok {
		return 0, false, false
	}
	budgetWh, hasBudget := d.LoadpointEnergyWh[lpCfg.ID]
	if !hasBudget {
		return 0, false, false
	}
	remainingS := d.SlotEnd.Sub(now).Seconds()
	alreadyWh, unmeasuredS := c.energySince(lpCfg.ID, d.SlotStart, now)
	if durationS := d.SlotEnd.Sub(d.SlotStart).Seconds(); durationS > 0 {
		alreadyWh += budgetWh * unmeasuredS / durationS
	}
	remainingWh := budgetWh - alreadyWh
	wantW := EnergyBudgetToPowerW(remainingWh, remainingS)
	maxW := lpCfg.MaxChargeW
	if ceiling, ok := d.LoadpointMaxPowerW[lpCfg.ID]; ok {
		maxW = min(maxW, ceiling)
		if maxW <= 0 || remainingWh <= 1e-6 || remainingS <= 0 {
			return 0, true, false
		}
		// A duty plan commands its legal on-power until the Wh budget is spent.
		wantW = maxW
	}
	// Joint fuse allocator (dispatch.go) caps EV demand when battery + EV
	// would together bust the fuse. Honour it before snapping to the
	// charger's discrete steps so the snap chooses a level under the cap.
	fuseCapped := false
	if c.fuseEVMax != nil {
		if cap, ok := c.fuseEVMax(); ok && cap >= 0 && wantW > cap {
			wantW = cap
			fuseCapped = true
		}
	}
	// Clamp to the loadpoint's static MaxChargeW (configured cap; the
	// driver's per-phase fuse clamp is the ultimate safety stop).
	if fuseCapped {
		return floorChargeW(wantW, lpCfg.MinChargeW, maxW, lpCfg.AllowedStepsW), true, true
	}
	return SnapChargeW(wantW, lpCfg.MinChargeW, maxW, lpCfg.AllowedStepsW), true, false
}
