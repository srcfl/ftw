package appuplink

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/protocol/appwire"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// The tunnel: the phones' sessions that FTW Cloud carries
// (docs/protocol-cloud.md, "Tunnel").
//
// The service names the device and session in tun {dev, sid, d}, so the box
// can look the session up. The Noise handshake proves the phone's key; the
// service cannot grant access. A handshake whose key does not match dev is
// dropped.
//
// A new session waits, pending, until its first transport message decrypts.
// The service sees every message 1 and can replay one under a new sid; the
// box answers it, but the replay never proves itself, so it takes no
// device's place, stamps nothing and spends no code. Each new session runs
// its handshake on a goroutine of its own, so a slow disk under the
// enrolment write never holds up routing or the stream.
//
// Lane 0 goes on each finished driver poll. A beat goes when no frame has
// gone for the subscribed period.
// Frames keep their fixed size. What the service learns from their timing
// is written in docs/protocol-cloud.md, "Lane 0 through the tunnel".

// TunSender sends one tun up the box's link to FTW Cloud. cloudlink.Link is
// one. bulk says the message carries a lane 1 frame, which the link paces;
// everything else goes first. An error whose Dropped method says true lost
// that one message and counted it; any other error means no message will
// leave until the box reconnects.
type TunSender interface {
	SendTun(t cloudwire.Tun, bulk bool) error
}

// dropped reports whether a TunSender's error lost one message only.
func dropped(err error) bool {
	var d interface{ Dropped() bool }
	return errors.As(err, &d) && d.Dropped()
}

const (
	// tunInbox is how many tun messages wait for the table. The link's
	// reader never waits on it; past it a message is dropped, which Noise
	// transport survives at the cost of one frame.
	tunInbox = 256
	// tunSessionInbox is the same, per session, so one phone's slow
	// command never holds up another phone.
	tunSessionInbox = 64
	// tunOpensPerDevice bounds one device's handshakes through the service
	// a minute, so one phone or one pairing cannot starve the others.
	// tunOpensPerMinute bounds them all, each of which costs the box up to
	// four X25519 operations.
	tunOpensPerDevice = 12
	tunOpensPerMinute = 240
	// tunPendingPerDevice is how many unproved sessions a device may hold,
	// and pendingWait (session.go) how long one waits to prove itself.
	tunPendingPerDevice = 4

	// tunGather is how long an arrival waits for polls that finish with it,
	// so drivers that read together go in one frame. One driver's poll is
	// one arrival already.
	tunGather = 5 * time.Millisecond
	// tunRate and tunBurst bound the frames arrivals push: five a second on
	// average, three at once. A few drivers that each read once a second
	// seldom wait; one that reads ten times a second costs each phone five
	// frames a second, and four phones stay well under the service's 60 tun
	// a second per home.
	tunRate  = 5
	tunBurst = 3
	// tunPushUntil and tunBeatUntil are the shares of the tunnel's 24-hour
	// budget past which readings stop: pushes at 60 %, so a display left on
	// all day falls back to a frame a second, and beats at 90 %, so the
	// last tenth is left for handshakes, command answers and ends.
	tunPushUntil = 0.6
	tunBeatUntil = 0.9
	// tunBeat is how often each session is asked whether it has been quiet
	// for its subscribed period. Silence on the phone is at most the period
	// plus this.
	tunBeat = 250 * time.Millisecond
)

// Tunnel is the box's table of sessions that FTW Cloud carries: one live per
// device, in the slot that device holds across every carrier, and the
// pending ones that have not yet proved themselves.
type Tunnel struct {
	build  HandlerBuilder
	enroll *appenroll.Identity
	static appwire.KeyPair
	slots  *slots
	log    *slog.Logger
	now    func() time.Time

	inbox chan tunIn
	// arrivals hears each finished driver poll; nil sends on Beat alone.
	arrivals <-chan struct{}
	// kick tells Run a session went live or ended, so it beats and hears
	// arrivals only while one is live.
	kick chan struct{}

	mu     sync.Mutex
	sender TunSender
	// gen moves on every Drop, so a message queued from an uplink that has
	// ended opens nothing on the next.
	gen     uint64
	live    map[string]*tunSession
	pending map[tunKey]*tunSession
	opens   map[string][]time.Time
}

type tunKey struct {
	dev string
	sid uint32
}

type tunIn struct {
	t   cloudwire.Tun
	gen uint64
}

// tunSession is one phone's session through the service. Its session is nil
// until it proves itself; its goroutine owns everything else.
type tunSession struct {
	dev     string
	sid     uint32
	gen     uint64
	session *session
	// appStatic is the key the handshake proved, once it has run.
	appStatic []byte
	in        chan []byte
	stop      chan struct{}
	once      sync.Once
}

