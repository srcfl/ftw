// Package appuplink keeps the box's outbound connection to the blind relay,
// and runs the app sessions that arrive over it.
//
// The relay is the only remote path between a box and a phone, and it cannot
// read a watt: it joins one box uplink to a handful of browser streams under a
// rendezvous handle and moves opaque bytes between them. Everything that makes
// those bytes meaningful is in go/internal/appwire (the Noise session) and
// go/internal/appproto (the messages). See srcfl/ftw-webapp relay/README.md
// for the server's half.
//
// One rule shapes the whole file: a dropped connection is this package's
// problem. There is no reconnect button in the app and no place to put one, so
// every failure here retries on its own and the user sees nothing but a
// freshness stamp slipping behind while it is being solved.
package appuplink

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
)

// Endpoint is the production relay. Not configurable: a box that can be
// pointed at another relay is a box a support call can point at an attacker's,
// and the relay is blind anyway, so there is nothing to gain by choosing one.
const Endpoint = "wss://relay.ftw.energy"

// The relay's close codes and control words. Held here rather than shared with
// the server so the box builds without it; relay_contract_test.go fails if
// they drift from srcfl/ftw-webapp relay/src/protocol.ts.
const (
	CloseBadJoin = 4400
	CloseEpoch   = 4409
	CloseRotated = 4410
	CloseBusy    = 4429

	CtrlReady = "ready"
	CtrlGone  = "gone"
)

const (
	dialTimeout = 15 * time.Second
	// writeTimeout is generous because a stalled write is a dead socket, not
	// a slow one, and the reconnect path handles dead sockets well.
	writeTimeout = 10 * time.Second

	// readTimeout is three relay heartbeats. The relay pings every fifteen
	// seconds; missing three means the socket is gone whatever TCP thinks,
	// which is the case a NAT drop produces and neither end otherwise sees.
	readTimeout = 45 * time.Second

	// backoffBase is where the first retry lands.
	backoffBase = 500 * time.Millisecond
	// backoffCap keeps a box off a struggling relay without letting it sulk.
	backoffCap = 60 * time.Second

	// rotateJitter is how long a box waits before rejoining after an epoch
	// turned over.
	//
	// Not zero, and this is the point of it. The relay closes the old handle
	// and the new one appears when the peer comes back; returning instantly
	// lets a relay watching the clock line the two epochs up by timing alone,
	// which is the correlation rotation exists to prevent. The relay already
	// spreads the closes across five minutes, so a few seconds here is enough
	// to put other households' rotations in between — and short enough that
	// the phone in someone's hand does not notice.
	rotateJitter = 5 * time.Second

	// correctionLimit is how many epoch corrections we take at full speed
	// before backing off. More than a couple means something is wrong rather
	// than stale.
	correctionLimit = 2

	// maxEpochCorrection bounds how far the relay may move our clock, in
	// epochs. An hour either way covers a box whose RTC drifted; it does not
	// cover a relay steering us onto handles of its choosing so it can build
	// a table of this household's future names.
	maxEpochCorrection = 1

	// maxFrameBytes matches the relay's own ceiling. A larger message is
	// refused there anyway; refusing it here keeps the memory bound local.
	maxFrameBytes = 65536

	// tickInterval is lane 0's cadence, fixed for the life of a session.
	tickInterval = time.Second
)

// Options wires the uplink. Everything except the injectable seams is required.
type Options struct {
	// Endpoint is the relay origin, no path. Defaults to Endpoint.
	Endpoint string

	// Enroll owns the static key, the rendezvous secret and the record of
	// which phones are allowed in.
	Enroll *appenroll.Identity

	// Handler builds one message-layer handler per app session. The caller
	// supplies it so this package needs to know nothing about telemetry,
	// control or the planner.
	Handler func(appproto.Sender) (*appproto.Handler, error)

	Logger *slog.Logger

	// Injectable seams. Production leaves them nil.
	Dialer *websocket.Dialer
	Now    func() time.Time
	Random func() float64
}

