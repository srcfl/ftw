package appuplink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/protocol/appwire"
)

// The exchange: a watch's one-shot door to the box (ADR 0009, decisions 1
// and 2; the relay's half is srcfl/ftw-webapp docs/watch-exchange.md).
//
// watchOS gives an app HTTPS and nothing lower, so a watch cannot hold a
// stream. It posts one Noise message to the relay, and the relay hands it to
// the box on a second socket, /r/<epoch>/<handle>/exchange, as
// id(16) ‖ message. The box answers id ‖ reply on the same socket. The id is
// the relay's own random number; the box copies it back and reads nothing
// into it.
//
// Exchange sessions are a class of their own: a table of at most eight, apart
// from the four stream places, so a watch never pushes a phone out. Losing
// the exchange socket changes nothing for the uplink, local control or the
// planner.

const (
	// MaxExchangeSessions is how many watch sessions the box holds at once.
	MaxExchangeSessions = 8

	// ExchangeIDBytes is the relay's id in front of every message.
	ExchangeIDBytes = 16

	// exchangeFrameBytes is the one size every frame takes but hello_ok:
	// lane 0's 512 B bucket.
	exchangeFrameBytes = 512
	// exchangeOpenBytes is handshake message 1 with a lane 0 hello.
	exchangeOpenBytes = appwire.Message1Overhead + exchangeFrameBytes
	// exchangeRequestBytes is one transport message with one lane 0 frame.
	exchangeRequestBytes = appwire.TransportOverhead + exchangeFrameBytes
	// exchangeAnswerFrames is how many frames every answer after the
	// handshake carries, so a read, a command and a refusal have one length.
	exchangeAnswerFrames = 2

	// A session ends after four exchanges, a minute without one, or three
	// minutes, whichever comes first. A watch opens one when its confirm
	// screen appears and spends it within seconds.
	exchangeUses     = 4
	exchangeIdle     = 60 * time.Second
	exchangeLifetime = 3 * time.Minute

	// exchangeOpensPerMinute bounds handshakes, each of which costs the box
	// up to four X25519 operations. The rest are dropped unanswered.
	exchangeOpensPerMinute = 60

	// exchangeSettle is how long the box holds an answer for a command's
	// result. Core settles a charger's result in the pass that takes the
	// intent, within 2 s for the EV save, so one answer carries both. Past
	// this the answer carries a tick in the result's place and the watch
	// asks again with the same cmdId.
	exchangeSettle = 3 * time.Second

	// ExchangeCmdFloor is the soonest the box answers a watch's cmd, counted
	// from when the request arrived (ADR 0009, decision F; Fredrik, 14
	// October 2026). A refusal is ready in a millisecond and an applied
	// charge waits for the EV save, so without a floor the time to answer
	// would tell the relay which happened. A command that takes longer, an
	// EV save of up to 2 s, answers when it is done and waits no more.
	ExchangeCmdFloor = 400 * time.Millisecond

	// exchangeInFlight bounds requests handled at once. The relay allows two
	// open exchanges per room; this is the box's own ceiling.
	exchangeInFlight = 8

	// exchangeReadLimit admits the largest request plus its id.
	exchangeReadLimit = 1024
)

// exchangePrologue binds an exchange handshake to this box, and keeps it
// apart from a stream handshake: one can never stand in for the other.
func exchangePrologue(boxStatic []byte) []byte {
	return append([]byte("ftw.exchange.v1:"), boxStatic...)
}

var errExchangeRate = errors.New("appuplink: too many exchange handshakes this minute")

// exchangeSession is one watch's short conversation with the box.
type exchangeSession struct {
	transport *appwire.Transport
	handler   *appproto.Handler
	// appStatic is the watch's key as the handshake authenticated it.
	appStatic []byte
	openedAt  time.Time

	// run lets one request at a time through the handler. A command whose
	// answer went without its result may still be running when the watch
	// asks again; the second request waits for it.
	run sync.Mutex

	mu     sync.Mutex
	lastAt time.Time
	uses   int
	// gen names the request whose frames are being caught, and catching
	// says whether one is. A frame from any other run is dropped.
	gen      uint64
	catching bool
	capture  [][]byte
}