// Tunnel returns the uplink's tunnel table, for the box's link to FTW Cloud
// to deliver to. Each device holds at most one live session.
func (u *Uplink) Tunnel() *Tunnel { return u.tunnel }

func newTunnel(build HandlerBuilder, enroll *appenroll.Identity, slots *slots, arrivals <-chan struct{}, log *slog.Logger) *Tunnel {
	return &Tunnel{
		build: build, enroll: enroll, static: enroll.StaticKey(), slots: slots, log: log, now: time.Now,
		inbox: make(chan tunIn, tunInbox), arrivals: arrivals, kick: make(chan struct{}, 1),
		live: map[string]*tunSession{}, pending: map[tunKey]*tunSession{}, opens: map[string][]time.Time{},
	}
}

// Attach names where answers go. The box's link calls nothing until then.
func (t *Tunnel) Attach(s TunSender) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sender = s
}

// Deliver takes one tun from the service. It never blocks.
func (t *Tunnel) Deliver(m cloudwire.Tun) {
	t.mu.Lock()
	gen := t.gen
	t.mu.Unlock()
	select {
	case t.inbox <- tunIn{t: m, gen: gen}:
	default:
		t.log.Debug("tunnel inbox full; a message was dropped")
	}
}

// Drop ends every session, when the uplink that carried them ends. The
// phones start again on the next.
func (t *Tunnel) Drop() {
	t.mu.Lock()
	t.gen++
	all := make([]*tunSession, 0, len(t.live)+len(t.pending))
	for _, s := range t.live {
		all = append(all, s)
	}
	for _, s := range t.pending {
		all = append(all, s)
	}
	t.live, t.pending = map[string]*tunSession{}, map[tunKey]*tunSession{}
	t.mu.Unlock()
	for _, s := range all {
		s.end()
	}
}

// Run serves the table until ctx ends: the messages the service delivers,
// each arrival's frame, and the beat that keeps a quiet session heard.
func (t *Tunnel) Run(ctx context.Context) {
	defer t.Drop()
	// The beat runs, and arrivals are heard, only while a session is live:
	// a box nobody watches wakes for nothing.
	var beat *time.Ticker
	defer func() {
		if beat != nil {
			beat.Stop()
		}
	}()
	// flush is armed by an arrival and fires once its round is in and the
	// pace allows a frame. Nil while nothing waits.
	var flush <-chan time.Time
	p := pace{tokens: tunBurst, at: time.Now()}
	for {
		var beats <-chan time.Time
		var arrivals <-chan struct{}
		switch live := t.count() > 0; {
		case live && beat == nil:
			beat = time.NewTicker(tunBeat)
		case !live && beat != nil:
			beat.Stop()
			beat = nil
		}
		if beat != nil {
			beats, arrivals = beat.C, t.arrivals
		}
		select {
		case <-ctx.Done():
			return
		case m := <-t.inbox:
			t.route(ctx, m)
		case <-t.kick:
		case <-arrivals:
			if flush == nil {
				flush = time.After(max(tunGather, p.wait(time.Now())))
			}
		case <-flush:
			flush = nil
			// A frame spends the pace; a poll nobody sees spends nothing.
			if t.budgetUsed() < tunPushUntil && t.anyVisible() {
				p.take(time.Now())
				t.each("push", (*appproto.Handler).Push)
			}
		case <-beats:
			if t.budgetUsed() < tunBeatUntil {
				t.each("beat", (*appproto.Handler).Beat)
			}
		}
	}
}

// anyVisible says a live session would take a push.
func (t *Tunnel) anyVisible() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.live {
		if s.session.handler.Visible() {
			return true
		}
	}
	return false
}

// budgetUsed is the share of the link's tunnel budget spent in the last 24
// hours, or 0 from a link that does not say.
func (t *Tunnel) budgetUsed() float64 {
	t.mu.Lock()
	s := t.sender
	t.mu.Unlock()
	if b, ok := s.(interface{ TunBudgetUsed() float64 }); ok {
		return b.TunBudgetUsed()
	}
	return 0
}

// kicked tells Run the live sessions changed. It never blocks.
func (t *Tunnel) kicked() {
	select {
	case t.kick <- struct{}{}:
	default:
	}
}

// count is how many live sessions the table holds.
func (t *Tunnel) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.live)
}

