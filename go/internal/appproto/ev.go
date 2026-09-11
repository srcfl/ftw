package appproto

import (
	"math"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/units"
)

// The EV loadpoint operations: loadpoint.hold pins the charger to a fixed
// power now and loadpoint.boost lets the house battery push the car, each
// with its own way back — `clear` releases a hold, `cancel` withdraws a
// boost, the session's spelling of the DELETE the HTTP routes use. Both ops
// move energy, so onCmd holds them behind every gate site.mode.set passes
// through plus the dispatch block, and they reach the box through the same
// loadpoint controller the HTTP routes call. This file owns the argument
// shapes and the read-back; no control logic lives here.

// argNum reads a numeric argument. Args arrive as CBOR and decode by value,
// not by declared type — integers come back int64 or uint64 by sign, floats
// float64 — so the type switch is the honest way to ask "was this a number".
func argNum(args map[string]any, key string) (float64, bool) {
	switch v := args[key].(type) {
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float64:
		return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	}
	return 0, false
}

// argInt is argNum for values that must be whole. A fractional second or
// millisecond is a client bug worth refusing rather than rounding.
func argInt(args map[string]any, key string) (int64, bool) {
	f, ok := argNum(args, key)
	if !ok || f != math.Trunc(f) || f < math.MinInt64 || f >= math.MaxInt64 {
		return 0, false
	}
	return int64(f), true
}

// rejectArg refuses a command over one argument, naming it in machine form so
// the app can say which one without the box writing a sentence. The same
// shape setMode uses for a mode the box does not have.
func (h *Handler) rejectArg(cmd Cmd, arg string, value any) error {
	return h.sendCmdResult(CmdResult{
		CmdID: cmd.CmdID,
		State: CmdRejected,
		Error: &ErrorBody{
			Code:      ErrUnknownOp,
			Retryable: false,
			Args:      map[string]any{"op": cmd.Op, "arg": arg, "value": value},
		},
	})
}

// loadpointFor answers the two questions every loadpoint op starts with:
// does this box have loadpoints at all, and does the named one exist. The
// same questions the HTTP routes ask, in the same order. ok is false when a
// refusal has already been sent; err is whatever sending it returned.
func (h *Handler) loadpointFor(cmd Cmd) (Loadpoints, string, bool, error) {
	lp := h.cfg.Loadpoints
	if lp == nil {
		return nil, "", false, h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"subsystem": "loadpoints"},
			},
		})
	}
	id, _ := cmd.Args["id"].(string)
	if id == "" || !lp.Exists(id) {
		return nil, "", false, h.rejectArg(cmd, "id", id)
	}
	return lp, id, true, nil
}