// Send collects the handler's frames for the request being answered now. A
// frame sent when no answer is waiting, such as a result that settled after
// its answer went, is dropped: the command log holds it, and asking again
// returns it.
func (s *exchangeSession) Send(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.catching && len(s.capture) < exchangeAnswerFrames {
		s.capture = append(s.capture, append([]byte(nil), frame...))
	}
	return nil
}

// next numbers a request and stamps it at now.
func (s *exchangeSession) next(now time.Time) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uses++
	s.lastAt = now
	return uint64(s.uses)
}

// catch starts collecting frames for one request.
func (s *exchangeSession) catch(gen uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen, s.capture, s.catching = gen, nil, true
}

// release stops collecting and returns what the request sent. Nil when the
// request never started catching, because an earlier one still held the
// handler.
func (s *exchangeSession) release(gen uint64) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen || !s.catching {
		return nil
	}
	frames := s.capture
	s.capture, s.catching = nil, false
	return frames
}

// spent reports whether the session has run its course.
func (s *exchangeSession) spent(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uses >= exchangeUses ||
		now.Sub(s.lastAt) > exchangeIdle ||
		now.Sub(s.openedAt) > exchangeLifetime
}

func (s *exchangeSession) close() {
	s.handler.Close()
	s.transport.Close()
}

// exchanges is the box's table of watch sessions. One per box: a watch's
// request may arrive on the old hour's socket or the new one, and finds its
// session either way.
type exchanges struct {
	build  HandlerBuilder
	enroll *appenroll.Identity
	static appwire.KeyPair
	log    *slog.Logger
	// now is the real clock. Session ages are durations, and the uplink's
	// injectable clock answers which epoch it is.
	now func() time.Time
	// settle is exchangeSettle; a test shortens it.
	settle time.Duration
	// cmdFloor is ExchangeCmdFloor as New wires it. Zero holds nothing,
	// which only a test that builds the table by hand gets.
	cmdFloor time.Duration

	mu    sync.Mutex
	live  []*exchangeSession
	opens []time.Time
}

// answer takes one request off the exchange socket and returns the reply, or
// nil for silence. A stranger, a removed key, a spent session, a malformed
// body and a handshake over the rate all get silence.
func (x *exchanges) answer(ctx context.Context, body []byte) []byte {
	arrived := time.Now()
	x.prune()
	switch len(body) {
	case exchangeOpenBytes:
		reply, err := x.open(ctx, body)
		if err != nil {
			x.log.Debug("exchange handshake opened no session", "err", err)
			return nil
		}
		return reply
	case exchangeRequestBytes:
		return x.route(ctx, body, arrived)
	default:
		return nil
	}
}