// route hands one message to its session, live or pending, or starts a
// pending one. It never waits: each session works on its own goroutine.
func (t *Tunnel) route(ctx context.Context, m tunIn) {
	key := tunKey{m.t.Dev, m.t.Sid}
	t.mu.Lock()
	if m.gen != t.gen {
		t.mu.Unlock()
		return
	}
	s := t.pending[key]
	if cur := t.live[m.t.Dev]; cur != nil && cur.sid == m.t.Sid {
		s = cur
	}
	if s == nil && !m.t.Ended() {
		s = t.startLocked(ctx, key, m.gen)
	}
	t.mu.Unlock()

	switch {
	case s == nil:
		// The end of a session the box no longer holds, or a handshake
		// over the device's allowance.
	case m.t.Ended():
		t.remove(s, false)
	default:
		select {
		case s.in <- m.t.D:
		default:
			t.log.Debug("tunnel session inbox full; a message was dropped")
		}
	}
}

// startLocked makes a pending session for a new (dev, sid), within the
// device's allowance. The caller holds t.mu.
func (t *Tunnel) startLocked(ctx context.Context, key tunKey, gen uint64) *tunSession {
	if !t.allowOpenLocked(key.dev) {
		return nil
	}
	held := 0
	var oldest *tunSession
	for k, p := range t.pending {
		if k.dev == key.dev {
			held++
			if oldest == nil || p.sid != key.sid {
				oldest = p
			}
		}
	}
	if held >= tunPendingPerDevice && oldest != nil {
		delete(t.pending, tunKey{oldest.dev, oldest.sid})
		go oldest.end()
	}
	s := &tunSession{dev: key.dev, sid: key.sid, gen: gen,
		in: make(chan []byte, tunSessionInbox), stop: make(chan struct{})}
	t.pending[key] = s
	go s.run(ctx, t)
	return s
}

var (
	errTunDevice = errors.New("appuplink: the handshake's key is not the device the service named")
	errTunShort  = errors.New("appuplink: too short to be handshake message 1")
	errTunGone   = errors.New("appuplink: the tunnel's uplink is gone")
)

// run is one session's life: the handshake on its first message, then each
// message in order. Until a transport message decrypts, the session is
// pending and has no handler; the first that does makes it live.
func (s *tunSession) run(ctx context.Context, t *Tunnel) {
	var first []byte
	select {
	case <-ctx.Done():
		return
	case <-s.stop:
		return
	case first = <-s.in:
	}
	transport, commit, err := t.handshake(s, first)
	if err != nil {
		t.log.Debug("tun opened no session", "err", err)
		t.remove(s, false)
		return
	}
	defer transport.Close()

	wait := time.NewTimer(pendingWait)
	defer wait.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-wait.C:
			if s.session == nil {
				// Never proved: a replay, or a phone that went away.
				t.remove(s, false)
				return
			}
		case d := <-s.in:
			frame, err := transport.Decrypt(d)
			if err != nil {
				// A replay, or bytes that are not this session's. The
				// service can delay and drop; it cannot make the box act.
				continue
			}
			if s.session == nil {
				if err := t.promote(s, transport, commit); err != nil {
					t.log.Debug("a proved tunnel session was not admitted", "err", err)
					t.remove(s, true)
					return
				}
			}
			if err := s.session.handler.Handle(ctx, frame); err != nil {
				t.log.Warn("app session failed on a frame", "err", err)
				t.remove(s, true)
				return
			}
		}
	}
}

// handshake runs the responder half for a new session and answers it. A
// message that is not a message 1 gets an end back, so the phone starts
// again; a key the box does not admit, or one the service put under another
// device, gets nothing.
func (t *Tunnel) handshake(s *tunSession, message1 []byte) (*appwire.Transport, func() (appenroll.Grant, error), error) {
	end := cloudwire.Tun{Dev: s.dev, Sid: s.sid}
	if len(message1) < appwire.Message1Overhead {
		t.send(end)
		return nil, nil, errTunShort
	}
	hs, err := appwire.NewResponder(appwire.HandshakeOptions{
		StaticKey: t.static,
		Prologue:  prologue(t.static.Public()),
	})
	if err != nil {
		return nil, nil, err
	}
	payload, err := hs.ReadMessage(message1)
	if err != nil {
		// Not message 1 of this box's session: most often a frame of a
		// session the box dropped. The phone starts again.
		t.send(end)
		return nil, nil, err
	}
	// The service names the device; the handshake proves it. A key that is
	// not the named device's is the service, or a bug, putting one phone's
	// session under another's slot.
	if cloudwire.DeviceID(hs.RemoteStatic()) != s.dev {
		return nil, nil, errTunDevice
	}
	// Built before the caller is checked and sent only after. The check
	// changes nothing yet; commit does, once the session proves itself.
	reply, err := hs.WriteMessage(nil)
	if err != nil {
		return nil, nil, err
	}
	noise, err := hs.Split()
	if err != nil {
		return nil, nil, err
	}
	transport := appwire.NewTransport(noise)
	// CheckTunnel and not Check: an agent reaches the box this way and no
	// other (docs/agents.md).
	if _, commit, err := t.enroll.CheckTunnel(noise.RemoteStatic, payload); err != nil {
		transport.Close()
		return nil, nil, err
	} else {
		t.mu.Lock()
		s.appStatic = noise.RemoteStatic
		t.mu.Unlock()
		if !t.send(cloudwire.Tun{Dev: s.dev, Sid: s.sid, D: reply}) {
			transport.Close()
			return nil, nil, errTunGone
		}
		return transport, commit, nil
	}
}