// loadpointHold is the manual hold through the door: charge the car now at a
// fixed power, or with `clear` stop doing so. Argument names mirror the HTTP
// manual_hold body, plus the loadpoint id the route carries in its path.
func (h *Handler) loadpointHold(cmd Cmd, uptimeMs int64) error {
	lp, id, ok, err := h.loadpointFor(cmd)
	if !ok {
		return err
	}

	if clear, _ := cmd.Args["clear"].(bool); clear {
		return h.clearHold(lp, id, cmd, uptimeMs)
	}

	// The HTTP handler's own validation, mirrored value for value. power_w
	// absent means 0, a valid "pause" hold, exactly as an omitted JSON field
	// would; hold_s of 0 or absent is the persistent operator hold that only
	// clear or an unplug releases.
	powerW, powerOK := argNum(cmd.Args, "power_w")
	if _, present := cmd.Args["power_w"]; (present && !powerOK) || powerW < 0 {
		return h.rejectArg(cmd, "power_w", cmd.Args["power_w"])
	}
	for _, key := range []string{"phase_split_w", "voltage", "max_amps_per_phase"} {
		if value, present := cmd.Args[key]; present {
			n, ok := argNum(cmd.Args, key)
			if !ok || n < 0 {
				return h.rejectArg(cmd, key, value)
			}
		}
	}
	for _, key := range []string{"min_phase_hold_s", "site_phases"} {
		if value, present := cmd.Args[key]; present {
			n, ok := argInt(cmd.Args, key)
			if !ok || n < 0 || (key == "site_phases" && n != 0 && n != 1 && n != 3) {
				return h.rejectArg(cmd, key, value)
			}
		}
	}
	var holdS int64
	if _, present := cmd.Args["hold_s"]; present {
		v, ok := argInt(cmd.Args, "hold_s")
		if !ok || v < 0 {
			return h.rejectArg(cmd, "hold_s", cmd.Args["hold_s"])
		}
		holdS = v
	}
	if holdS > int64(loadpoint.MaxManualHold/time.Second) {
		return h.rejectArg(cmd, "hold_s", holdS)
	}
	phaseMode, phaseOK := cmd.Args["phase_mode"].(string)
	if _, present := cmd.Args["phase_mode"]; present && !phaseOK {
		return h.rejectArg(cmd, "phase_mode", cmd.Args["phase_mode"])
	}
	switch phaseMode {
	case "", "auto", "1p", "3p":
	default:
		return h.rejectArg(cmd, "phase_mode", phaseMode)
	}

	// The optional overrides fall through as zero values, which the
	// controller reads as "use the loadpoint's configuration and the wired
	// site fuse" — the fall-through that keeps a minimal hold under the
	// per-phase fuse clamp.
	phaseSplitW, _ := argNum(cmd.Args, "phase_split_w")
	minPhaseHoldS, _ := argInt(cmd.Args, "min_phase_hold_s")
	voltage, _ := argNum(cmd.Args, "voltage")
	maxAmpsPerPhase, _ := argNum(cmd.Args, "max_amps_per_phase")
	sitePhases, _ := argInt(cmd.Args, "site_phases")

	persistent := holdS == 0
	var expires time.Time
	if !persistent {
		expires = h.cfg.Clock.Now().Add(time.Duration(holdS) * time.Second)
	}
	hold := loadpoint.ManualHold{
		PowerW:          powerW,
		PhaseMode:       phaseMode,
		PhaseSplitW:     phaseSplitW,
		MinPhaseHoldS:   int(minPhaseHoldS),
		Voltage:         voltage,
		MaxAmpsPerPhase: maxAmpsPerPhase,
		SitePhases:      int(sitePhases),
		ExpiresAt:       expires,
		Persistent:      persistent,
	}

	if _, err := h.acceptCmd(cmd, uptimeMs); err != nil {
		return err
	}

	lp.Hold(id, hold)

	// Read back what the box now holds, never the echo of the request.
	observed, held := lp.ObservedHold(id, h.cfg.Clock.Now())
	readAtMs := h.cfg.Clock.UptimeMs()
	var res CmdResult
	switch {
	case !held:
		res = CmdResult{CmdID: cmd.CmdID, State: CmdUnconfirmed}
	case observed.PowerW != powerW:
		// Something else moved the hold between the write and the read.
		res = CmdResult{
			CmdID:    cmd.CmdID,
			State:    CmdSuperseded,
			Observed: &Observed{Value: observed.PowerW, Src: ObservedSrcCore, UptimeMs: readAtMs},
		}
	default:
		res = CmdResult{
			CmdID:    cmd.CmdID,
			State:    CmdApplied,
			Observed: &Observed{Value: observed.PowerW, Src: ObservedSrcCore, UptimeMs: readAtMs},
		}
	}
	return h.settleAndReport(cmd.CmdID, res)
}

// clearHold releases a hold the way the HTTP DELETE does: a separate call to
// ClearManualHold, never a zero setpoint — 0 W is itself a valid hold that
// pauses the charger, and a release must not be spelled the same way.
func (h *Handler) clearHold(lp Loadpoints, id string, cmd Cmd, uptimeMs int64) error {
	if _, err := h.acceptCmd(cmd, uptimeMs); err != nil {
		return err
	}

	lp.ClearHold(id)

	observed, held := lp.ObservedHold(id, h.cfg.Clock.Now())
	readAtMs := h.cfg.Clock.UptimeMs()
	var res CmdResult
	if held {
		// Something re-installed a hold between the release and the read.
		res = CmdResult{
			CmdID:    cmd.CmdID,
			State:    CmdSuperseded,
			Observed: &Observed{Value: observed.PowerW, Src: ObservedSrcCore, UptimeMs: readAtMs},
		}
	} else {
		res = CmdResult{
			CmdID:    cmd.CmdID,
			State:    CmdApplied,
			Observed: &Observed{Value: 0, Src: ObservedSrcCore, UptimeMs: readAtMs},
		}
	}
	return h.settleAndReport(cmd.CmdID, res)
}

