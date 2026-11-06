package cloudlink

import (
	"encoding/base64"
	"errors"
	"slices"
	"time"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// Events and the last word.
//
// The box's notifications reach FTW Cloud as signed events, which the
// service wraps unchanged in an alert for the owners' phones. QueueEvent only
// queues; the writer signs and sends on its own goroutine, ahead of anything
// else due on that tick. An event waits at most cloudwire.AlertMaxAgeMs for
// the uplink: older, a phone would show it without its text and buttons, so
// it is dropped instead. A reconnect sends the events still waiting and
// replays none that left.

// EventIn is one notification the box raised, before ids are cut.
type EventIn struct {
	Kind string
	// Priority is the rule's, 1 to 5. It becomes the event's severity.
	Priority int
	// Args are the catalogue's placeholders. Names are never sent: the
	// link drops the name placeholders and sends the device's opaque id.
	Args map[string]string
	// Driver and Loadpoint are the box's own names for what the event
	// concerns, empty when it concerns neither.
	Driver    string
	Loadpoint string
	At        time.Time
}

const (
	// eventRing is how many events wait for the uplink. When it is full
	// the oldest goes, and the drop is counted.
	eventRing = 32
	// eventIDBytes makes a 12-character event id.
	eventIDBytes = 9
)

// nameArgs are catalogue placeholders that hold a name. The phone fills
// them from the table it holds from the box.
var nameArgs = map[string]bool{"name": true}

// errClosed ends the uplink after its bye, as the box shuts down.
var errClosed = errors.New("cloudlink: the box is shutting down")

// QueueEvent keeps an event for the uplink to sign and send. It never
// blocks. False means FTW Cloud will not carry it: off, being revoked, a
// grant that leaves events out, or a security notice's kind, which goes to
// the owners alone as a notice (Notice).
func (l *Link) QueueEvent(e EventIn) bool {
	if slices.Contains(cloudwire.NoticeKinds, e.Kind) {
		return false
	}
	l.mu.Lock()
	g := l.grant
	if g == nil || g.Revoked || !g.has(protocol.ScopeEventsRead) || l.closing {
		l.mu.Unlock()
		return false
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if len(l.events) >= eventRing {
		l.events = l.events[1:]
		l.eventsDropped++
	}
	l.events = append(l.events, e)
	l.mu.Unlock()
	l.signal()
	return true
}

// sendEvents signs and sends what waits, oldest first.
func (w *writer) sendEvents(snap grantSnapshot, now time.Time) error {
	l := w.link
	if !snap.allowed(cloudwire.MsgEvent) {
		return nil
	}
	l.mu.Lock()
	queued := l.events
	l.events = nil
	l.mu.Unlock()
	for i, e := range queued {
		if now.Sub(e.At) > cloudwire.AlertMaxAgeMs*time.Millisecond {
			l.mu.Lock()
			l.eventsDropped++
			l.mu.Unlock()
			continue
		}
		ev, err := l.projectEvent(snap, e)
		if err != nil {
			return err
		}
		if _, err := w.sendBody(snap, cloudwire.MsgEvent, &ev, true); err != nil {
			// The one that failed may have left; the rest have not.
			l.mu.Lock()
			l.events = append(queued[i+1:len(queued):len(queued)], l.events...)
			l.mu.Unlock()
			return err
		}
	}
	return nil
}

// noticeClient is the client name a notice may carry: on agent.added, while
// the copy is on and its grant has events, as an event's arguments would,
// and only a name cloudwire.NoticeClientOK takes, as the consent page cut it.
func noticeClient(snap grantSnapshot, n notice) string {
	if n.kind != protocol.PushAgentAdded || !snap.copyOn() || !snap.g.has(protocol.ScopeEventsRead) ||
		!cloudwire.NoticeClientOK(n.client) {
		return ""
	}
	return n.client
}

// projectEvent cuts opaque ids and drops names.
func (l *Link) projectEvent(snap grantSnapshot, e EventIn) (cloudwire.Event, error) {
	id, err := randomBytes(eventIDBytes)
	if err != nil {
		return cloudwire.Event{}, err
	}
	gk := snap.g.GrantKey
	ev := cloudwire.Event{
		EventID: base64.RawURLEncoding.EncodeToString(id), Kind: e.Kind,
		Severity: severity(e.Priority), Args: map[string]string{},
	}
	for k, v := range e.Args {
		if !nameArgs[k] {
			ev.Args[k] = v
		}
	}
	if e.Driver != "" {
		ev.Src = cloudwire.SourceID(gk, e.Driver)
	}
	if e.Loadpoint != "" {
		ev.LP = cloudwire.LoadpointID(gk, e.Loadpoint)
	}
	if l.clockSynced() {
		ts := e.At.UnixMilli()
		ev.Ts = &ts
	}
	return ev, nil
}

func severity(priority int) string {
	switch {
	case priority >= 5:
		return cloudwire.SeverityAlarm
	case priority == 4:
		return cloudwire.SeverityWarning
	default:
		return cloudwire.SeverityInfo
	}
}

// Bye tells the service why the box is going and stops the uplink for good.
// why is cloudwire.ByeUpdate for a restart the box chose, such as an update,
// which holds back box.unreachable for an hour; ByeShutdown otherwise. It
// waits at most wait for the bye to leave, and returns at once when the
// uplink is down.
func (l *Link) Bye(why string, wait time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	if l.closing {
		l.mu.Unlock()
		return
	}
	l.closing, l.byeWhy = true, why
	done := make(chan struct{})
	l.byeDone = done
	connected := l.connected
	l.mu.Unlock()
	l.signal()
	if !connected {
		return
	}
	select {
	case <-done:
	case <-time.After(wait):
	}
}

// closeIfAsked sends the bye Bye asked for and ends the connection.
func (w *writer) closeIfAsked(snap grantSnapshot) error {
	l := w.link
	l.mu.Lock()
	closing, why, done := l.closing, l.byeWhy, l.byeDone
	l.mu.Unlock()
	if !closing {
		return nil
	}
	err := w.bye(snap, why, false)
	l.mu.Lock()
	l.byeDone = nil
	l.mu.Unlock()
	if done != nil {
		close(done)
	}
	if err != nil {
		return err
	}
	return errClosed
}

// noticeQueue bounds the notices waiting for the uplink.
const noticeQueue = 16

// notice is one security notice waiting for the uplink.
type notice struct{ kind, client string }

// Notice queues a security notice for the owners' phones (docs/agents.md,
// Push kinds). Unlike an event it needs no grant: it says only that access
// to the box changed, and goes whenever the box is linked. client is the
// name of the client given access, on agent.added; it leaves the box only
// while the readable copy is on and carries events. False means the box is
// not linked, its grant is for statistics only, which carries no notice, or
// the kind is not a notice.
func (l *Link) Notice(kind, client string) bool {
	if !slices.Contains(cloudwire.NoticeKinds, kind) {
		return false
	}
	l.mu.Lock()
	if l.link == nil || l.link.Ending || l.closing || l.grant.statisticsOnly() {
		l.mu.Unlock()
		return false
	}
	dropped := ""
	if len(l.notices) >= noticeQueue {
		dropped = l.notices[0].kind
		l.notices = l.notices[1:]
		l.noticesDropped++
	}
	l.notices = append(l.notices, notice{kind, client})
	l.mu.Unlock()
	if dropped != "" {
		// The box's own transports carried it; FTW Cloud never will.
		l.log.Warn("cloudlink: the notice queue is full; the oldest notice was dropped", "kind", dropped, "queued", noticeQueue)
	}
	l.signal()
	return true
}

// sendNotices sends what waits, oldest first.
func (w *writer) sendNotices(snap grantSnapshot) error {
	if snap.g.statisticsOnly() {
		return nil
	}
	l := w.link
	l.mu.Lock()
	queued := l.notices
	l.notices = nil
	l.mu.Unlock()
	for i, n := range queued {
		body := cloudwire.Notice{Kind: n.kind, Client: noticeClient(snap, n)}
		if _, err := w.sendBody(snap, cloudwire.MsgNotice, &body, true); err != nil {
			l.mu.Lock()
			l.notices = append(queued[i:len(queued):len(queued)], l.notices...)
			l.mu.Unlock()
			return err
		}
	}
	return nil
}
