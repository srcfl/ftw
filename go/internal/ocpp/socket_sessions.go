package ocpp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

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
	return &socketSessions{slots: map[string]*socketSlot{}, aliases: map[string]*socketBinding{}, raw: map[ws.Channel]*socketBinding{}}
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
	if slot.current != nil || slot.pending != nil || r.Context().Err() != nil {
		return false
	}
	pending := &socketHandshake{remote: r.RemoteAddr}
	slot.pending = pending
	pending.stop = context.AfterFunc(r.Context(), func() {
		slot.mu.Lock()
		defer slot.mu.Unlock()
		if slot.pending == pending {
			slot.pending = nil
		}
	})
	return true
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
func boundCall[T any](s *socketSessions, alias string, fn func(string) (T, error)) (zero T, err error) {
	b := s.lookup(alias)
	if b == nil {
		return zero, errOldSocket
	}
	b.slot.mu.Lock()
	defer b.slot.mu.Unlock()
	if b.slot.current != b || !b.active {
		return zero, errOldSocket
	}
	return fn(b.Channel.ID())
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