// loadpointBoost is the battery boost through the door, or with `cancel` its
// withdrawal. Argument names mirror the HTTP battery_boost body plus the id;
// the lease is validated by the same function the HTTP route answers 400
// from, so the two doors cannot grow different rules.
func (h *Handler) loadpointBoost(cmd Cmd, uptimeMs int64) error {
	lp, id, ok, err := h.loadpointFor(cmd)
	if !ok {
		return err
	}

	if cancel, _ := cmd.Args["cancel"].(bool); cancel {
		return h.cancelBoost(lp, id, cmd, uptimeMs)
	}

	// Exactly one of duration_s and expires_at_ms, the HTTP body's own rule.
	// Wall clock for the absolute form, deliberately: a boost's end is a
	// future moment a person plans around, like a plan slot's start.
	durationS, _ := argInt(cmd.Args, "duration_s")
	expiresAtMs, _ := argInt(cmd.Args, "expires_at_ms")
	if (durationS > 0) == (expiresAtMs > 0) {
		return h.rejectArg(cmd, "duration_s", durationS)
	}

	now := h.cfg.Clock.Now()
	expires := time.UnixMilli(expiresAtMs)
	if durationS > 0 {
		expires = now.Add(time.Duration(durationS) * time.Second)
	}
	minSoC, _ := argNum(cmd.Args, "min_battery_soc_pct")
	evTarget, _ := argNum(cmd.Args, "ev_target_soc_pct")
	lease := loadpoint.BatteryBoostLease{
		StartedAt:     now,
		ExpiresAt:     expires,
		MinBatterySoC: units.ClampFraction(units.FractionFromLegacyPercent(minSoC)),
		EVTargetSoC:   units.ClampFraction(units.FractionFromLegacyPercent(evTarget)),
	}
	if departureAtMs, ok := argInt(cmd.Args, "departure_at_ms"); ok && departureAtMs > 0 {
		lease.DepartureAt = time.UnixMilli(departureAtMs)
	}
	if err := loadpoint.ValidateBatteryBoostLease(lease, now); err != nil {
		// The validator's reasons are prose and prose stays off this wire;
		// naming the lease is enough for the app to say the box refused the
		// shape of it.
		return h.rejectArg(cmd, "lease", nil)
	}

	if _, err := h.acceptCmd(cmd, uptimeMs); err != nil {
		return err
	}

	if err := lp.Boost(id, lease, now); err != nil {
		// Live state cannot safely honour the lease — the HTTP route's 409,
		// reported rather than swallowed, like control refusing a mode.
		h.log.Warn("battery boost refused by loadpoint control", "loadpoint", id, "err", err)
		return h.settleAndReport(cmd.CmdID, CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"op": cmd.Op},
			},
		})
	}

	// Read back the status the box now reports: 1 means the boost is
	// running, 0 means something stopped it between the write and the read.
	status := lp.ObservedBoost(id, h.cfg.Clock.Now())
	readAtMs := h.cfg.Clock.UptimeMs()
	res := CmdResult{
		CmdID:    cmd.CmdID,
		State:    CmdApplied,
		Observed: &Observed{Value: 1, Src: ObservedSrcCore, UptimeMs: readAtMs},
	}
	if !status.Active {
		res.State = CmdSuperseded
		res.Observed = &Observed{Value: 0, Src: ObservedSrcCore, UptimeMs: readAtMs}
	}
	return h.settleAndReport(cmd.CmdID, res)
}

// cancelBoost withdraws a boost the way the HTTP DELETE does: idempotent,
// and reported from the read-back like everything else.
func (h *Handler) cancelBoost(lp Loadpoints, id string, cmd Cmd, uptimeMs int64) error {
	if _, err := h.acceptCmd(cmd, uptimeMs); err != nil {
		return err
	}

	now := h.cfg.Clock.Now()
	lp.CancelBoost(id, now)

	status := lp.ObservedBoost(id, h.cfg.Clock.Now())
	readAtMs := h.cfg.Clock.UptimeMs()
	res := CmdResult{
		CmdID:    cmd.CmdID,
		State:    CmdApplied,
		Observed: &Observed{Value: 0, Src: ObservedSrcCore, UptimeMs: readAtMs},
	}
	if status.Active {
		res.State = CmdSuperseded
		res.Observed = &Observed{Value: 1, Src: ObservedSrcCore, UptimeMs: readAtMs}
	}
	return h.settleAndReport(cmd.CmdID, res)
}

