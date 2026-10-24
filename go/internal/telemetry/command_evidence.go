package telemetry

import (
	"encoding/json"
	"math"
	"time"
)

// CommandEvidence records a driver call, not proof that hardware obeyed it.
// Keep one command per device function so a hybrid's PV cap cannot replace
// its battery command. A restart clears response proof; outstanding battery
// orders retain only their small unconfirmed bound until raw power replaces it.
//
// A device keeps one measurement record while FTW sends it the same action.
// Modes that retune the target every tick must not restart the proof on each
// command, so a reading is compared with every command that could still be in
// force during the device's response delay. Only a material step starts a new
// run with its own response wait and site comparison.
type CommandEvidence struct {
	Driver                string
	Kind                  string
	Action                string
	PowerW                *float64
	At                    time.Time
	Since                 time.Time
	Result                string
	ReadbackMismatchSince time.Time
	PowerMismatchSince    time.Time
	// PowerOverSince is the start of the current run of readings above
	// every command in force, a part of PowerMismatchSince's run.
	PowerOverSince     time.Time
	PowerMatchSince    time.Time
	PowerObservedSince time.Time
	LastObservation    time.Time
	LastMeasuredAt     time.Time
	// NoPower is an observed EV stop during uninterrupted positive orders.
	// Retuning the current cannot restart this clock; a pause, failed command,
	// missing measurement or observed power ends it.
	NoPowerSince, NoPowerAt time.Time
	// LastGapW is the latest fresh reading's distance from the commands in
	// force during the response delay. Nil until such a reading arrives.
	LastGapW *float64
	// MaxAge is the power freshness the source declares; zero when it declares none.
	MaxAge   time.Duration
	Recent   []CommandPoint
	Baseline map[string]ControlBaseline
	// StepAfter freezes the completed window after a material step, so a later
	// household load cannot rewrite the site comparison for that step.
	StepAfter   map[string]ControlBaseline
	StepAfterAt time.Time
	// A battery order can reach hardware after a newer, lower order. Keep
	// its highest signed target even when Recent evicts it. Only a fresh
	// source observation after the lower run's response delay can reduce it.
	// This is command evidence, not a second power history or a live reading.
	batteryUpperW     float64
	batteryUpperKnown bool
	batteryLowerW     float64
	batteryLowerSince time.Time
	batteryReleased   bool
	batteryOrderAt    time.Time
	cleared           bool
}

// CommandPoint is one command FTW sent, kept for the response-delay check.
type CommandPoint struct {
	PowerW float64
	At     time.Time
}

func commandKind(action string) string {
	switch action {
	case "battery":
		return "battery"
	case "ev_set_current", "ev_pause", "ev_resume", "ev_start":
		return "ev"
	case "curtail", "curtail_disable":
		return "pv"
	case "v2x_set_power":
		return "v2x_charger"
	default:
		return ""
	}
}

// ResponseDelay bounds how long a device may still show an earlier command.
// Cloud chargers often report a new current a minute or more after the call.
func ResponseDelay(kind string) time.Duration {
	if kind == "ev" {
		return 2 * time.Minute
	}
	return 15 * time.Second
}

// A material step needs its own response wait and site comparison. Smaller
// retuning continues the current run.
func materialStep(old, next *float64) bool {
	if old == nil || next == nil {
		return old != nil || next != nil
	}
	return math.Abs(*next-*old) >= math.Max(500, ControlToleranceW(*old))
}