// promote makes a proved session live: it commits the pairing or the
// reconnect stamp, builds the handler from the grant as it stands now, takes
// the device's one slot and the table's live place.
func (t *Tunnel) promote(s *tunSession, transport *appwire.Transport, commit func() (appenroll.Grant, error)) error {
	grant, err := commit()
	if err != nil {
		return err
	}
	caller := callerOf(grant)
	send := func(msg []byte, bulk bool) error {
		return t.sendFrame(cloudwire.Tun{Dev: s.dev, Sid: s.sid, D: msg}, bulk)
	}
	handler, err := t.build(tunSender{transport: transport, send: send}, caller,
		liveGrant{enroll: t.enroll, deviceID: grant.DeviceID})
	if err != nil {
		return err
	}
	sess := &session{transport: transport, handler: handler, appStatic: s.appStatic}

	t.mu.Lock()
	if s.gen != t.gen {
		// The uplink ended while the session proved itself.
		t.mu.Unlock()
		sess.close()
		return errTunGone
	}
	delete(t.pending, tunKey{s.dev, s.sid})
	old := t.live[s.dev]
	s.session = sess
	t.live[s.dev] = s
	t.mu.Unlock()
	if old != nil && old != s {
		// The phone hears that its older session ended.
		t.remove(old, true)
	}
	// Claim the device's slot and end any older session.
	sess.setRelease(t.slots.claim(grant.DeviceID, func() { t.remove(s, true) }))
	t.kicked()
	t.log.Info("app session open through FTW Cloud")
	return nil
}

// tunSender encrypts one session's frames for the tunnel and says which are
// bulk, so the link paces those and sends lane 0 (commands, their answers
// and the readings) first. A message the link dropped and counted is gone like
// any lost frame, and the session goes on; only a lost uplink fails it.
type tunSender struct {
	transport *appwire.Transport
	send      func(msg []byte, bulk bool) error
}

func (s tunSender) Send(frame []byte) error {
	msg, err := s.transport.Encrypt(frame)
	if err != nil {
		return err
	}
	return s.send(msg, len(frame) > 1 && frame[1] == appwire.LaneBulk)
}

// SendInLine sends a bulk frame with lane 0, in order: the snapshot, which a
// reading sent after it must not overtake.
func (s tunSender) SendInLine(frame []byte) error {
	msg, err := s.transport.Encrypt(frame)
	if err != nil {
		return err
	}
	return s.send(msg, false)
}

// end closes the session once and frees its slot. It reports whether this
// call ended it.
func (s *tunSession) end() bool {
	ended := false
	s.once.Do(func() {
		ended = true
		close(s.stop)
		if s.session != nil {
			s.session.close()
		}
	})
	return ended
}

// remove takes a session out of the table and ends it. tell sends the phone
// an end, when the uplink that carried it is still up.
func (t *Tunnel) remove(s *tunSession, tell bool) {
	t.mu.Lock()
	if t.live[s.dev] == s {
		delete(t.live, s.dev)
		defer t.kicked()
	}
	if t.pending[tunKey{s.dev, s.sid}] == s {
		delete(t.pending, tunKey{s.dev, s.sid})
	}
	current := s.gen == t.gen
	t.mu.Unlock()
	if s.end() && tell && current {
		t.send(cloudwire.Tun{Dev: s.dev, Sid: s.sid})
	}
}

// send hands one tun to the link, ahead of bulk: a handshake's answer and
// an end. False means it did not leave.
func (t *Tunnel) send(m cloudwire.Tun) bool { return t.sendTun(m, false) == nil }