// socTolerance is how far a read-back may sit from the level asked for and
// still be that level: half a permille, the finest unit the telemetry wire
// carries a state of charge in. The manager re-anchors by subtracting and
// re-adding the session's delivered energy, which is float arithmetic, and a
// result that called that "superseded" would be reporting rounding as a
// rival writer.
const socTolerance = 0.0005

// loadpointSoCSet is the operator's correction of the car's charge level
// through the door: the same re-anchor POST /api/loadpoints/{id}/soc does,
// refused the same way when no car is plugged in. `soc` is a fraction in
// [0,1] — the rule the rest of the box keeps; permille is a telemetry wire
// unit, not an argument shape — and the read-back is the same fraction.
func (h *Handler) loadpointSoCSet(cmd Cmd, uptimeMs int64) error {
	lp, id, ok, err := h.loadpointFor(cmd)
	if !ok {
		return err
	}

	soc, ok := argNum(cmd.Args, "soc")
	if !ok || soc < 0 || soc > 1 {
		return h.rejectArg(cmd, "soc", cmd.Args["soc"])
	}

	if _, err := h.acceptCmd(cmd, uptimeMs); err != nil {
		return err
	}

	if !lp.SetSoC(id, soc) {
		// No session to correct — the HTTP route's 409. Reported after the
		// ack, the way control refusing a boost is, and named so the app can
		// say "plug the car in" rather than "the box is down".
		return h.settleAndReport(cmd.CmdID, CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"op": cmd.Op, "reason": "unplugged"},
			},
		})
	}

	// Read back the level the box now holds, never the echo of the request.
	observed, known := lp.ObservedSoC(id)
	readAtMs := h.cfg.Clock.UptimeMs()
	var res CmdResult
	switch {
	case !known:
		res = CmdResult{CmdID: cmd.CmdID, State: CmdUnconfirmed}
	case math.Abs(observed-soc) > socTolerance:
		// Something else re-anchored the level between the write and the
		// read — a vehicle reading, another operator.
		res = CmdResult{
			CmdID:    cmd.CmdID,
			State:    CmdSuperseded,
			Observed: &Observed{Value: observed, Src: ObservedSrcCore, UptimeMs: readAtMs},
		}
	default:
		res = CmdResult{
			CmdID:    cmd.CmdID,
			State:    CmdApplied,
			Observed: &Observed{Value: observed, Src: ObservedSrcCore, UptimeMs: readAtMs},
		}
	}
	return h.settleAndReport(cmd.CmdID, res)
}

// loadpointSurplusOnlySet turns PV-only charging on or off: the surplus_only
// field of POST /api/loadpoints/{id}/target, and only that field. The port
// carries the replan the HTTP route forces when the flag turns off, so the
// plan pushed after the result already allows the grid. The read-back is 1
// for on and 0 for off, the boost's convention for a flag.
func (h *Handler) loadpointSurplusOnlySet(cmd Cmd, uptimeMs int64) error {
	lp, id, ok, err := h.loadpointFor(cmd)
	if !ok {
		return err
	}

	want, ok := cmd.Args["surplus_only"].(bool)
	if !ok {
		return h.rejectArg(cmd, "surplus_only", cmd.Args["surplus_only"])
	}

	if _, err := h.acceptCmd(cmd, uptimeMs); err != nil {
		return err
	}

	if _, ok := lp.SetSurplusOnly(id, want); !ok {
		// The loadpoint disappeared or storage rejected the change. Keep
		// the previous choice and let the app offer a retry.
		return h.settleAndReport(cmd.CmdID, CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"op": cmd.Op},
			},
		})
	}

	observed, known := lp.ObservedSurplusOnly(id)
	readAtMs := h.cfg.Clock.UptimeMs()
	var res CmdResult
	switch {
	case !known:
		res = CmdResult{CmdID: cmd.CmdID, State: CmdUnconfirmed}
	case observed != want:
		res = CmdResult{
			CmdID:    cmd.CmdID,
			State:    CmdSuperseded,
			Observed: &Observed{Value: flagValue(observed), Src: ObservedSrcCore, UptimeMs: readAtMs},
		}
	default:
		res = CmdResult{
			CmdID:    cmd.CmdID,
			State:    CmdApplied,
			Observed: &Observed{Value: flagValue(observed), Src: ObservedSrcCore, UptimeMs: readAtMs},
		}
	}
	return h.settleAndReport(cmd.CmdID, res)
}

// flagValue is a flag as an observed value: 1 on, 0 off.
func flagValue(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
