package cloudlink

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

const (
	dialTimeout = 15 * time.Second
	// writeTimeout: a write blocked this long is a dead socket.
	writeTimeout = 10 * time.Second
	// readTimeout is three of the service's 15 s pings.
	readTimeout = 45 * time.Second
	// handshakeTimeout bounds challenge and hello_ok.
	handshakeTimeout = 15 * time.Second
	// purgeWait is how long a revoking box waits for the receipt before it
	// hangs up and tries again later.
	purgeWait = 30 * time.Second

	backoffBase = 500 * time.Millisecond
	backoffCap  = 60 * time.Second
)

// errService is the service breaking the protocol. The link closes and backs
// off; nothing the service says can do more than that.
var errService = errors.New("cloudlink: the service broke the protocol")

func (l *Link) dialer() *websocket.Dialer {
	return &websocket.Dialer{
		// No proxy, whatever the environment says. A proxy that ends TLS
		// breaks the handshake binding by design, and one that does not
		// adds nothing.
		Proxy:             nil,
		HandshakeTimeout:  dialTimeout,
		TLSClientConfig:   l.tlsConfig(),
		EnableCompression: false,
		NetDialContext:    l.opts.NetDial,
	}
}

// once dials, serves until the connection ends and says how long to wait.
func (l *Link) once(ctx context.Context, snap grantSnapshot) (time.Duration, error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, err := l.dial(dialCtx)
	cancel()
	if err != nil {
		l.hostFailed()
		return l.backoff(), fmt.Errorf("dialling FTW Cloud: %w", err)
	}
	defer conn.Close()

	connectedAt := time.Now()
	err = l.serve(ctx, conn, snap)
	l.mu.Lock()
	up := l.connected
	upFor := time.Since(l.upAt)
	l.connected = false
	l.serviceStatsPolicy = ""
	if time.Since(connectedAt) >= backoffCap {
		l.attempt = 0
	}
	l.mu.Unlock()
	if q, ok := conn.(*quicConn); ok && q.network != "" && quicFailed(err, up, upFor) {
		l.demote(q.network)
		l.log.Info("QUIC failed on this network; the WebSocket goes first here for a day", "err", err)
	}
	if errors.Is(err, errLinkEnded) || errors.Is(err, errClosed) {
		return 0, nil
	}
	// The service's two closes that are not failures: another instance
	// holds the home, or this one is about to stop.
	var closed *websocket.CloseError
	if errors.As(err, &closed) {
		switch closed.Code {
		case cloudwire.CloseWrongShard:
			if l.followShard(closed.Text) {
				return 0, nil
			}
		case cloudwire.CloseRestart:
			wait := spread(cloudwire.RestartSpreadMs(closed.Text))
			if !up {
				// A service that sends every box away before hello_ok is
				// not restarting once; the backoff grows as for a failure.
				return max(wait, l.backoff()), nil
			}
			l.mu.Lock()
			l.attempt = 0
			l.mu.Unlock()
			return wait, nil
		}
	}
	if !up {
		l.hostFailed()
	}
	return l.backoff(), err
}

// spread is a uniformly random wait within ms milliseconds.
func spread(ms int64) time.Duration {
	if ms <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(ms*int64(time.Millisecond)))
	if err != nil {
		return time.Duration(ms) * time.Millisecond / 2
	}
	return time.Duration(n.Int64())
}

// endpoint is the uplink at the host the service last named for this home,
// or at Endpoint's own host. A kept host that is not under Endpoint's, as
// after a build moved the service, is not dialled.
func (l *Link) endpoint() string {
	l.mu.Lock()
	host := ""
	if l.link != nil {
		host = l.link.Host
	}
	l.mu.Unlock()
	u, err := url.Parse(l.opts.Endpoint)
	if host == "" || err != nil || !cloudwire.ShardHostOK(u.Hostname(), host) {
		return l.opts.Endpoint
	}
	if port := u.Port(); port != "" {
		host = net.JoinHostPort(host, port)
	}
	u.Host = host
	return u.String()
}