func (s *Store) BeginCommand(driver string, payload []byte, now time.Time) CommandEvidence {
	var request struct {
		Action string   `json:"action"`
		PowerW *float64 `json:"power_w"`
		// ExpectedW is what the device should draw when it differs from
		// the order: a one-phase car takes a third of a three-phase one.
		ExpectedW *float64 `json:"expected_w"`
	}
	if json.Unmarshal(payload, &request) != nil {
		return CommandEvidence{}
	}
	if request.ExpectedW != nil && finite(*request.ExpectedW) && *request.ExpectedW >= 0 {
		request.PowerW = request.ExpectedW
	}
	// Resume changes permission to draw, not the current limit. Keep the
	// accepted limit and its measured response while this optional call runs.
	if request.Action == "ev_resume" && request.PowerW == nil {
		return CommandEvidence{}
	}
	if request.Action == "ev_pause" {
		zero := 0.0
		request.PowerW = &zero
	}
	kind := commandKind(request.Action)
	if kind == "" || (request.PowerW != nil && !finite(*request.PowerW)) {
		return CommandEvidence{}
	}
	c := CommandEvidence{Driver: driver, Kind: kind, Action: request.Action, PowerW: request.PowerW, At: now, Since: now, Result: "pending"}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commands == nil {
		s.commands = map[string]CommandEvidence{}
	}
	k := driver + ":" + kind
	old, ok := s.commands[k]
	if kind == "battery" {
		c.batteryOrderAt = now
		c.batteryUpperW, c.batteryUpperKnown = old.batteryUpperW, old.batteryUpperKnown
		c.batteryLowerW, c.batteryLowerSince = old.batteryLowerW, old.batteryLowerSince
		if c.PowerW != nil {
			if !c.batteryUpperKnown || *c.PowerW >= c.batteryUpperW {
				c.batteryUpperW, c.batteryUpperKnown, c.batteryLowerSince = *c.PowerW, true, time.Time{}
			} else if c.batteryLowerSince.IsZero() || *c.PowerW > c.batteryLowerW {
				c.batteryLowerW, c.batteryLowerSince = *c.PowerW, now
			}
		}
	}
	if ok && old.Result != "released" {
		c.LastMeasuredAt = old.LastMeasuredAt
	}
	// Retuning the same action continues one measurement record. A material
	// step, a failed or unknown call, or a return to device control needs new
	// proof against the new target.
	if ok && old.Action == c.Action && (old.Result == "accepted" || old.Result == "pending") {
		if kind == "ev" && old.PowerW != nil && c.PowerW != nil && *old.PowerW > 100 && *c.PowerW > 100 {
			c.NoPowerSince, c.NoPowerAt = old.NoPowerSince, old.NoPowerAt
		}
		c.Recent, c.MaxAge = append([]CommandPoint(nil), old.Recent...), old.MaxAge
		if !materialStep(old.PowerW, c.PowerW) {
			c.Since, c.Baseline, c.StepAfter, c.StepAfterAt = old.Since, old.Baseline, old.StepAfter, old.StepAfterAt
			c.ReadbackMismatchSince, c.PowerMismatchSince, c.PowerMatchSince = old.ReadbackMismatchSince, old.PowerMismatchSince, old.PowerMatchSince
			c.PowerOverSince = old.PowerOverSince
			c.PowerObservedSince, c.LastObservation, c.LastGapW = old.PowerObservedSince, old.LastObservation, old.LastGapW
		}
	}
	// A repeat of the last order is the same order: keeping it would push
	// the earlier ones out of Recent long before their response time ends.
	if n := len(c.Recent); c.PowerW != nil && (n == 0 || c.Recent[n-1].PowerW != *c.PowerW) {
		c.Recent = append(c.Recent, CommandPoint{PowerW: *c.PowerW, At: now})
	}
	if len(c.Recent) > 8 {
		c.Recent = c.Recent[len(c.Recent)-8:]
	}
	if c.Baseline == nil {
		c.Baseline = s.controlBaseline(now)
	}
	s.commands[k] = c
	// The completion token never owns the stored windows or history.
	c.Baseline, c.StepAfter, c.Recent = nil, nil, nil
	if c.PowerW != nil {
		v := *c.PowerW
		c.PowerW = &v
	}
	return c
}

// CompleteCommand never retains raw driver errors: they may contain vendor
// URLs or credentials. Detailed errors remain in the existing private logs.
func (s *Store) CompleteCommand(c CommandEvidence, result string) {
	if c.Kind == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := c.Driver + ":" + c.Kind
	if latest, ok := s.commands[k]; ok && latest.At.Equal(c.At) {
		latest.Result = result
		s.commands[k] = latest
	}
}

