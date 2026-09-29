package telemetry

import (
	"encoding/json"
	"math"
	"time"
)

// CommandEvidence records a driver call, not proof that hardware obeyed it.
// Keep one command per device function so a hybrid's PV cap cannot replace
// its battery command. The registry clears these records on driver restart.
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
	LastObservation       time.Time
	LastVerifiedAt        time.Time
	Baseline              map[string]ControlBaseline
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
	if old, ok := s.commands[k]; ok && old.Result != "released" {
		c.LastVerifiedAt = old.LastVerifiedAt
	}
	if old, ok := s.commands[k]; ok && old.Result == "accepted" && old.Action == c.Action && samePower(old.PowerW, c.PowerW) {
		c.Since = old.Since
		c.ReadbackMismatchSince = old.ReadbackMismatchSince
		c.PowerMismatchSince = old.PowerMismatchSince
		c.PowerMatchSince = old.PowerMatchSince
		c.LastObservation = old.LastObservation
		c.Baseline = old.Baseline
	}
	if c.Baseline == nil {
		c.Baseline = s.controlBaseline(now)
	}
	s.commands[k] = c
	c.Baseline = nil // the completion token never owns the stored baseline
	if c.PowerW != nil {
		v := *c.PowerW
		c.PowerW = &v
	}
	return c
}

func samePower(a, b *float64) bool {
	return a == nil && b == nil || a != nil && b != nil && math.Abs(*a-*b) <= math.Max(100, math.Abs(*a)*0.05)
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

func (s *Store) CommandEvidence(driver, kind string) (CommandEvidence, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.commands[driver+":"+kind]
	if c.Baseline != nil {
		copied := make(map[string]ControlBaseline, len(c.Baseline))
		for k, v := range c.Baseline {
			v.Points = append([]ControlObservation(nil), v.Points...)
			copied[k] = v
		}
		c.Baseline = copied
	}
	if c.PowerW != nil {
		v := *c.PowerW
		c.PowerW = &v
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
		if !failed {
			c.LastVerifiedAt = time.Time{}
		}
		s.commands[k] = c
	}
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
		s.commands[k] = c
		return
	}
	if now.Sub(c.LastObservation) > time.Minute {
		c.ReadbackMismatchSince, c.PowerMismatchSince, c.PowerMatchSince = time.Time{}, time.Time{}, time.Time{}
	}
	mark := func(since *time.Time, mismatch bool, at time.Time) {
		if !mismatch {
			*since = time.Time{}
		} else if since.IsZero() {
			*since = at
		}
	}
	readbackGap := 0.0
	if d.SetpointW != nil {
		readbackGap = math.Abs(*d.SetpointW - *c.PowerW)
		if kind == DerPV {
			readbackGap = math.Abs(math.Abs(*d.SetpointW) - math.Abs(*c.PowerW))
		}
	}
	mark(&c.ReadbackMismatchSince, d.SetpointW != nil && finite(*d.SetpointW) && readbackGap > math.Max(100, math.Abs(*c.PowerW)*0.05), now)
	observation, fresh := ControlPowerObservation(power, data, now)
	power, observedAt := observation.PowerW, observation.At
	// A cached sample from before the command cannot show its response. Repeated
	// delivery of one source sample also cannot extend a measured time window.
	if !fresh || !finite(power) || observedAt.Before(c.Since) {
		c.PowerMismatchSince, c.PowerMatchSince, c.LastObservation = time.Time{}, time.Time{}, time.Time{}
		s.commands[k] = c
		return
	}
	if !observedAt.After(c.LastObservation) {
		s.commands[k] = c
		return
	}
	c.LastObservation = observedAt
	gap := math.Abs(power - *c.PowerW)
	if kind == DerPV {
		gap = math.Max(0, math.Abs(power)-math.Abs(*c.PowerW))
	}
	mark(&c.PowerMismatchSince, gap > ControlToleranceW(*c.PowerW), observedAt)
	mark(&c.PowerMatchSince, PowerFollowsCommand(c.Kind, *c.PowerW, power), observedAt)
	if !c.PowerMatchSince.IsZero() && observedAt.Sub(c.PowerMatchSince) >= 10*time.Second && now.Sub(observedAt) <= 10*time.Second {
		c.LastVerifiedAt = observedAt
	}
	s.commands[k] = c
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