// followShard keeps the host a wrong-shard close named, once. A second
// wrong-shard close before the box is connected again, or a host off the
// service's domain, sends the box back to Endpoint's host after backoff,
// where it may follow once more.
func (l *Link) followShard(host string) bool {
	base := ""
	if u, err := url.Parse(l.opts.Endpoint); err == nil {
		base = u.Hostname()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.link == nil {
		return false
	}
	if l.shardHops > 0 || !cloudwire.ShardHostOK(base, host) {
		l.shardHops = 0
		l.keepHostLocked("")
		return false
	}
	l.shardHops = 1
	l.keepHostLocked(host)
	l.log.Info("FTW Cloud: another instance holds this home; following", "host", host)
	return true
}

// hostFailed counts a dial or handshake that failed at a kept host. After
// cloudwire.ShardHostFailures in a row over at least
// cloudwire.ShardHostFailWindowMs the box forgets it, and Endpoint's host
// names the right one again. The window outlasts a shard's restart, so a
// box does not leave its shard for the default host because of one.
func (l *Link) hostFailed() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.link == nil || l.link.Host == "" {
		return
	}
	now := l.now()
	if l.hostFails == 0 {
		l.hostFailedSince = now
	}
	l.hostFails++
	window := time.Duration(cloudwire.ShardHostFailWindowMs) * time.Millisecond
	if l.hostFails >= cloudwire.ShardHostFailures && now.Sub(l.hostFailedSince) >= window {
		l.log.Info("FTW Cloud: the kept host stopped answering; starting again at the default host")
		l.keepHostLocked("")
	}
}

// keepHostLocked stores the host the uplink dials. Forgetting it starts a
// new chain at Endpoint's host, where one redirect may be followed again.
// The caller holds l.mu.
func (l *Link) keepHostLocked(host string) {
	l.hostFails = 0
	if host == "" {
		l.shardHops = 0
	}
	if l.link.Host == host {
		return
	}
	rec := *l.link
	rec.Host = host
	if err := saveJSON(l.opts.KV, linkKey, rec); err != nil {
		l.log.Warn("could not store the FTW Cloud host", "err", err)
	}
	l.link = &rec
}

// backoff is full jitter from 0.5 s to 60 s.
func (l *Link) backoff() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	ceiling := min(backoffBase<<min(l.attempt, 16), backoffCap)
	l.attempt = min(l.attempt+1, 16)
	n, err := rand.Int(rand.Reader, big.NewInt(int64(ceiling)))
	if err != nil {
		return ceiling / 2
	}
	return time.Duration(n.Int64())
}

// errLinkEnded ends a connection after the service confirmed that the home
// is gone.
var errLinkEnded = errors.New("cloudlink: the end of the link is confirmed")

// The tunnel's two queues, apart from the readable copy. Lane 0, a phone's
// commands, their answers, its readings and its snapshot, goes first and is
// never held. Bulk is paced under the service's limits and waits its turn. When a
// queue is full the newest message is dropped: Noise transport carries its
// own counter, so a lost message costs the phone one frame.
const (
	tunQueue     = 64
	tunBulkQueue = 256
	// tunPace is the share of the service's limits bulk is paced to, so
	// the service never drops for a box that keeps to it.
	tunPace = 0.9
)

