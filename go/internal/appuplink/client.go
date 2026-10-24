// Package appuplink keeps the box's outbound connection to the content-blind
// relay, and runs the app sessions that arrive over it.
//
// The relay is the only remote path between a box and a phone, and it cannot
// read a watt: it joins one box uplink to a handful of browser streams under a
// rendezvous handle and moves opaque bytes between them. Everything that makes
// those bytes meaningful is in go/protocol/appwire (the Noise session) and
// go/protocol (the messages). See srcfl/ftw-webapp relay/README.md
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
)

// Endpoint is the production relay. Not configurable: a box that can be
// pointed at another relay is a box a support call can point at an attacker's,
// and the relay is fixed by the protocol, so there is nothing to gain by
// choosing one.
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

	// defaultReadTimeout is three relay heartbeats. The relay pings every fifteen
	// seconds; missing three means the socket is gone whatever TCP thinks,
	// which is the case a NAT drop produces and neither end otherwise sees.
	defaultReadTimeout = 45 * time.Second

	// backoffBase is where the first retry lands.
	backoffBase = 500 * time.Millisecond
	// backoffCap keeps a box off a struggling relay without letting it sulk.
	backoffCap = 60 * time.Second

	// rotateJitter is the longest a box waits before it joins a new epoch's
	// handle: after its clock turns the hour, or after the relay closed the
	// old handle.
	//
	// Not zero. Every box turns at the same hour, so this spreads their
	// joins, and coming back the instant the relay closes the old handle
	// gives it a stronger timing link between the two protocol handles. It
	// cannot hide IP or connection continuity.
	rotateJitter = 5 * time.Second

	// turnRetry is how long a box waits to try the new hour's handle again
	// when the relay refused it with the old epoch. The relay reads its
	// clock on its fifteen-second heartbeat, so it can still be on the old
	// hour that long after ours turned; turnTries tries cover that. After
	// them the box waits for the relay to close the old room, as it always
	// has.
	turnRetry = 5 * time.Second
	turnTries = 4

	// turnCheck is how often a connected box looks at its clock for a new
	// hour.
	turnCheck = time.Second

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

	// tickInterval is lane 0's cadence over the relay, fixed for the life
	// of a session.
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
	Handler HandlerBuilder

	// ExchangeHandler builds one handler per watch session, with
	// appproto.Config.Exchange set (ADR 0009). Nil opens no exchange: the
	// box takes no x from FTW Cloud, and nothing else changes.
	ExchangeHandler HandlerBuilder

	Logger *slog.Logger

	// Arrivals hears each finished driver poll (telemetry.Store.Arrivals).
	// Sessions through FTW Cloud send on it; nil leaves them on the beat
	// alone, one frame per subscribed period. Relay sessions never use it.
	Arrivals <-chan struct{}

	// Deadman, when set, arms the dead man's switch: rows posted to the
	// relay's HTTPS side, claims spoken on every connection. Nil leaves
	// the uplink exactly as it was — no rows, no claims, no text frames.
	Deadman DeadmanSource

	// Injectable seams. Production leaves them nil (and zero).
	Dialer      *websocket.Dialer
	Now         func() time.Time
	Random      func() float64
	DeadmanHTTP *http.Client
	// DeadmanRefresh shortens the six-hour re-signing cadence in tests.
	DeadmanRefresh time.Duration
}