// open runs the responder half of a handshake for a watch. The watch's hello
// rides in message 1 and its hello_ok in message 2, so a session costs one
// round trip.
func (x *exchanges) open(ctx context.Context, message1 []byte) ([]byte, error) {
	if !x.allowOpen() {
		return nil, errExchangeRate
	}
	handshake, err := appwire.NewResponder(appwire.HandshakeOptions{
		StaticKey: x.static,
		Prologue:  exchangePrologue(x.static.Public()),
	})
	if err != nil {
		return nil, err
	}
	payload, err := handshake.ReadMessage(message1)
	if err != nil {
		return nil, err
	}
	frame, err := appwire.DecodeFrame(payload)
	if err != nil {
		return nil, err
	}
	if frame.Envelope.T != appproto.MsgHello {
		return nil, fmt.Errorf("appuplink: message 1 carries %q, not a hello", frame.Envelope.T)
	}
	var hello appproto.Hello
	if err := appproto.Unmarshal(frame.Envelope.B, &hello); err != nil {
		return nil, err
	}

	// Checked before anything is written: answering a stranger would confirm
	// that a box is on this handle and hand them a fresh ephemeral key.
	grant, err := x.enroll.AuthoriseWatch(handshake.RemoteStatic(), hello.Code)
	if err != nil {
		return nil, err
	}

	now := x.now()
	sess := &exchangeSession{appStatic: handshake.RemoteStatic(), openedAt: now, lastAt: now}
	handler, err := x.build(sess, apiauth.Caller{
		Subject: subjectOf(grant.DeviceID),
		Kind:    apiauth.KindApp,
		Role:    grant.Role,
		Scopes:  appproto.ScopesForGrant(grant.Role, grant.ParentRole),
		Epoch:   grant.Epoch,
	}, liveGrant{enroll: x.enroll, deviceID: grant.DeviceID})
	if err != nil {
		return nil, err
	}
	sess.handler = handler

	sess.catch(0)
	err = handler.Handle(ctx, payload)
	frames := sess.release(0)
	if err != nil || len(frames) != 1 {
		handler.Close()
		return nil, fmt.Errorf("appuplink: hello made %d frames: %v", len(frames), err)
	}
	reply, err := handshake.WriteMessage(frames[0])
	if err != nil {
		handler.Close()
		return nil, err
	}
	noise, err := handshake.Split()
	if err != nil {
		handler.Close()
		return nil, err
	}
	sess.transport = appwire.NewTransport(noise)
	x.admit(sess)
	return reply, nil
}

// route finds the session a transport message belongs to by letting each
// one's AEAD try, and runs the exchange in it.
func (x *exchanges) route(ctx context.Context, message []byte, arrived time.Time) []byte {
	x.mu.Lock()
	live := slices.Clone(x.live)
	x.mu.Unlock()

	for _, sess := range live {
		frame, err := sess.transport.Decrypt(message)
		if err != nil {
			continue
		}
		reply := x.exchange(ctx, sess, frame)
		if reply != nil && x.cmdFloor > 0 && isCmd(frame) {
			holdUntil(ctx, arrived.Add(x.cmdFloor))
		}
		if sess.spent(x.now()) {
			x.drop(sess)
		}
		return reply
	}
	return nil
}

// isCmd reports whether a request frame is a command.
func isCmd(frame []byte) bool {
	f, err := appwire.DecodeFrame(frame)
	return err == nil && f.Envelope.T == appproto.MsgCmd
}

// holdUntil waits for a moment on the real clock, or for the socket to go.
func holdUntil(ctx context.Context, at time.Time) {
	wait := time.Until(at)
	if wait <= 0 {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// exchange runs one request through the session's handler and seals the
// answer: exactly two lane 0 frames. A command's ack and result when the
// result settles in time; otherwise a tick takes the missing place.
func (x *exchanges) exchange(ctx context.Context, sess *exchangeSession, frame []byte) []byte {
	gen := sess.next(x.now())
	done := make(chan error, 1)
	go func() {
		sess.run.Lock()
		defer sess.run.Unlock()
		sess.catch(gen)
		done <- sess.handler.Handle(ctx, frame)
	}()

	wait := x.settle
	if wait <= 0 {
		wait = exchangeSettle
	}
	settle := time.NewTimer(wait)
	select {
	case err := <-done:
		if err != nil {
			x.log.Warn("watch exchange failed on a frame", "err", err)
		}
	case <-settle.C:
	case <-ctx.Done():
	}
	settle.Stop()

	frames := sess.release(gen)
	for len(frames) < exchangeAnswerFrames {
		tick, err := sess.handler.PadFrame()
		if err != nil {
			return nil
		}
		frames = append(frames, tick)
	}

	var reply []byte
	for _, f := range frames {
		sealed, err := sess.transport.Encrypt(f)
		if err != nil {
			return nil
		}
		reply = append(reply, sealed...)
	}
	return reply
}

// allowOpen counts a handshake against the minute's budget.
func (x *exchanges) allowOpen() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	now := x.now()
	x.opens = slices.DeleteFunc(x.opens, func(at time.Time) bool { return now.Sub(at) >= time.Minute })
	if len(x.opens) >= exchangeOpensPerMinute {
		return false
	}
	x.opens = append(x.opens, now)
	return true
}

// admit keeps a new session, ending the oldest when the table is full. The
// newcomer is the watch somebody is holding now.
func (x *exchanges) admit(sess *exchangeSession) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.live = append(x.live, sess)
	for len(x.live) > MaxExchangeSessions {
		x.live[0].close()
		x.live = x.live[1:]
	}
}