// serve runs one connection: the handshake, then the reader here and the
// sender on its own goroutine.
func (l *Link) serve(ctx context.Context, conn uplinkConn, snap grantSnapshot) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	ekm, err := exporterValue(conn)
	if err != nil {
		return err
	}
	conn.SetReadLimit(cloudwire.MaxTunFrame)

	// 1. challenge
	_ = conn.SetReadDeadline(time.Now().Add(handshakeTimeout))
	env, err := readEnvelope(conn)
	if err != nil {
		return err
	}
	var ch cloudwire.Challenge
	if env.T != cloudwire.MsgChallenge || len(env.Sig) != 0 || cloudwire.Unmarshal(env.B, &ch) != nil ||
		ch.V != cloudwire.Version || len(ch.Nonce) != cloudwire.NonceBytes {
		return fmt.Errorf("%w: expected a challenge", errService)
	}

	// 2. hello, the only thing sent before the service has proved itself.
	boxNonce, err := randomBytes(cloudwire.NonceBytes)
	if err != nil {
		return err
	}
	rosterSeq, _ := l.rosterNumber(snap)
	l.mu.Lock()
	hello := cloudwire.Hello{
		Header: l.header(), V: cloudwire.Version, PK: snap.pk, Nonce: ch.Nonce, EKM: ekm,
		BoxNonce: boxNonce, Build: l.opts.Build, Clock: l.clockName(),
		Caps: l.caps(snap), RosterSeq: rosterSeq, GrantID: snap.grantID(),
	}
	l.mu.Unlock()
	w := &writer{conn: conn, link: l}
	if err := w.send(snap, cloudwire.MsgHello, hello); err != nil {
		return err
	}

	// 3. hello_ok, signed by a key this build carries, bound to this TLS
	// session and to this hello.
	env, err = readEnvelope(conn)
	if err != nil {
		return err
	}
	var ok cloudwire.HelloOK
	if env.T != cloudwire.MsgHelloOK || !l.verifiedByService(snap.home, env) ||
		cloudwire.Unmarshal(env.B, &ok) != nil ||
		!bytes.Equal(ok.BoxNonce, boxNonce) || !bytes.Equal(ok.EKM, ekm) {
		return fmt.Errorf("%w: hello_ok did not check out", errService)
	}
	if ok.RosterSeq > rosterSeq {
		// A restore from an older backup: move past the service's number
		// so revokes made since then still arrive.
		l.bumpRoster(ok.RosterSeq)
	}
	w.have = ok.Have

	tunOut, tunBulk := make(chan []byte, tunQueue), make(chan []byte, tunBulkQueue)
	l.mu.Lock()
	l.connected = true
	l.serviceStatsPolicy = ok.StatsPolicy
	l.upAt = time.Now()
	l.transport = transportOf(conn)
	l.shardHops, l.hostFails = 0, 0
	if l.opts.Tunnel != nil {
		l.tunOut, l.tunBulk = tunOut, tunBulk
	}
	l.mu.Unlock()
	l.log.Info("FTW Cloud uplink connected", "home", snap.home, "transport", transportOf(conn))
	defer func() {
		l.mu.Lock()
		l.tunOut, l.tunBulk = nil, nil
		l.mu.Unlock()
		// Every session the uplink carried ends with it. The phone starts
		// again on the next one.
		if l.opts.Tunnel != nil {
			l.opts.Tunnel.Drop()
		}
	}()

	events := make(chan cloudwire.Envelope, 4)
	readErr := make(chan error, 1)
	go func() {
		readErr <- l.read(conn, snap, events)
	}()
	sendErr := make(chan error, 1)
	go func() {
		sendErr <- w.run(ctx, events)
	}()
	tunErr := make(chan error, 1)
	go func() {
		tunErr <- w.tunnel(ctx, tunOut, tunBulk)
	}()

	// The first to end ends the others: cancel closes the socket under the
	// reader, and the writers stop on the context.
	pending := []chan error{readErr, sendErr, tunErr}
	select {
	case err = <-readErr:
		pending = pending[1:]
	case err = <-sendErr:
		pending = []chan error{readErr, tunErr}
	case err = <-tunErr:
		pending = pending[:2]
	}
	cancel()
	for _, ch := range pending {
		<-ch
	}
	return err
}

func (l *Link) verifiedByService(home string, env cloudwire.Envelope) bool {
	for _, k := range l.opts.ServiceKeys {
		if cloudwire.Verify(k, home, env) == nil {
			return true
		}
	}
	return false
}

func (l *Link) clockName() string {
	if l.clockSynced() {
		return cloudwire.ClockNTP
	}
	return cloudwire.ClockNone
}

// readEnvelope reads one binary frame. A text message, a lane 0 frame, a tun
// frame over MaxTunFrame or any other over 4 KiB is the service breaking the
// protocol.
func readEnvelope(conn uplinkConn) (cloudwire.Envelope, error) {
	kind, msg, err := conn.ReadMessage()
	if err != nil {
		return cloudwire.Envelope{}, err
	}
	return parseDownlink(kind, msg)
}

