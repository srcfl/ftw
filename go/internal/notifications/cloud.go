package notifications

import (
	"context"

	"github.com/srcfl/ftw/go/internal/config"
)

// NotifyCloudGrant tells every owner that FTW Cloud was turned on or off,
// with the catalogue's sentence for kind. The rule gates it like any other,
// but a box whose stored rules predate these kinds still sends it: a missing
// rule reads as the default, which is on.
func (s *Service) NotifyCloudGrant(kind string) {
	if kind != PushCloudGranted && kind != PushCloudRevoked {
		return
	}
	s.notifyOwners(kind)
}

// NotifyAgents tells every owner about access to the box (docs/agents.md):
// an agent given access, asking for a durable change or about to lose
// access, a phone's new approval key, or a new owner. Each with the
// catalogue's sentence for kind. These are security notices: they go
// whether or not the owner turned notifications on, and whatever the rule
// says, to every transport the box has.
//
// FTW Cloud carries them on a path of its own, queued before this is called.
// viaCloud says it took this one, which counts as a delivery: a phone paired
// as the first owner finds no other transport with anyone to tell (#1785).
func (s *Service) NotifyAgents(kind string, viaCloud bool) {
	switch kind {
	case PushAgentAdded, PushAgentRequest, PushAgentExpiring, PushApprovalKeySet, PushOwnerAdded:
	default:
		return
	}
	s.notify(kind, true, viaCloud)
}

func (s *Service) notifyOwners(kind string) { s.notify(kind, false, false) }

// notify sends kind to every owner. A security notice passes the master
// switch and its rule.
func (s *Service) notify(kind string, security, viaCloud bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !security && (s.cfg == nil || !s.cfg.Enabled) {
		s.mu.Unlock()
		return
	}
	var events []config.NotificationRule
	defaultPriority := 3
	if s.cfg != nil {
		events = s.cfg.Events
		if s.cfg.DefaultPriority > 0 {
			defaultPriority = s.cfg.DefaultPriority
		}
	}
	rule, ok := findRule(events, kind)
	if !ok {
		rule, _ = findRule(DefaultRules(), kind)
	}
	prio := rule.Priority
	if prio == 0 {
		prio = defaultPriority
	}
	s.mu.Unlock()
	if !rule.Enabled && !security {
		return
	}

	title, body, err := RenderPush(kind, nil)
	if err != nil {
		s.bumpFailed()
		s.emitDispatched(kind, "", Message{Priority: prio}, "failed", err.Error())
		return
	}
	pubs := s.publishers()
	if viaCloud {
		pubs = append(pubs, cloudNotice{})
	}
	s.deliverTo(pubs, kind, "", Message{Kind: kind, Title: title, Body: body, Priority: prio, Tags: splitTags(rule.Tags)})
}

// cloudNotice is FTW Cloud's security notice, already queued for the owners'
// phones when the box's own transports hear of it.
type cloudNotice struct{}

func (cloudNotice) Publish(context.Context, Message) error { return nil }

// CloudPublisher hands each catalogue-worded message to FTW Cloud's uplink,
// which signs it and sends it as an event for the owners' phones. It only
// queues: the uplink sends on its own goroutine, so a slow or absent service
// never holds up another transport. Template rules carry no kind and stay on
// the box's own transports.
type CloudPublisher struct {
	// Queue takes one message and reports whether FTW Cloud will carry it.
	// False means FTW Cloud is off or its grant leaves events out.
	Queue func(Message) bool
}

// Publish queues m, or answers ErrNothingToSend when FTW Cloud will not
// carry it.
func (p CloudPublisher) Publish(_ context.Context, m Message) error {
	if m.Kind == "" || p.Queue == nil || !p.Queue(m) {
		return ErrNothingToSend
	}
	return nil
}