func copyWindows(in map[string]ControlBaseline) map[string]ControlBaseline {
	if in == nil {
		return nil
	}
	out := make(map[string]ControlBaseline, len(in))
	for k, v := range in {
		v.Points = append([]ControlObservation(nil), v.Points...)
		out[k] = v
	}
	return out
}

func (s *Store) CommandEvidence(driver, kind string) (CommandEvidence, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.commands[driver+":"+kind]
	if c.cleared {
		return CommandEvidence{}, false
	}
	c.Baseline, c.StepAfter = copyWindows(c.Baseline), copyWindows(c.StepAfter)
	c.Recent = append([]CommandPoint(nil), c.Recent...)
	for _, p := range []**float64{&c.PowerW, &c.LastGapW} {
		if *p != nil {
			v := **p
			*p = &v
		}
	}
	return c, ok
}

func (s *Store) ClearCommandEvidence(driver string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearCommandEvidenceLocked(driver, time.Now())
}

// BatteryCommandUpperW is the highest battery target not yet replaced by
// source-time power evidence. Failed calls and driver resets do not prove
// that an order failed to reach hardware. A newer computed offer also needs
// room while it waits for the driver actor; an earlier default/reset retires
// old computed offers. Offers are not retained as measurements or commands.
func (s *Store) BatteryCommandUpperW(driver string, offers ...CommandPoint) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.commands[driver+":battery"]
	upper := c.batteryUpperW
	for _, offer := range offers {
		if c.batteryUpperKnown && offer.At.After(c.batteryOrderAt) {
			upper = math.Max(upper, offer.PowerW)
		}
	}
	return upper, c.batteryUpperKnown
}

// Called under mu. Drop response/UI evidence on restart, but retain the
// small outstanding order bound until a new raw observation clears it.
func (s *Store) clearCommandEvidenceLocked(driver string, now time.Time) {
	for k, c := range s.commands {
		if c.Driver == driver {
			if c.Kind == "battery" && c.batteryUpperKnown {
				since := c.batteryLowerSince
				if !c.batteryReleased || since.IsZero() {
					since = now
				}
				s.commands[k] = CommandEvidence{Driver: driver, Kind: "battery", Result: "released", cleared: true,
					batteryUpperW: math.Max(0, c.batteryUpperW), batteryUpperKnown: true, batteryLowerSince: since, batteryReleased: true, batteryOrderAt: now}
			} else {
				delete(s.commands, k)
			}
		}
	}
}

// EndCommandControl invalidates comparisons with an earlier command when the
// actor returns the device to its own control. Keep failed attempts visible.
func (s *Store) EndCommandControl(driver string, failed bool) {
	s.endCommandControlAt(driver, failed, time.Now())
}

func (s *Store) endCommandControlAt(driver string, failed bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, c := range s.commands {
		if c.Driver != driver {
			continue
		}
		if c.Kind == "battery" && c.batteryUpperKnown {
			// Returning to autonomous control can also stop discharge. A
			// successful default call is still not a measured response.
			c.batteryUpperW = math.Max(0, c.batteryUpperW)
			if !c.batteryReleased || c.batteryLowerSince.IsZero() {
				c.batteryLowerSince = now
			}
			c.batteryLowerW = 0
			c.batteryReleased = true
			c.batteryOrderAt = now
		}
		if failed {
			c.Result = "default_failed"
		} else if c.Result == "accepted" || c.Result == "pending" || c.Result == "default_failed" {
			c.Result = "released"
		}
		c.ReadbackMismatchSince, c.PowerMismatchSince, c.PowerMatchSince = time.Time{}, time.Time{}, time.Time{}
		c.PowerOverSince = time.Time{}
		c.PowerObservedSince, c.LastObservation, c.LastGapW = time.Time{}, time.Time{}, nil
		c.Recent, c.StepAfter, c.StepAfterAt = nil, nil, time.Time{}
		if !failed {
			c.LastMeasuredAt = time.Time{}
		}
		s.commands[k] = c
	}
}