// HandlerBuilder makes one message-layer handler for one app session.
//
// The caller is passed in rather than looked up later, because the handshake
// has just authenticated the device and that is the only moment the answer is
// certain. The grant reader is passed beside it because the caller is a
// snapshot and a revoke can land a second afterwards: the handler re-reads
// the enrolment on every privileged request, and refuses one whose epoch has
// moved.
type HandlerBuilder func(appproto.Sender, apiauth.Caller, appproto.GrantReader) (*appproto.Handler, error)

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

	// liveMu guards live, the relay connections up right now, oldest first.
	// Usually one; two for the few minutes after the hour while the box
	// holds the old handle and the new. Kept on the uplink so a revoke can
	// reach whatever is up right now; empty between connections, when there
	// is nothing to drop and nothing to say.
	liveMu sync.Mutex
	live   []*liveLink

	// exchange is the watches' door through FTW Cloud and their table of
	// sessions, nil without an ExchangeHandler. One for the box, so a
	// watch's session survives the hour turning under it.
	exchange *Exchange

	// slots holds each device's one session, across the relay's
	// connections and the tunnel through FTW Cloud.
	slots *slots
	// tunnel is the table of sessions FTW Cloud carries. It runs whether or
	// not the relay can be reached.
	tunnel *Tunnel

	// The dead man's switch bookkeeping: which row ids the relay holds,
	// so a removed subscription's row is deleted rather than left armed.
	deadmanMu      sync.Mutex
	deadmanPosted  map[string]bool
	deadmanOrigin  string
	deadmanHTTP    *http.Client
	deadmanRefresh time.Duration
	readTimeout    time.Duration
	turnCheck      time.Duration
	turnRetry      time.Duration
}

// liveLink is one relay connection's sessions and its text-frame writer,
// which the deadman claims ride on.
type liveLink struct {
	sessions *sessions
	text     func(string) error
}

