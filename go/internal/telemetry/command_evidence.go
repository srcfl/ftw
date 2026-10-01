package telemetry

import (
	"encoding/json"
	"math"
	"time"
)

// CommandEvidence records a driver call, not proof that hardware obeyed it.
// Keep one command per device function so a hybrid's PV cap cannot replace
// its battery command. The registry clears these records on driver restart.
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
	PowerMatchSince       time.Time
	PowerObservedSince    time.Time
	LastObservation       time.Time
	LastMeasuredAt        time.Time
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
	}
	if json.Unmarshal(payload, &request) != nil {
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
	if ok && old.Result != "released" {
		c.LastMeasuredAt = old.LastMeasuredAt
	}
	// Retuning the same action continues one measurement record. A material
	// step, a failed or unknown call, or a return to device control needs new
	// proof against the new target.
	if ok && old.Action == c.Action && (old.Result == "accepted" || old.Result == "pending") {
		c.Recent, c.MaxAge = append([]CommandPoint(nil), old.Recent...), old.MaxAge
		if !materialStep(old.PowerW, c.PowerW) {
			c.Since, c.Baseline, c.StepAfter, c.StepAfterAt = old.Since, old.Baseline, old.StepAfter, old.StepAfterAt
			c.ReadbackMismatchSince, c.PowerMismatchSince, c.PowerMatchSince = old.ReadbackMismatchSince, old.PowerMismatchSince, old.PowerMatchSince
			c.PowerObservedSince, c.LastObservation, c.LastGapW = old.PowerObservedSince, old.LastObservation, old.LastGapW
		}
	}
	if c.PowerW != nil {
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
	for k, c := range s.commands {
		if c.Driver == driver {
			delete(s.commands, k)
		}
	}
}

// EndCommandControl invalidates comparisons with an earlier command when the
// actor returns the device to its own control. Keep failed attempts visible.
func (s *Store) EndCommandControl(driver string, failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, c := range s.commands {
		if c.Driver != driver {
			continue
		}
		if failed {
			c.Result = "default_failed"
		} else if c.Result == "accepted" || c.Result == "pending" || c.Result == "default_failed" {
			c.Result = "released"
		}
		c.ReadbackMismatchSince, c.PowerMismatchSince, c.PowerMatchSince = time.Time{}, time.Time{}, time.Time{}
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
	if !ok || c.Result != "accepted" || c.PowerW == nil {
		return
	}
	var d struct {
		SetpointW *float64 `json:"setpoint_w"`
	}
	if json.Unmarshal(data, &d) != nil {
		c.ReadbackMismatchSince, c.PowerMismatchSince, c.PowerMatchSince = time.Time{}, time.Time{}, time.Time{}
		c.PowerObservedSince, c.LastObservation = time.Time{}, time.Time{}
		s.commands[k] = c
		return
	}
	c.MaxAge = controlPowerDeclaredMaxAge(data)
	if now.Sub(c.LastObservation) > max(time.Minute, c.MaxAge) {
		c.ReadbackMismatchSince, c.PowerMismatchSince, c.PowerMatchSince = time.Time{}, time.Time{}, time.Time{}
		c.PowerObservedSince = time.Time{}
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
	power, observedAt := observation.PowerW, observation.At
	// A cached sample from before the run cannot show its response. Repeated
	// delivery of one source sample also cannot extend a measured time window.
	if !fresh || !finite(power) || observedAt.Before(c.Since) {
		c.PowerMismatchSince, c.PowerMatchSince, c.LastObservation = time.Time{}, time.Time{}, time.Time{}
		c.PowerObservedSince, c.LastGapW = time.Time{}, nil
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
