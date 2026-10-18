package appuplink

import (
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/protocol/appwire"
)

// session is one app's encrypted conversation with this box.
//
// A session belongs to one Cloud tunnel connection and never outlives it. The app
// tears its own Noise session down whenever its carrier drops, because the
// keys are bound to one handshake and the counters to one stream, so keeping
// state across a reconnect here would only be state the peer has forgotten.
type session struct {
	transport *appwire.Transport
	handler   *appproto.Handler
	// appStatic is the app's static key as the handshake authenticated it,
	// never as it was claimed. Logged as a prefix and nothing more.
	appStatic []byte
	// release frees the device's slot (see slots), if the session still
	// holds it. Nil for a session that took none. Set through setRelease,
	// because a newer session can end this one before its claim returns.
	relMu   sync.Mutex
	release func()
	closed  bool
}

// setRelease keeps the slot's release for close, or runs it at once when the
// session already ended.
func (s *session) setRelease(fn func()) {
	s.relMu.Lock()
	if s.closed {
		s.relMu.Unlock()
		fn()
		return
	}
	s.release = fn
	s.relMu.Unlock()
}

// close ends one session for good.
//
// Both halves matter and neither is enough alone: closing the transport stops
// anything further being encrypted onto the socket, and closing the handler
// cancels work already running inside the box — a passthrough request in
// flight, which must not be allowed to finish for a phone that has just been
// locked out.
func (s *session) close() {
	s.handler.Close()
	s.transport.Close()
	s.relMu.Lock()
	s.closed = true
	fn := s.release
	s.release = nil
	s.relMu.Unlock()
	if fn != nil {
		fn()
	}
}

const pendingWait = 30 * time.Second

// prologue binds a session to the box it was enrolled against, so a captured
// handshake cannot be replayed into a different one. Both ends must spell it
// identically; the app builds it in src/lib/state/connect.ts.
func prologue(boxStatic []byte) []byte {
	tag := []byte("ftw.session.v1:")
	return append(tag, boxStatic...)
}

// liveGrant is one session's view of its own enrolment, as it stands now.
//
// The device id and not the key, because that is what the settings page
// revokes by and what an audit line names — one identifier for the phone
// across every place the box talks about it.
type liveGrant struct {
	enroll   *appenroll.Identity
	deviceID string
}

func (g liveGrant) Grant() (appproto.Enrolment, bool) {
	grant, ok := g.enroll.GrantFor(g.deviceID)
	if !ok {
		return appproto.Enrolment{}, false
	}
	return enrolmentOf(grant), true
}

// enrolmentOf is a grant as the message layer reads it. A watch spends its
// parent phone's command ids, so one intent sent by both acts once. An agent
// spends its own.
func enrolmentOf(grant appenroll.Grant) appproto.Enrolment {
	e := appproto.Enrolment{Role: grant.Role, Epoch: grant.Epoch, ParentRole: grant.ParentRole}
	if grant.Parent != "" {
		e.Root = subjectOf(grant.Parent)
	}
	if a := grant.Agent; a != nil {
		e.Agent = &appproto.AgentGrant{Scopes: a.Scopes, Ops: a.Ops, Caps: a.Caps, ExpiresAtMs: a.ExpiresAtMs,
			Principal: a.Principal, Expired: a.Expired, NoClock: a.NoClock}
	}
	return e
}

// callerOf is the caller a session carries. The grant becomes a caller here
// and nowhere else: everything the box later decides about the session is
// decided from this value, and nothing the device sends can add to it.
func callerOf(grant appenroll.Grant) apiauth.Caller {
	kind := apiauth.KindApp
	if grant.Agent != nil {
		kind = apiauth.KindAgent
	}
	return apiauth.Caller{
		Subject: kind + ":" + grant.DeviceID,
		Kind:    kind,
		Role:    grant.Role,
		Scopes:  appproto.ScopesForEnrolment(enrolmentOf(grant)),
		Epoch:   grant.Epoch,
	}
}

// subjectOf is the caller subject for a device id. Built here and nowhere
// else, so the command log and the audit line name a device the same way.
func subjectOf(deviceID string) string {
	return apiauth.KindApp + ":" + deviceID
}