// DropSessionsByAppKey tears down every live session a revoked key holds.
// Returns how many went; zero is normal when the phone is not connected.
func (u *Uplink) DropSessionsByAppKey(pub []byte) int {
	u.liveMu.Lock()
	live := slices.Clone(u.live)
	u.liveMu.Unlock()
	dropped := 0
	for _, l := range live {
		dropped += l.sessions.dropByAppKey(pub)
	}
	if u.exchange != nil {
		dropped += u.exchange.table.dropByAppKey(pub)
	}
	dropped += u.tunnel.dropByAppKey(pub)
	return dropped
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
		opts:           opts,
		log:            opts.Logger.With("component", "appuplink"),
		dialer:         opts.Dialer,
		now:            opts.Now,
		random:         opts.Random,
		deadmanOrigin:  deadmanOrigin(opts.Endpoint),
		deadmanHTTP:    opts.DeadmanHTTP,
		deadmanPosted:  map[string]bool{},
		deadmanRefresh: opts.DeadmanRefresh,
		readTimeout:    defaultReadTimeout,
		turnCheck:      turnCheck,
		turnRetry:      turnRetry,
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
	if u.deadmanHTTP == nil {
		u.deadmanHTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if u.deadmanRefresh <= 0 {
		u.deadmanRefresh = deadmanRefreshInterval
	}
	u.slots = &slots{}
	u.tunnel = newTunnel(opts.Handler, opts.Enroll, u.slots, opts.Arrivals, u.log)
	if opts.ExchangeHandler != nil {
		u.exchange = newExchange(&exchanges{
			build:    opts.ExchangeHandler,
			enroll:   opts.Enroll,
			static:   opts.Enroll.StaticKey(),
			log:      u.log,
			now:      time.Now,
			cmdFloor: ExchangeCmdFloor,
		}, u.log)
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

// Run serves phone sessions and watch exchanges carried by FTW Cloud until
// the context ends. It opens no connection to the retired relay.
func (u *Uplink) Run(ctx context.Context) error {
	if u.exchange != nil {
		go u.exchange.Run(ctx)
	}
	u.tunnel.Run(ctx)
	return ctx.Err()
}

// once dials, serves until the connection ends, and says how long to wait.
func (u *Uplink) once(ctx context.Context) (time.Duration, error) {
	epoch := u.epoch()
	handle, err := Handle(u.opts.Enroll.RendezvousSecret(), epoch)
	if err != nil {
		// Unrecoverable and not the network's fault. Back off rather than
		// spin; the operator sees the log and nothing else changes.
		return backoffCap, err
	}

	conn, err := u.dial(ctx, epoch, handle)
	if err != nil {
		return u.backoff(), fmt.Errorf("appuplink: dialling the relay: %w", err)
	}

	// A connection that lasted is not a connection that is failing. Without
	// this, an hourly rotation would compound the backoff until a box that
	// works perfectly waits a minute after every rotation.
	connectedAt := u.now()
	end := u.ride(ctx, u.start(ctx, conn, epoch, handle))
	if u.now().Sub(connectedAt) >= backoffCap {
		u.attempt = 0
	}

	switch end.code {
	case CloseRotated:
		u.adoptEpoch(end.reason)
		u.attempt = 0
		u.corrections = 0
		u.log.Info("relay epoch rotated", "epoch", u.epoch())
		return time.Duration(u.random() * float64(rotateJitter)), nil
	case CloseEpoch:
		u.adoptEpoch(end.reason)
		u.corrections++
		if u.corrections > correctionLimit {
			return u.backoff(), fmt.Errorf("appuplink: the relay keeps refusing our epoch")
		}
		// Straight back with the right number. A box whose RTC reads 1970 is
		// right after one round trip.
		return 0, nil
	default:
		return u.backoff(), end.err
	}
}

// epoch is the epoch the box would join now: its clock's guess, corrected by
// what the relay last told it.
func (u *Uplink) epoch() int64 {
	return CurrentEpoch(u.now().UnixMilli()) + u.epochOffset
}

// dial opens a socket under one epoch's handle. The relay answers a wrong
// epoch after the upgrade, with a close, so an open socket is not yet an
// accepted one.
func (u *Uplink) dial(ctx context.Context, epoch int64, handle string) (*websocket.Conn, error) {
	target := fmt.Sprintf("%s/r/%d/%s/box", u.opts.Endpoint, epoch, handle)

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, response, err := u.dialer.DialContext(dialCtx, target, nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	return conn, err
}

// link is one relay connection, served on its own goroutine.
type link struct {
	epoch int64
	stop  context.CancelFunc
	done  chan linkEnd
}

// linkEnd is how a connection ended: the relay's close code and reason, or
// the error when it ended some other way.
type linkEnd struct {
	code   int
	reason string
	err    error
}

func (u *Uplink) start(ctx context.Context, conn *websocket.Conn, epoch int64, handle string) *link {
	ctx, stop := context.WithCancel(ctx)
	l := &link{epoch: epoch, stop: stop, done: make(chan linkEnd, 1)}
	go func() {
		defer stop()
		code, reason, err := u.serve(ctx, conn)
		l.done <- linkEnd{code: code, reason: reason, err: err}
	}()
	return l
}

// end stops a connection and waits until its sessions are closed.
func (l *link) end() {
	if l == nil {
		return
	}
	l.stop()
	<-l.done
}

// doneOf is l.done, or nil, which never fires in a select, when there is no
// connection.
func doneOf(l *link) chan linkEnd {
	if l == nil {
		return nil
	}
	return l.done
}

// ride serves cur until it ends, and joins each new hour's handle beside it.
//
// The relay turns its epoch on the hour and from then on refuses the old
// one, but it closes each old room only on that room's turn in a five-minute
// spread. A box that moved only when closed left every phone that dialled in
// between waiting in an empty room for up to five minutes; on 13 Oct 2026 a
// phone waited from 09:00 until the relay closed its box's room at 09:03:18.
// So when the hour turns the box also joins the new handle, and keeps the
// old one until the relay closes it: a phone already talking to the box keeps
// it, and a phone that dials the new handle finds it.
//
// It returns how the last connection ended, once no other is up to carry on.
func (u *Uplink) ride(ctx context.Context, cur *link) linkEnd {
	// Early joins live only as long as this call, so none outlives it.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		next    *link
		joining chan *link
		tries   int
		// tryAt is on the real clock, like every other delay here. Zero
		// until this hour's turn has been seen.
		tryAt time.Time
	)
	check := time.NewTicker(u.turnCheck)
	defer check.Stop()

	for {
		select {
		case end := <-cur.done:
			if next == nil || ctx.Err() != nil {
				next.end()
				return end
			}
			if end.code == CloseRotated {
				u.adoptEpoch(end.reason)
			}
			cur, next = next, nil
			tries, tryAt = 0, time.Time{}
			u.attempt, u.corrections = 0, 0
			u.log.Info("relay epoch rotated", "epoch", cur.epoch, "joined_early", true)

		case <-doneOf(next):
			// The new handle did not hold. Most often the relay has not
			// read its clock since the hour and still says the old epoch,
			// which is not a correction to take. The old room carries on.
			next = nil
			tries++
			tryAt = time.Now().Add(u.turnRetry)

		case l := <-joining:
			joining = nil
			if l == nil {
				tries++
				tryAt = time.Now().Add(u.turnRetry)
				continue
			}
			next = l

		case <-check.C:
			want := u.epoch()
			if next != nil || joining != nil || tries >= turnTries || want <= cur.epoch {
				continue
			}
			if tryAt.IsZero() {
				tryAt = time.Now().Add(time.Duration(u.random() * float64(rotateJitter)))
			}
			if time.Now().Before(tryAt) {
				continue
			}
			joining = make(chan *link, 1)
			go func(result chan<- *link) { result <- u.join(ctx, want) }(joining)
		}
	}
}

// join opens the new hour's handle beside the old one. Nil is a failed try.
func (u *Uplink) join(ctx context.Context, epoch int64) *link {
	handle, err := Handle(u.opts.Enroll.RendezvousSecret(), epoch)
	if err != nil {
		return nil
	}
	conn, err := u.dial(ctx, epoch, handle)
	if err != nil {
		u.log.Debug("could not join the new hour's handle yet", "err", err)
		return nil
	}
	return u.start(ctx, conn, epoch, handle)
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
	// Text frames share the socket and the mutex. Only the deadman claims
	// use them; a box with the switch unarmed never says a text word, so a
	// relay that predates the contract never sees one.
	writeText := func(word string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			return err
		}
		return conn.WriteMessage(websocket.TextMessage, []byte(word))
	}

	live := &sessions{
		slots:  u.slots,
		build:  u.opts.Handler,
		enroll: u.opts.Enroll,
		static: u.opts.Enroll.StaticKey(),
		write:  write,
		log:    u.log,
	}
	here := &liveLink{sessions: live, text: writeText}
	u.liveMu.Lock()
	u.live = append(u.live, here)
	u.liveMu.Unlock()
	defer func() {
		u.liveMu.Lock()
		u.live = slices.DeleteFunc(u.live, func(l *liveLink) bool { return l == here })
		u.liveMu.Unlock()
		live.closeAll()
	}()

	// Post the rows and claim their ids for this connection. The claim is
	// per-socket state on the relay, so it must happen on every connect —
	// and off this goroutine, because it talks HTTP and the read loop must
	// start missing heartbeats on time. And while the connection lasts, the
	// rows are re-posted on a six-hour cadence: each row's auth is a VAPID
	// signature with 24 hours to live, and a connection quiet for a day
	// would otherwise leave the relay holding a farewell it could no longer
	// deliver. Same shape as the lane-0 ticker below.
	if u.opts.Deadman != nil {
		go u.ResyncDeadman()
		refresh := time.NewTicker(u.deadmanRefresh)
		defer refresh.Stop()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-refresh.C:
					u.ResyncDeadman()
				}
			}
		}()
	}

	conn.SetReadLimit(maxFrameBytes)
	_ = conn.SetReadDeadline(time.Now().Add(u.readTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(u.readTimeout))
	})
	conn.SetPingHandler(func(message string) error {
		if err := conn.SetReadDeadline(time.Now().Add(u.readTimeout)); err != nil {
			return err
		}
		// Gorilla's default PingHandler answers with the same payload. Keep
		// that response while also treating the relay heartbeat as activity.
		return conn.WriteControl(websocket.PongMessage, []byte(message), time.Now().Add(writeTimeout))
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
		_ = conn.SetReadDeadline(time.Now().Add(u.readTimeout))

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
