package notifications

import (
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// ControlState is one device function in the box's answer to "Are we in
// control?", as GET /api/status serves it in control_feedback.
type ControlState struct {
	Driver string
	Kind   string
	Status string
}

// ControlReader answers what the box's control feedback says now. main.go
// wires it to the API's own builder, so the rule and the status document
// can never disagree.
type ControlReader = func(now time.Time) []ControlState

// controlCheckEvery is how often the control rule asks. The answer is
// worked out on demand, and the rule waits minutes before it speaks, so a
// HealthTick every few seconds would only repeat the work.
const controlCheckEvery = 30 * time.Second

// SetControlReader wires the control feedback the control.not_following rule
// reads. nil disables the rule.
func (s *Service) SetControlReader(r ControlReader) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.controlReader = r
	s.mu.Unlock()
}

// evaluateControl fires control.not_following for each device function the
// box has said is not following for the rule's threshold, once until it
// follows again. A box whose stored rules predate the kind takes its
// default, as FTW Cloud's own notices do.
func (s *Service) evaluateControl(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.cfg == nil || !s.cfg.Enabled || s.controlReader == nil || now.Sub(s.controlCheckedAt) < controlCheckEvery {
		s.mu.Unlock()
		return
	}
	s.controlCheckedAt = now
	rule, ok := findRule(s.cfg.Events, PushControlNotFollowing)
	if !ok {
		rule, _ = findRule(DefaultRules(), PushControlNotFollowing)
	}
	if !rule.Enabled {
		clear(s.controlFirstAt)
		s.mu.Unlock()
		return
	}
	reader := s.controlReader
	prio := rule.Priority
	if prio == 0 {
		prio = s.cfg.DefaultPriority
	}
	s.mu.Unlock()

	// Read outside the lock: the builder takes the controller's own.
	troubled := map[string]ControlState{}
	for _, st := range reader(now) {
		if st.Status == "not_following" {
			troubled[st.Driver+"|"+st.Kind] = st
		}
	}

	threshold := time.Duration(rule.ThresholdS) * time.Second
	if threshold == 0 {
		threshold = time.Duration(DefaultThresholdS) * time.Second
	}
	var pending []ControlState
	s.mu.Lock()
	for key := range s.controlFirstAt {
		if _, still := troubled[key]; !still {
			delete(s.controlFirstAt, key)
			delete(s.alreadyFired, PushControlNotFollowing+"|"+key)
		}
	}
	for key, st := range troubled {
		first, seen := s.controlFirstAt[key]
		if !seen {
			s.controlFirstAt[key] = now
			continue
		}
		fired := PushControlNotFollowing + "|" + key
		if now.Sub(first) < threshold || s.alreadyFired[fired] {
			continue
		}
		if rule.CooldownS > 0 {
			if last, ok := s.lastFired[fired]; ok && now.Sub(last) < time.Duration(rule.CooldownS)*time.Second {
				continue
			}
		}
		s.alreadyFired[fired] = true
		s.lastFired[fired] = now
		pending = append(pending, st)
	}
	s.mu.Unlock()

	for _, st := range pending {
		s.deliverControl(rule, prio, st, now)
	}
}

func (s *Service) deliverControl(rule config.NotificationRule, prio int, st ControlState, now time.Time) {
	args := map[string]string{"name": st.Driver}
	title, body, err := RenderPush(PushControlNotFollowing, args)
	if err != nil {
		slog.Warn("notifications: catalogue render failed", "event", PushControlNotFollowing, "err", err)
		s.bumpFailed()
		s.emitDispatched(PushControlNotFollowing, st.Driver, Message{Priority: prio}, "failed", err.Error())
		return
	}
	s.deliver(PushControlNotFollowing, st.Driver, Message{
		Kind:     PushControlNotFollowing,
		Title:    title,
		Body:     body,
		Priority: prio,
		Tags:     splitTags(rule.Tags),
		Args:     args,
		Device:   st.Driver,
		At:       now,
	})
}
