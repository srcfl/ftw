package appuplink

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appwire"
)

// MaxSessions is how many phones may watch one house at once.
//
// It matches the relay's own per-handle stream limit; a fifth stream never
// reaches the box, so a larger number here would only describe sessions that
// cannot exist.
const MaxSessions = 4

// session is one app's encrypted conversation with this box.
//
// A session belongs to one relay connection and never outlives it. The app
// tears its own Noise session down whenever its carrier drops, because the
// keys are bound to one handshake and the counters to one stream, so keeping
// state across a reconnect here would only be state the peer has forgotten.
type session struct {
	transport *appwire.Transport
	handler   *appproto.Handler
	// appStatic is the app's static key as the handshake authenticated it,
	// never as it was claimed. Logged as a prefix and nothing more.
	appStatic []byte
}

// sender encrypts one session's frames onto the shared socket.
type sender struct {
	transport *appwire.Transport
	write     func([]byte) error
}

func (s sender) Send(frame []byte) error {
	message, err := s.transport.Encrypt(frame)
	if err != nil {
		return err
	}
	return s.write(message)
}

// sessions is the demultiplexer.
//
// The relay broadcasts the box's frames to every stream in the room and hands
// the box every stream's frames without saying which is which — it cannot say,
// because telling them apart would mean reading them. So the box does what the
// app does in the other direction: try each live session, and let the AEAD
// decide. A message nothing authenticates is either a new handshake or noise,
// and noise is dropped.
type sessions struct {
	mu    sync.Mutex
	live  []*session
	build func(appproto.Sender) (*appproto.Handler, error)

	enroll *appenroll.Identity
	static appwire.KeyPair
	write  func([]byte) error
	log    *slog.Logger
}

// prologue binds a session to the box it was enrolled against, so a captured
// handshake cannot be replayed into a different one. Both ends must spell it
// identically; the app builds it in src/lib/state/connect.ts.
func prologue(boxStatic []byte) []byte {
	tag := []byte("ftw.session.v1:")
	return append(tag, boxStatic...)
}

// deliver routes one message from the relay.
func (s *sessions) deliver(ctx context.Context, message []byte) {
	if s.route(ctx, message) {
		return
	}
	if err := s.open(message); err != nil {
		// Anyone who can reach the relay can write to this room, so a
		// message that authenticates against nothing is ordinary. Tearing
		// the uplink down over one would hand a stranger a way to cut the
		// household off.
		s.log.Debug("message matched no session and opened none", "err", err)
	}
}

// route hands the message to the session that can decrypt it, if any.
func (s *sessions) route(ctx context.Context, message []byte) bool {
	s.mu.Lock()
	live := append([]*session(nil), s.live...)
	s.mu.Unlock()

	for _, sess := range live {
		frame, err := sess.transport.Decrypt(message)
		if err != nil {
			continue
		}
		if err := sess.handler.Handle(ctx, frame); err != nil {
			s.log.Warn("app session failed on a frame", "err", err)
			s.drop(sess)
		}
		return true
	}
	return false
}

// open runs the responder half of a handshake and, if the app is allowed in,
// keeps the session.
func (s *sessions) open(message []byte) error {
	if len(message) < appwire.Message1Overhead {
		return errors.New("appuplink: too short to be handshake message 1")
	}

	handshake, err := appwire.NewResponder(appwire.HandshakeOptions{
		StaticKey: s.static,
		Prologue:  prologue(s.static.Public()),
	})
	if err != nil {
		return err
	}

	payload, err := handshake.ReadMessage(message)
	if err != nil {
		return err
	}

	// The reply is built before the caller is checked, because the app's
	// static key is only authenticated once the pattern has run — but it is
	// not sent until the check passes. Replying to a stranger would confirm
	// that a box is on this handle and hand them a fresh ephemeral key for
	// their trouble.
	reply, err := handshake.WriteMessage(nil)
	if err != nil {
		return err
	}

	noise, err := handshake.Split()
	if err != nil {
		return err
	}
	transport := appwire.NewTransport(noise)

	// The payload is the pairing code from the QR. A device that has already
	// paired presents nothing and is recognised by its static key, which is
	// what makes the code single-use without breaking reconnects.
	if err := s.enroll.Authorise(noise.RemoteStatic, payload); err != nil {
		transport.Close()
		return err
	}

	handler, err := s.build(sender{transport: transport, write: s.write})
	if err != nil {
		transport.Close()
		return err
	}

	sess := &session{transport: transport, handler: handler, appStatic: noise.RemoteStatic}
	s.admit(sess)

	// Written after the session is registered, so a fast app whose first
	// frame arrives immediately meets a session that exists.
	if err := s.write(reply); err != nil {
		s.drop(sess)
		return err
	}

	s.log.Info("app session open", "sessions", s.count())
	return nil
}

// admit adds a session, evicting the oldest when the room is full.
//
// Evicting rather than refusing: the newcomer is the phone somebody is holding
// right now, and the session it displaces is one whose socket the relay has
// probably already reaped.
func (s *sessions) admit(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.live = append(s.live, sess)
	for len(s.live) > MaxSessions {
		s.live[0].transport.Close()
		s.live = s.live[1:]
	}
}

func (s *sessions) drop(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, live := range s.live {
		if live == sess {
			live.transport.Close()
			s.live = append(s.live[:i], s.live[i+1:]...)
			return
		}
	}
}

// closeAll ends every session. Called when the relay says the last app left
// and when the socket dies: in both cases the app has already forgotten its
// half, so holding ours would only be memory nobody can decrypt into.
func (s *sessions) closeAll() {
	s.mu.Lock()
	live := s.live
	s.live = nil
	s.mu.Unlock()

	for _, sess := range live {
		sess.transport.Close()
	}
}

func (s *sessions) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// tick drives every subscribed session's telemetry frame.
//
// Every session, every tick, whether or not anything changed: silence on lane
// 0 would itself tell the relay operator that nothing happened in the house
// that second.
func (s *sessions) tick() {
	s.mu.Lock()
	live := append([]*session(nil), s.live...)
	s.mu.Unlock()

	for _, sess := range live {
		if err := sess.handler.Tick(); err != nil {
			s.log.Warn("app session failed on a tick", "err", err)
			s.drop(sess)
		}
	}
}