// commandGap is a reading's distance from the commands that could still be in
// force at its source time: the last command sent before the response delay
// and every later one. A PV command is a ceiling, so lower output is no gap.
func (c CommandEvidence) commandGap(power float64, at time.Time) float64 {
	cutoff := at.Add(-ResponseDelay(c.Kind))
	lo, hi := math.Inf(1), math.Inf(-1)
	var inForce *CommandPoint
	for i := range c.Recent {
		p := c.Recent[i]
		if p.At.After(at) {
			continue
		}
		if !p.At.After(cutoff) {
			inForce = &c.Recent[i]
			continue
		}
		lo, hi = math.Min(lo, p.PowerW), math.Max(hi, p.PowerW)
	}
	if inForce != nil {
		lo, hi = math.Min(lo, inForce.PowerW), math.Max(hi, inForce.PowerW)
	}
	if lo > hi {
		return math.NaN()
	}
	if c.Kind == "pv" {
		return math.Max(0, math.Abs(power)-math.Max(math.Abs(lo), math.Abs(hi)))
	}
	if power < lo {
		return lo - power
	}
	if power > hi {
		return power - hi
	}
	return 0
}

// Called under mu only when telemetry arrives. UI polling never starts or
// advances a mismatch timer. A gap in readings requires new evidence.
func (s *Store) observeCommand(driver string, kind DerType, power float64, data json.RawMessage, now time.Time) {
	k := driver + ":" + kind.String()
	c, ok := s.commands[k]
	if !ok {
		return
	}
	if kind == DerBattery && c.batteryUpperKnown && !c.batteryLowerSince.IsZero() {
		observation, fresh := ControlPowerObservation(power, data, now)
		if fresh && !observation.Seen().Before(c.batteryLowerSince.Add(ResponseDelay("battery"))) &&
			!observation.Seen().After(now) && now.Sub(observation.Seen()) <= ControlWindowDuration {
			// The lower run's maximum also covers intermediate orders that
			// may not yet have arrived, even when Recent no longer has them.
			upper := math.Max(c.batteryLowerW, observation.PowerW)
			if c.batteryReleased {
				upper = math.Max(0, upper)
			} else if c.PowerW != nil {
				upper = math.Max(*c.PowerW, upper)
			}
			if upper < c.batteryUpperW {
				c.batteryUpperW = upper
				if upper <= c.batteryLowerW {
					c.batteryLowerSince = time.Time{}
				}
			}
		}
		s.commands[k] = c
	}
	if c.Result != "accepted" || c.PowerW == nil || c.cleared {
		return
	}
	var d struct {
		SetpointW *float64 `json:"setpoint_w"`
	}
	if json.Unmarshal(data, &d) != nil {
		c.NoPowerSince, c.NoPowerAt = time.Time{}, time.Time{}
		c.ReadbackMismatchSince, c.PowerMismatchSince, c.PowerMatchSince = time.Time{}, time.Time{}, time.Time{}
		c.PowerOverSince = time.Time{}
		c.PowerObservedSince, c.LastObservation = time.Time{}, time.Time{}
		s.commands[k] = c
		return
	}
	c.MaxAge = DeclaredPowerMaxAge(data)
	if now.Sub(c.LastObservation) > max(time.Minute, c.MaxAge) {
		c.ReadbackMismatchSince, c.PowerMismatchSince, c.PowerMatchSince = time.Time{}, time.Time{}, time.Time{}
		c.PowerOverSince, c.PowerObservedSince = time.Time{}, time.Time{}
	}
	mark := func(since *time.Time, mismatch bool, at time.Time) {
		if !mismatch {
			*since = time.Time{}
		} else if since.IsZero() {
			*since = at
		}
	}
	readbackMismatch := false
	if d.SetpointW != nil && finite(*d.SetpointW) {
		setpoint := *d.SetpointW
		if kind == DerPV {
			setpoint = -math.Abs(setpoint)
		}
		gap := c.commandGap(setpoint, now)
		readbackMismatch = math.IsNaN(gap) || gap > math.Max(100, math.Abs(*c.PowerW)*0.05)
		if kind == DerPV {
			readbackMismatch = math.Abs(math.Abs(*d.SetpointW)-math.Abs(*c.PowerW)) > math.Max(100, math.Abs(*c.PowerW)*0.05)
		}
	}
	mark(&c.ReadbackMismatchSince, readbackMismatch, now)
	observation, fresh := ControlPowerObservation(power, data, now)
	// A change-only source that confirms an unchanged sample states the power
	// at the confirmation time; an unconfirmed repeat adds nothing new.
	power, observedAt := observation.PowerW, observation.Seen()
	if kind == DerEV {
		switch {
		case !fresh || !finite(power) || *c.PowerW <= 100 || math.Abs(power) >= 100:
			c.NoPowerSince, c.NoPowerAt = time.Time{}, time.Time{}
		case observedAt.After(c.NoPowerAt) && (!c.NoPowerSince.IsZero() || !observedAt.Before(c.Since)):
			if c.NoPowerSince.IsZero() || observedAt.Sub(c.NoPowerAt) > max(time.Minute, c.MaxAge) {
				c.NoPowerSince = observedAt
			}
			c.NoPowerAt = observedAt
		}
	}
	// A cached sample from before the run cannot show its response. Repeated
	// delivery of one source sample also cannot extend a measured time window.
	if !fresh || !finite(power) || observedAt.Before(c.Since) {
		c.PowerMismatchSince, c.PowerMatchSince, c.LastObservation = time.Time{}, time.Time{}, time.Time{}
		c.PowerOverSince, c.PowerObservedSince, c.LastGapW = time.Time{}, time.Time{}, nil
		s.commands[k] = c
		return
	}
	if !observedAt.After(c.LastObservation) {
		s.commands[k] = c
		return
	}
	c.LastObservation = observedAt
	mark(&c.PowerObservedSince, true, observedAt)
	gap := c.commandGap(power, observedAt)
	c.LastGapW = nil
	if finite(gap) {
		c.LastGapW = &gap
	}
	tolerance := ControlToleranceW(*c.PowerW)
	mismatch := !finite(gap) || gap > tolerance
	mark(&c.PowerMismatchSince, mismatch, observedAt)
	// Above the latest command and off every one in force is above them all.
	mark(&c.PowerOverSince, mismatch && kind != DerPV && power > *c.PowerW, observedAt)
	follows := !mismatch
	if kind == DerPV {
		// Weak sun alone cannot prove that a ceiling took effect.
		follows = PowerFollowsCommand(c.Kind, *c.PowerW, power)
	}
	mark(&c.PowerMatchSince, follows, observedAt)
	if c.HasMeasuredPower(now) {
		c.LastMeasuredAt = observedAt
	}
	s.commands[k] = c
}

// HasMeasuredPower confirms a window of distinct, fresh device readings, even
// when they show a shortfall, no response or the wrong direction. Whether the
// device meets the command is a separate verdict; a setpoint echo is not power.
// A slow source counts as fresh for as long as it declares.
func (c CommandEvidence) HasMeasuredPower(now time.Time) bool {
	return !c.PowerObservedSince.IsZero() &&
		c.LastObservation.Sub(c.PowerObservedSince) >= 10*time.Second &&
		!c.LastObservation.After(now) && now.Sub(c.LastObservation) <= max(10*time.Second, c.MaxAge)
}

// ControlToleranceW is the response tolerance, not a device rating or safety limit.
func ControlToleranceW(sent float64) float64 { return math.Max(100, math.Abs(sent)*0.1) }

// PowerFollowsCommand requires a measured target response. PV below its ceiling
// is not a failure, but weak sun alone cannot prove that curtailment took effect.
func PowerFollowsCommand(kind string, sent, actual float64) bool {
	if !finite(sent) || !finite(actual) {
		return false
	}
	if kind == "pv" {
		return math.Abs(math.Abs(actual)-math.Abs(sent)) <= ControlToleranceW(sent)
	}
	return math.Abs(actual-sent) <= ControlToleranceW(sent)
}