func parseDownlink(kind int, msg []byte) (cloudwire.Envelope, error) {
	if kind != websocket.BinaryMessage && kind != msgBulk {
		// A text message, or a datagram: the service sends the box none.
		return cloudwire.Envelope{}, fmt.Errorf("%w: a message of type %d", errService, kind)
	}
	if len(msg) > cloudwire.MaxTunFrame {
		return cloudwire.Envelope{}, fmt.Errorf("%w: %d bytes", errService, len(msg))
	}
	payload, err := cloudwire.DecodeFrame(msg)
	if err != nil {
		return cloudwire.Envelope{}, fmt.Errorf("%w: %v", errService, err)
	}
	env, err := cloudwire.DecodeEnvelope(payload)
	if err != nil {
		return cloudwire.Envelope{}, fmt.Errorf("%w: %v", errService, err)
	}
	if !slices.Contains(cloudwire.Downlink, env.T) {
		return cloudwire.Envelope{}, fmt.Errorf("%w: %q is not a message the service may send", errService, env.T)
	}
	if kind == msgBulk && env.T != cloudwire.MsgTun {
		return cloudwire.Envelope{}, fmt.Errorf("%w: a %s on the bulk stream", errService, env.T)
	}
	if env.T != cloudwire.MsgTun && len(msg) > cloudwire.MaxServiceFrame {
		return cloudwire.Envelope{}, fmt.Errorf("%w: a %d-byte %s", errService, len(msg), env.T)
	}
	return env, nil
}

// read is the downlink. After the handshake the service may send watch,
// resync, purged, tun and x; challenge and hello_ok a second time are refused
// like any stranger. watch and resync go to the sender, which owns the
// cadence. A purge receipt is taken here, before the service hangs up, so the
// box never misses one it was sent. A tun goes to the Tunnel and an x to the
// Exchange, unread.
func (l *Link) read(conn uplinkConn, snap grantSnapshot, events chan<- cloudwire.Envelope) error {
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeTimeout))
	})
	for {
		env, err := readEnvelope(conn)
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		switch env.T {
		case cloudwire.MsgPurged:
			if l.acceptPurged(snap, env) {
				return errLinkEnded
			}
		case cloudwire.MsgTun:
			var t cloudwire.Tun
			if len(env.Sig) != 0 || cloudwire.Unmarshal(env.B, &t) != nil || len(t.D) > cloudwire.TunDataMax {
				return fmt.Errorf("%w: a tun the box cannot read", errService)
			}
			if l.TakesTun() {
				l.opts.Tunnel.Deliver(t)
			}
		case cloudwire.MsgX:
			var x cloudwire.X
			if len(env.Sig) != 0 || cloudwire.Unmarshal(env.B, &x) != nil || len(x.D) > cloudwire.XDataMax || x.Dev == "" {
				return fmt.Errorf("%w: an x the box cannot read", errService)
			}
			if l.opts.Exchange != nil && l.opts.Tunnel != nil {
				l.opts.Exchange.Deliver(x)
			}
		case cloudwire.MsgWatch, cloudwire.MsgResync:
			events <- env
		default:
			return fmt.Errorf("%w: %q after the handshake", errService, env.T)
		}
	}
}

// writer is the one goroutine that writes data frames on a connection.
type writer struct {
	conn uplinkConn
	link *Link
	mu   sync.Mutex
	// have is the closed tiles hello_ok said the service holds.
	have map[string]string
}

// send seals body as type t, frames it and writes it. The byte budget is
// the caller's to check.
func (w *writer) send(snap grantSnapshot, t string, body any) error {
	raw, err := cloudwire.Marshal(body)
	if err != nil {
		return err
	}
	env, err := cloudwire.SealBytes(snap.key, snap.home, t, raw)
	if err != nil {
		return err
	}
	payload, err := env.Encode()
	if err != nil {
		return err
	}
	frame, err := cloudwire.EncodeFrame(payload)
	if err != nil {
		return err
	}

	// Over QUIC an hb, or a live without its table, goes as a datagram: the
	// newest wins, and a lost one shows as a gap on the phone.
	kind := websocket.BinaryMessage
	if cloudwire.Datagram(env) {
		kind = msgDatagram
	}
	if err := w.writeAs(kind, frame); err != nil {
		return err
	}
	l := w.link
	l.mu.Lock()
	l.lastSend = time.Now()
	l.budget.add(l.uptimeMs(), int64(len(frame)))
	l.sent[t] = sentFrame{T: t, Frame: frame, AtMs: l.uptimeMs()}
	l.mu.Unlock()
	return nil
}