// Uplink maintains the connection and the sessions on it.
type Uplink struct {
	opts   Options
	log    *slog.Logger
	dialer *websocket.Dialer
	now    func() time.Time
	random func() float64

	// epochOffset is the difference between the relay's clock and ours,
	// learned from a correction. An offset rather than a remembered epoch
	// number, so it stays right as time passes instead of going stale the
	// moment the hour turns. A box with a working clock carries zero here
	// for its whole life.
	epochOffset int64
	attempt     int
	corrections int

	// liveMu guards live, the sessions of the current relay connection.
	// Kept on the uplink so a revoke can reach into whatever connection is
	// up right now; nil between connections, when there is nothing to drop.
	liveMu sync.Mutex
	live   *sessions
}

// DropSessionsByAppKey tears down every live session a revoked key holds.
// Returns how many went; zero is normal when the phone is not connected.
func (u *Uplink) DropSessionsByAppKey(pub []byte) int {
	u.liveMu.Lock()
	live := u.live
	u.liveMu.Unlock()
	if live == nil {
		return 0
	}
	return live.dropByAppKey(pub)
}

// New builds an uplink.
func New(opts Options) (*Uplink, error) {
	switch {
	case opts.Enroll == nil:
		return nil, errors.New("appuplink: Enroll is required")
	case opts.Handler == nil:
		return nil, errors.New("appuplink: Handler is required")
	}

	endpoint := opts.Endpoint
	if endpoint == "" {
		endpoint = Endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" {
		return nil, fmt.Errorf("appuplink: %q is not a relay origin", endpoint)
	}
	if parsed.Scheme != "wss" && parsed.Scheme != "ws" {
		return nil, fmt.Errorf("appuplink: %q is not a WebSocket scheme", parsed.Scheme)
	}
	opts.Endpoint = strings.TrimRight(endpoint, "/")

	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	u := &Uplink{
		opts:   opts,
		log:    opts.Logger.With("component", "appuplink"),
		dialer: opts.Dialer,
		now:    opts.Now,
		random: opts.Random,
	}
	if u.dialer == nil {
		u.dialer = productionDialer()
	}
	if u.now == nil {
		u.now = time.Now
	}
	if u.random == nil {
		u.random = randomFloat
	}
	return u, nil
}

func productionDialer() *websocket.Dialer {
	return &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: dialTimeout,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
		// A compressed frame's length depends on its content, which would
		// undo the padding that keeps the household's load pattern off the
		// wire. The relay refuses it too; refusing it here as well means
		// neither end can be talked into it alone.
		EnableCompression: false,
	}
}