// sendFrame hands a session's frame to the link. A frame the link dropped
// and counted comes back as appproto.ErrDropped: the session goes on, and
// its handler sends what the frame carried again.
func (t *Tunnel) sendFrame(m cloudwire.Tun, bulk bool) error {
	t.mu.Lock()
	s := t.sender
	t.mu.Unlock()
	if s == nil {
		return errTunGone
	}
	err := s.SendTun(m, bulk)
	if dropped(err) {
		return fmt.Errorf("%w: %w", appproto.ErrDropped, err)
	}
	return err
}

// sendTun hands the table's own message to the link: a handshake's answer
// or an end. A counted drop is not an error: the phone starts again. A lost
// uplink is.
func (t *Tunnel) sendTun(m cloudwire.Tun, bulk bool) error {
	t.mu.Lock()
	s := t.sender
	t.mu.Unlock()
	if s == nil {
		return errTunGone
	}
	err := s.SendTun(m, bulk)
	if dropped(err) {
		return nil
	}
	return err
}

// allowOpenLocked counts a handshake against its device's allowance for the
// minute, and against the box's. The caller holds t.mu.
func (t *Tunnel) allowOpenLocked(dev string) bool {
	now := t.now()
	total := 0
	for d, list := range t.opens {
		kept := list[:0]
		for _, at := range list {
			if now.Sub(at) < time.Minute {
				kept = append(kept, at)
			}
		}
		if len(kept) == 0 {
			delete(t.opens, d)
			continue
		}
		t.opens[d] = kept
		total += len(kept)
	}
	if total >= tunOpensPerMinute || len(t.opens[dev]) >= tunOpensPerDevice {
		return false
	}
	t.opens[dev] = append(t.opens[dev], now)
	return true
}

// pace is a token bucket: tunRate frames a second, tunBurst at once.
type pace struct {
	tokens float64
	at     time.Time
}

func (p *pace) fill(now time.Time) {
	p.tokens = min(tunBurst, p.tokens+now.Sub(p.at).Seconds()*tunRate)
	p.at = now
}

// wait is how long until a frame may go.
func (p *pace) wait(now time.Time) time.Duration {
	p.fill(now)
	if p.tokens >= 1 {
		return 0
	}
	return time.Duration((1 - p.tokens) / tunRate * float64(time.Second))
}

// take spends one frame.
func (p *pace) take(now time.Time) {
	p.fill(now)
	p.tokens--
}

// each runs one lane 0 step on every live session: Push or Beat.
func (t *Tunnel) each(what string, step func(*appproto.Handler) error) {
	t.mu.Lock()
	live := make([]*tunSession, 0, len(t.live))
	for _, s := range t.live {
		live = append(live, s)
	}
	t.mu.Unlock()
	for _, s := range live {
		if err := step(s.session.handler); err != nil {
			t.log.Warn("app session failed on a "+what, "err", err)
			t.remove(s, true)
		}
	}
}

// dropByAppKey ends every session a revoked key runs, live or pending.
func (t *Tunnel) dropByAppKey(pub []byte) int {
	t.mu.Lock()
	var gone []*tunSession
	for _, s := range t.live {
		if bytes.Equal(s.appStatic, pub) {
			gone = append(gone, s)
		}
	}
	for _, s := range t.pending {
		if bytes.Equal(s.appStatic, pub) {
			gone = append(gone, s)
		}
	}
	t.mu.Unlock()
	dropped := 0
	for _, s := range gone {
		if s.session != nil {
			_ = s.session.handler.Terminate(appproto.TerminateRevoked)
			dropped++
		}
		t.remove(s, true)
	}
	return dropped
}

// --------------------------------------------------------------------------
// One slot per device
// --------------------------------------------------------------------------

// slots holds each device's one session. A new
// session from a device ends its older one, so a phone that reconnects, a
// watch or another phone never pushes out a different device's session.
type slots struct {
	mu   sync.Mutex
	n    uint64
	held map[string]slot
}

type slot struct {
	id  uint64
	end func()
}

// claim gives dev's slot to a new session, whose end func closes it, and
// ends the session that held it. The returned func frees the slot if this
// session still holds it. A nil table claims nothing.
func (s *slots) claim(dev string, end func()) func() {
	if s == nil {
		return func() {}
	}
	s.mu.Lock()
	if s.held == nil {
		s.held = map[string]slot{}
	}
	s.n++
	id := s.n
	old, had := s.held[dev]
	s.held[dev] = slot{id: id, end: end}
	s.mu.Unlock()
	if had {
		old.end()
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if cur, ok := s.held[dev]; ok && cur.id == id {
			delete(s.held, dev)
		}
	}
}