// Why a tun did not leave. ErrNoUplink means none will until the box
// reconnects; ErrTunDropped means this one was lost, to a full queue or the
// tunnel's own budget, and was counted. Noise transport survives a lost
// message, so a session goes on after ErrTunDropped.
var (
	ErrNoUplink         = errors.New("cloudlink: no uplink to FTW Cloud")
	ErrTunDropped error = tunDropped{}
)

// tunDropped says, through Dropped, that one message was lost and counted,
// so a caller that may not import this package can tell it from a lost
// uplink.
type tunDropped struct{}

func (tunDropped) Error() string { return "cloudlink: a tun was dropped" }
func (tunDropped) Dropped() bool { return true }

// SendTun queues one tun for the uplink. bulk says it carries a lane 1 frame,
// which is paced; anything else goes first. It never blocks. nil means the
// message is queued.
func (l *Link) SendTun(t cloudwire.Tun, bulk bool) error {
	frame, err := tunFrame(t)
	l.mu.Lock()
	defer l.mu.Unlock()
	q := l.tunOut
	if bulk {
		q = l.tunBulk
	}
	switch {
	case q == nil || l.disabled || l.grant.statisticsOnly():
		l.tunDropped++
		return ErrNoUplink
	case err != nil || l.tunBudget.total(l.uptimeMs()) >= tunBudgetBytes:
		l.tunDropped++
		return ErrTunDropped
	}
	select {
	case q <- frame:
		return nil
	default:
		l.tunDropped++
		return ErrTunDropped
	}
}

// SendX queues a watch's exchange answer for the uplink. It is small and goes
// with the tun lane 0 frames, first and never held, under the tun budget and
// a share of its own (xAllowedLocked). It never blocks. nil means the message
// is queued.
func (l *Link) SendX(x cloudwire.X) error {
	frame, err := xFrame(x)
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.tunOut == nil:
		l.tunDropped++
		return ErrNoUplink
	case err != nil || !l.xAllowedLocked():
		l.tunDropped++
		return ErrTunDropped
	}
	select {
	case l.tunOut <- frame:
		l.xBudget.add(l.uptimeMs(), int64(len(frame)))
		return nil
	default:
		l.tunDropped++
		return ErrTunDropped
	}
}

// XAllowed reports whether a watch's exchange answer may leave now. The
// exchange asks before it runs a request, so a command never acts when its
// answer could not go.
func (l *Link) XAllowed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tunOut != nil && l.xAllowedLocked()
}

// xAllowedLocked keeps x to xBudgetBytes over a rolling 24 hours, and stops
// it once the tunnel has spent xStopsAt of its own budget, so the phones keep
// the rest for their handshakes, commands and ends. The caller holds l.mu.
func (l *Link) xAllowedLocked() bool {
	up := l.uptimeMs()
	return l.xBudget.total(up) < xBudgetBytes && l.tunBudget.total(up) < int64(xStopsAt*tunBudgetBytes)
}

func xFrame(x cloudwire.X) ([]byte, error) {
	if len(x.D) > cloudwire.XDataMax {
		return nil, ErrTunDropped
	}
	body, err := cloudwire.Marshal(x)
	if err != nil {
		return nil, err
	}
	raw, err := cloudwire.Envelope{T: cloudwire.MsgX, B: body}.Encode()
	if err != nil {
		return nil, err
	}
	return cloudwire.EncodeFrame(raw)
}

func tunFrame(t cloudwire.Tun) ([]byte, error) {
	if len(t.D) > cloudwire.TunDataMax {
		return nil, ErrTunDropped
	}
	body, err := cloudwire.Marshal(t)
	if err != nil {
		return nil, err
	}
	raw, err := cloudwire.Envelope{T: cloudwire.MsgTun, B: body}.Encode()
	if err != nil {
		return nil, err
	}
	return cloudwire.EncodeFrame(raw)
}