// Run keeps the uplink up until the context ends.
func (u *Uplink) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		delay, err := u.once(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			u.log.Warn("app uplink dropped; retrying", "retry_in", delay, "err", err)
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// once dials, serves until the connection ends, and says how long to wait.
func (u *Uplink) once(ctx context.Context) (time.Duration, error) {
	epoch := CurrentEpoch(u.now().UnixMilli()) + u.epochOffset
	handle, err := Handle(u.opts.Enroll.RendezvousSecret(), epoch)
	if err != nil {
		// Unrecoverable and not the network's fault. Back off rather than
		// spin; the operator sees the log and nothing else changes.
		return backoffCap, err
	}

	target := fmt.Sprintf("%s/r/%d/%s/box", u.opts.Endpoint, epoch, handle)

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, response, err := u.dialer.DialContext(dialCtx, target, nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return u.backoff(), fmt.Errorf("appuplink: dialling the relay: %w", err)
	}

	// A connection that lasted is not a connection that is failing. Without
	// this, an hourly rotation would compound the backoff until a box that
	// works perfectly waits a minute after every rotation.
	connectedAt := u.now()
	code, reason, serveErr := u.serve(ctx, conn)
	if u.now().Sub(connectedAt) >= backoffCap {
		u.attempt = 0
	}

	switch code {
	case CloseRotated:
		u.adoptEpoch(reason)
		u.attempt = 0
		u.corrections = 0
		u.log.Info("relay epoch rotated", "epoch", CurrentEpoch(u.now().UnixMilli())+u.epochOffset)
		return time.Duration(u.random() * float64(rotateJitter)), nil
	case CloseEpoch:
		u.adoptEpoch(reason)
		u.corrections++
		if u.corrections > correctionLimit {
			return u.backoff(), fmt.Errorf("appuplink: the relay keeps refusing our epoch")
		}
		// Straight back with the right number. A box whose RTC reads 1970 is
		// right after one round trip.
		return 0, nil
	default:
		return u.backoff(), serveErr
	}
}

// serve runs one connection. It returns the close code and reason when the
// relay closed us, so the caller can tell a rotation from a fault.
func (u *Uplink) serve(ctx context.Context, conn *websocket.Conn) (int, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var writeMu sync.Mutex
	write := func(message []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		// Socket deadlines take the real clock, never u.now. u.now answers
		// "which epoch is it", and a test that pins it to an hour boundary
		// would otherwise set every deadline in the past.
		if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return err
		}
		return conn.WriteMessage(websocket.BinaryMessage, message)
	}

	live := &sessions{
		build:  u.opts.Handler,
		enroll: u.opts.Enroll,
		static: u.opts.Enroll.StaticKey(),
		write:  write,
		log:    u.log,
	}
	u.liveMu.Lock()
	u.live = live
	u.liveMu.Unlock()
	defer func() {
		u.liveMu.Lock()
		u.live = nil
		u.liveMu.Unlock()
		live.closeAll()
	}()

	conn.SetReadLimit(maxFrameBytes)
	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readTimeout))
	})

	// The context ending has to reach a blocked read, and gorilla has no
	// other way to interrupt one.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				live.tick()
			}
		}
	}()

	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				return closeErr.Code, closeErr.Text, nil
			}
			return 0, "", err
		}
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))

		switch kind {
		case websocket.BinaryMessage:
			live.deliver(ctx, message)
		case websocket.TextMessage:
			// The relay says exactly two words, and neither is routed. Any
			// third word is a relay this box does not understand, which is
			// not a reason to drop a working connection.
			switch string(message) {
			case CtrlGone:
				// The last app left. Its Noise session is gone with it —
				// the app restarts the handshake on reconnect — so holding
				// ours would be memory nobody can decrypt into.
				live.closeAll()
			case CtrlReady:
				u.log.Debug("an app joined the room")
			default:
				u.log.Debug("ignoring an unknown relay control word")
			}
		}
	}
}

// adoptEpoch takes the relay's announced epoch as a clock correction, never as
// an order.
//
// The handle is derived from the epoch, so a relay that can name any epoch can
// make this box compute and publish handle(secret, N) for every N it likes,
// building a table of the household's future names. So: parse strictly, and
// accept only a correction small enough to be a real clock difference.
func (u *Uplink) adoptEpoch(announced string) {
	text := strings.TrimSpace(announced)
	if !epochPattern.MatchString(text) {
		return
	}
	epoch, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return
	}

	offset := epoch - CurrentEpoch(u.now().UnixMilli())
	if offset > maxEpochCorrection || offset < -maxEpochCorrection {
		u.log.Warn("ignoring an implausible epoch from the relay", "offset", offset)
		return
	}
	u.epochOffset = offset
}

var epochPattern = regexp.MustCompile(`^-?\d+$`)

// backoff is full jitter: the whole delay is random, so a fleet coming back
// after a relay restart never resynchronises into a second outage.
func (u *Uplink) backoff() time.Duration {
	ceiling := backoffBase << min(u.attempt, 16)
	if ceiling > backoffCap {
		ceiling = backoffCap
	}
	u.attempt = min(u.attempt+1, 16)
	return time.Duration(u.random() * float64(ceiling))
}

// randomFloat draws from the system source. math/rand would do for jitter, but
// a single source for everything in this package is one fewer thing to reason
// about when the next use is not jitter.
func randomFloat() float64 {
	const precision = 1 << 53
	n, err := rand.Int(rand.Reader, big.NewInt(precision))
	if err != nil {
		// The system entropy source failing is not something jitter should
		// turn into an outage. Half the ceiling is a fine delay.
		return 0.5
	}
	return float64(n.Int64()) / precision
}
