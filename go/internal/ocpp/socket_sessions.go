package ocpp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lorenzodonini/ocpp-go/ws"
)

var errOldSocket = errors.New("OCPP connection is no longer current")

// The SDK dispatches requests and replies on goroutines and retains only
// Channel.ID. Give each socket an immutable internal ID; public charger names
// remain the authentication, configuration and hardware-state keys.
// Both protocol listeners share this owner table.
type socketSessions struct {
	mu      sync.Mutex
	next    uint64
	slots   map[string]*socketSlot
	aliases map[string]*socketBinding
	raw     map[ws.Channel]*socketBinding
	// after holds the callback work a message handler queued while its
	// boundCall holds that charger's slot lock, keyed by charger id. A key
	// is present only during such a call.
	after map[string][]func()
	// The library closes the sockets in its table when it stops, but a
	// handshake that has passed the connection check joins that table
	// later and would stay open on a stopped server. Once closed is set no
	// handshake is admitted, and handshakes counts the admitted ones that
	// have neither joined the table nor ended.
	closed     bool
	handshakes sync.WaitGroup
}
type socketSlot struct {
	mu      sync.Mutex
	current *socketBinding
	pending *socketHandshake
}
type socketBinding struct {
	ws.Channel
	alias       string
	slot        *socketSlot
	ready       chan struct{}
	active      bool // slot.mu
	initialized bool // slot.mu
}

func (b *socketBinding) ID() string { return b.alias }
func newSocketSessions() *socketSessions {
	return &socketSessions{slots: map[string]*socketSlot{}, aliases: map[string]*socketBinding{}, raw: map[ws.Channel]*socketBinding{}, after: map[string][]func(){}}
}
func (s *socketSessions) slot(id string) *socketSlot {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.slots[id]
	if slot == nil {
		slot = &socketSlot{}
		s.slots[id] = slot
	}
	return slot
}
func (s *socketSessions) lookup(alias string) *socketBinding {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aliases[alias]
}
func (s *socketSessions) currentID(id string) (string, error) {
	// Lookup must not acquire slot.mu: a Boot handler can request an async
	// capability probe while holding that slot's mutation guard.
	s.mu.Lock()
	defer s.mu.Unlock()
	for alias, b := range s.aliases {
		if b.Channel.ID() == id && b.active {
			return alias, nil
		}
	}
	return "", errOldSocket
}

type socketHandshake struct {
	remote string
	stop   func() bool
}

func (s *socketSessions) reserveConnection(id string, r *http.Request) bool {
	slot := s.slot(id)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.current != nil || slot.pending != nil || r.Context().Err() != nil || !s.admitHandshake() {
		return false
	}
	pending := &socketHandshake{remote: r.RemoteAddr}
	slot.pending = pending
	pending.stop = context.AfterFunc(r.Context(), func() {
		slot.mu.Lock()
		defer slot.mu.Unlock()
		if slot.pending == pending {
			slot.pending = nil
			s.handshakes.Done()
		}
	})
	return true
}

// admitHandshake counts one more handshake past the connection check, unless
// the server has begun to stop. Whoever clears that handshake's slot.pending
// calls Done.
func (s *socketSessions) admitHandshake() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.handshakes.Add(1)
	return true
}