// MaxBulkWait is the longest a bulk tun can wait behind the pacer: a full
// bulk queue of the largest messages, paced to tunPace of the service's byte
// limit. A phone's Noise transport takes a message at most
// appwire.ReplayWindow behind the newest, so a session must send fewer lane
// 0 frames than that while one bulk message waits.
func MaxBulkWait() time.Duration {
	seconds := float64(tunBulkQueue*cloudwire.MaxTunFrame) / (tunPace * cloudwire.TunBytesPerSecond)
	return time.Duration(seconds * float64(time.Second))
}

// TunBudgetUsed is the share of the tunnel's 24-hour budget spent, from 0
// to 1 and past it when the budget is gone.
func (l *Link) TunBudgetUsed() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return float64(l.tunBudget.total(l.uptimeMs())) / tunBudgetBytes
}

// pacer keeps bulk under the service's tun limits: a message bucket and a
// byte bucket at tunPace of them. Taking may leave either in debt; the wait
// is how long until it is paid.
type pacer struct {
	msgs, bytes float64
	at          time.Time
}

func newPacer(now time.Time) *pacer {
	return &pacer{msgs: tunPace * cloudwire.TunMsgBurst, bytes: tunPace * cloudwire.TunByteBurst, at: now}
}

func (p *pacer) take(n int, now time.Time) time.Duration {
	dt := now.Sub(p.at).Seconds()
	p.at = now
	p.msgs = min(tunPace*cloudwire.TunMsgBurst, p.msgs+dt*tunPace*cloudwire.TunMsgsPerSecond) - 1
	p.bytes = min(tunPace*cloudwire.TunByteBurst, p.bytes+dt*tunPace*cloudwire.TunBytesPerSecond) - float64(n)
	wait := max(-p.msgs/(tunPace*cloudwire.TunMsgsPerSecond), -p.bytes/(tunPace*cloudwire.TunBytesPerSecond), 0)
	return time.Duration(wait * float64(time.Second))
}

// tunnel writes queued tun frames as they come, beside the readable copy,
// so a phone's session never waits for the copy's cadence. Lane 0 goes
// first and at once, even while bulk waits for the pacer. It counts both
// against the tunnel's budget, never the copy's.
func (w *writer) tunnel(ctx context.Context, out, bulk <-chan []byte) error {
	pace := newPacer(time.Now())
	// Lane 0 goes on the control stream, bulk on the bulk stream over QUIC,
	// so a lost bulk packet never holds up a command or its answer.
	send := func(frame []byte) error { return w.tunAs(websocket.BinaryMessage, frame) }
	sendBulk := func(frame []byte) error { return w.tunAs(msgBulk, frame) }
	for {
		select {
		case frame := <-out:
			pace.take(len(frame), time.Now())
			if err := send(frame); err != nil {
				return err
			}
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame := <-out:
			pace.take(len(frame), time.Now())
			if err := send(frame); err != nil {
				return err
			}
		case frame := <-bulk:
			if wait := pace.take(len(frame), time.Now()); wait > 0 {
				timer := time.NewTimer(wait)
			held:
				for {
					select {
					case <-ctx.Done():
						timer.Stop()
						return ctx.Err()
					case urgent := <-out:
						if err := send(urgent); err != nil {
							timer.Stop()
							return err
						}
					case <-timer.C:
						break held
					}
				}
			}
			if err := sendBulk(frame); err != nil {
				return err
			}
		}
	}
}

// tunAs writes one tun frame the way kind says and counts it against the
// tunnel's budget, never the copy's.
func (w *writer) tunAs(kind int, frame []byte) error {
	if err := w.writeAs(kind, frame); err != nil {
		return err
	}
	l := w.link
	l.mu.Lock()
	l.tunBudget.add(l.uptimeMs(), int64(len(frame)))
	l.mu.Unlock()
	return nil
}

// write sends one frame under the socket's one write lock.
func (w *writer) write(frame []byte) error { return w.writeAs(websocket.BinaryMessage, frame) }

// writeAs sends one frame the way kind says where the connection carries
// it: over QUIC, on the bulk stream or as a datagram. A WebSocket carries
// every kind as a binary message.
func (w *writer) writeAs(kind int, frame []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	return w.conn.WriteMessage(kindFor(w.conn, kind), frame)
}