// prune ends every session that has run its course.
func (x *exchanges) prune() {
	now := x.now()
	x.mu.Lock()
	defer x.mu.Unlock()
	x.live = slices.DeleteFunc(x.live, func(s *exchangeSession) bool {
		if s.spent(now) {
			s.close()
			return true
		}
		return false
	})
}

func (x *exchanges) drop(sess *exchangeSession) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.live = slices.DeleteFunc(x.live, func(s *exchangeSession) bool {
		if s == sess {
			s.close()
			return true
		}
		return false
	})
}

// dropByAppKey ends every session a removed key holds. Its next message 1
// meets AuthoriseWatch and gets silence.
func (x *exchanges) dropByAppKey(pub []byte) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	dropped := 0
	x.live = slices.DeleteFunc(x.live, func(s *exchangeSession) bool {
		if bytes.Equal(s.appStatic, pub) {
			s.close()
			dropped++
			return true
		}
		return false
	})
	return dropped
}

func (x *exchanges) count() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.live)
}

// --------------------------------------------------------------------------
// The socket
// --------------------------------------------------------------------------

// keepExchange holds the exchange socket for one relay connection's epoch and
// handle, and reconnects it with its own backoff until the connection ends. A
// relay that refuses the role, or a socket that drops, costs the watch its
// door and nothing else.
func (u *Uplink) keepExchange(ctx context.Context, origin string, epoch int64, handle string) {
	attempt := 0
	for ctx.Err() == nil {
		target := fmt.Sprintf("%s/r/%d/%s/exchange", origin, epoch, handle)
		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		conn, response, err := u.dialer.DialContext(dialCtx, target, nil)
		cancel()
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if err == nil {
			started := time.Now()
			err = u.serveExchange(ctx, conn)
			if time.Since(started) >= backoffCap {
				attempt = 0
			}
		}
		if ctx.Err() != nil {
			return
		}
		u.log.Debug("exchange socket dropped; retrying", "err", err)

		ceiling := min(backoffBase<<min(attempt, 16), backoffCap)
		attempt = min(attempt+1, 16)
		timer := time.NewTimer(time.Duration(u.random() * float64(ceiling)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// serveExchange reads requests off one exchange socket and writes each answer
// back with the relay's id in front.
func (u *Uplink) serveExchange(ctx context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	var writeMu sync.Mutex
	write := func(message []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, message)
	}

	conn.SetReadLimit(exchangeReadLimit)
	_ = conn.SetReadDeadline(time.Now().Add(u.readTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(u.readTimeout))
	})
	conn.SetPingHandler(func(message string) error {
		if err := conn.SetReadDeadline(time.Now().Add(u.readTimeout)); err != nil {
			return err
		}
		return conn.WriteControl(websocket.PongMessage, []byte(message), time.Now().Add(writeTimeout))
	})

	slots := make(chan struct{}, exchangeInFlight)
	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(u.readTimeout))
		if kind != websocket.BinaryMessage || len(message) <= ExchangeIDBytes {
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			// Busy: the relay times the request out, and the watch asks
			// again. A box that queued without end would answer late,
			// which is worse.
			continue
		}
		go func(message []byte) {
			defer func() { <-slots }()
			id, body := message[:ExchangeIDBytes], message[ExchangeIDBytes:]
			if reply := u.exchanges.answer(ctx, body); reply != nil {
				write(append(append([]byte(nil), id...), reply...))
			}
		}(message)
	}
}