// refuseHandshakes refuses every later handshake and waits, at most wait,
// until each admitted one has joined the library's table or ended. It reports
// whether they all did.
func (s *socketSessions) refuseHandshakes(wait time.Duration) bool {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.handshakes.Wait()
		close(done)
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
func (g *guardedServer) binding(ch ws.Channel) *socketBinding {
	s := g.sessions
	slot := s.slot(ch.ID())
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.raw[ch]; b != nil {
		return b
	}
	if g.Server.Connections(ch.ID()) != ch {
		return nil
	}
	s.next++
	b := &socketBinding{Channel: ch, alias: fmt.Sprintf("socket-%d", s.next), slot: slot, ready: make(chan struct{})}
	s.raw[ch], s.aliases[b.alias] = b, b
	return b
}
func (g *guardedServer) SetNewClientHandler(fn func(ws.Channel)) {
	g.Server.SetNewClientHandler(func(ch ws.Channel) {
		b := g.binding(ch)
		if b == nil {
			return
		}
		b.slot.mu.Lock()
		if b.slot.current != nil || b.slot.pending == nil || b.slot.pending.remote != ch.RemoteAddr().String() {
			close(b.ready)
			if g.Server.Connections(ch.ID()) == ch {
				_ = g.Server.StopConnection(ch.ID(), websocket.CloseError{Code: websocket.ClosePolicyViolation, Text: "charger already connected"})
			}
			b.slot.mu.Unlock()
			return
		}
		b.slot.pending.stop()
		b.slot.pending = nil
		g.sessions.handshakes.Done()
		b.slot.current = b
		g.sessions.mu.Lock()
		b.active = true
		g.sessions.mu.Unlock()
		b.initialized = true
		b.slot.mu.Unlock()
		// SDK request queues and Core OnConnect must exist before the read pump
		// delivers its first message (the SDK starts that pump before this hook).
		fn(b)
		close(b.ready)
	})
}
func (g *guardedServer) SetDisconnectedClientHandler(fn func(ws.Channel)) {
	g.Server.SetDisconnectedClientHandler(func(ch ws.Channel) {
		g.sessions.mu.Lock()
		b := g.sessions.raw[ch]
		g.sessions.mu.Unlock()
		if b == nil {
			return
		}
		<-b.ready
		b.slot.mu.Lock()
		g.sessions.mu.Lock()
		b.active = false
		g.sessions.mu.Unlock()
		initialized := b.initialized
		b.slot.mu.Unlock()
		if initialized {
			fn(b)
		}
		b.slot.mu.Lock()
		if b.slot.current == b {
			b.slot.current = nil
		}
		g.sessions.mu.Lock()
		delete(g.sessions.aliases, b.alias)
		delete(g.sessions.raw, ch)
		g.sessions.mu.Unlock()
		b.slot.mu.Unlock()
	})
}
func (g *guardedServer) SetMessageHandler(fn func(ws.Channel, []byte) error) {
	g.Server.SetMessageHandler(func(ch ws.Channel, data []byte) error {
		b := g.binding(ch)
		if b == nil {
			return nil
		}
		<-b.ready
		return fn(b, data)
	})
}
func (g *guardedServer) Write(alias string, data []byte) error {
	b := g.sessions.lookup(alias)
	if b == nil {
		return errOldSocket
	}
	b.slot.mu.Lock()
	defer b.slot.mu.Unlock()
	if b.slot.current != b || !b.active || g.Server.Connections(b.Channel.ID()) != b.Channel {
		return errOldSocket
	}
	// Connection validation takes this same slot lock and refuses an occupied
	// slot. A replacement cannot appear between this check and raw Write.
	return g.Server.Write(b.Channel.ID(), data)
}

// Match and mutate under one per-charger lock. SDK goroutines from a closed
// socket cannot alter its replacement, including via a late BootNotification.
//
// The lock guards state only. Every write to this charger takes the same
// lock, so callback work that fn queues through later (main.go writes SQLite
// from identityReported) runs after the unlock: a stalled callback must not
// stop the commands to this charger and, through the SDK's single request
// pump, to every other charger.
func boundCall[T any](s *socketSessions, alias string, fn func(string) (T, error)) (zero T, err error) {
	b := s.lookup(alias)
	if b == nil {
		return zero, errOldSocket
	}
	b.slot.mu.Lock()
	if b.slot.current != b || !b.active {
		b.slot.mu.Unlock()
		return zero, errOldSocket
	}
	id := b.Channel.ID()
	s.mu.Lock()
	s.after[id] = nil
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		after := s.after[id]
		delete(s.after, id)
		s.mu.Unlock()
		b.slot.mu.Unlock()
		for _, f := range after {
			f()
		}
	}()
	return fn(id)
}

// later runs fn once the boundCall that holds id's slot lock releases it, or
// now when no boundCall holds it.
func (s *socketSessions) later(id string, fn func()) {
	s.mu.Lock()
	after, held := s.after[id]
	if held {
		s.after[id] = append(after, fn)
	}
	s.mu.Unlock()
	if !held {
		fn()
	}
}
func (s *socketSessions) disconnected(alias string, fn func(string)) {
	b := s.lookup(alias)
	if b == nil {
		return
	}
	b.slot.mu.Lock()
	defer b.slot.mu.Unlock()
	if b.slot.current == b {
		fn(b.Channel.ID())
		b.slot.current = nil
	}
}

func (g *guardedServer) StopConnection(alias string, reason websocket.CloseError) error {
	b := g.sessions.lookup(alias)
	if b == nil {
		return errOldSocket
	}
	b.slot.mu.Lock()
	defer b.slot.mu.Unlock()
	if b.slot.current != b || !b.active || g.Server.Connections(b.Channel.ID()) != b.Channel {
		return errOldSocket
	}
	return g.Server.StopConnection(b.Channel.ID(), reason)
}
