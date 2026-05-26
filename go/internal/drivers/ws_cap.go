package drivers

import (
	"fmt"
	net_http "net/http"
	net_url "net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// gorillaWS is the production WSCap implementation. One per driver.
//
// Concurrency model: the read pump runs on its own goroutine and pushes
// inbound text frames onto an in-memory queue; PopMessages drains the
// queue atomically and is non-blocking, so the Lua driver's poll loop
// stays single-threaded and predictable. Writes (Send) hold the write
// mutex so we don't interleave frames from concurrent dispatch ticks.
//
// On Close the read pump exits via its own Read error (the underlying
// net.Conn close races a graceful WS close message — either path lands
// the goroutine on a sane stop).
type gorillaWS struct {
	driverName string

	mu     sync.Mutex // protects conn, open, queue
	conn   *websocket.Conn
	open   bool
	queue  []string
	writeMu sync.Mutex // serializes WriteMessage calls
	stop    chan struct{}
}

// NewGorillaWS returns a fresh WSCap. The connection isn't established
// until Open is called by the driver — drivers commonly need to do an
// HTTP call (e.g. resolve a Tibber homeId) before they know what to
// subscribe to, so connecting eagerly from the registry would be wrong.
func NewGorillaWS(driverName string) WSCap {
	return &gorillaWS{driverName: driverName}
}

// Open establishes the WebSocket connection. headers go into the HTTP
// upgrade request (Tibber's GraphQL-transport-ws expects
// `Sec-WebSocket-Protocol: graphql-transport-ws`, for instance). Calling
// Open on an already-open cap is a no-op so a driver that re-runs
// driver_init doesn't double-dial.
func (g *gorillaWS) Open(url string, headers map[string]string) error {
	g.mu.Lock()
	if g.open {
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()

	hdr := net_http.Header{}
	var subprotocols []string
	for k, v := range headers {
		// gorilla/websocket requires Subprotocols on the dialer, NOT the
		// header — setting Sec-WebSocket-Protocol in the header map
		// would either be ignored or fight the dialer's own handling. Pull
		// it out and feed the dialer. Comma-separated values are split.
		if strings.EqualFold(k, "Sec-WebSocket-Protocol") {
			for _, p := range strings.Split(v, ",") {
				if p = strings.TrimSpace(p); p != "" {
					subprotocols = append(subprotocols, p)
				}
			}
			continue
		}
		hdr.Set(k, v)
	}
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second
	if len(subprotocols) > 0 {
		dialer.Subprotocols = subprotocols
	}
	// EnableCompression default is true on DefaultDialer; leave it.
	conn, _, err := dialer.Dial(url, hdr)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	// Set a generous read deadline. Tibber's graphql-transport-ws
	// keepalive is a JSON text "ping" frame, NOT a WS control ping —
	// so SetPongHandler never fires on a healthy Tibber stream and
	// the read pump must refresh the deadline itself on every frame.
	// The pong handler is kept as defence-in-depth for servers that
	// do send control-frame pongs.
	conn.SetReadLimit(1 << 20) // 1 MiB per frame is plenty for GraphQL
	_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	g.mu.Lock()
	g.conn = conn
	g.open = true
	g.stop = make(chan struct{})
	// Drop any frames left over from a previous connection's lifetime.
	// Without this the EOF sentinel ("") pushed by the *prior* read pump
	// is delivered to the *new* connection's first PopMessages, the
	// driver sees it as a fresh close, and tears the new socket down —
	// turning a single Tibber server-side disconnect into an infinite
	// reconnect/teardown loop that only ends on a process restart.
	g.queue = nil
	g.mu.Unlock()

	go g.readPump()
	return nil
}

func (g *gorillaWS) readPump() {
	g.mu.Lock()
	c := g.conn
	stop := g.stop
	g.mu.Unlock()
	if c == nil {
		return
	}
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			g.mu.Lock()
			g.open = false
			g.mu.Unlock()
			// Signal end-of-stream by appending a sentinel so the driver
			// can re-open. Empty strings are never valid GraphQL frames,
			// so the driver tests `if msg == ""` for the closed marker.
			g.mu.Lock()
			g.queue = append(g.queue, "")
			g.mu.Unlock()
			select {
			case <-stop:
			default:
				close(stop)
			}
			return
		}
		// Frame arrived — push the read deadline forward. Without this
		// a brief data lull on a healthy stream (no traffic ≥60 s) would
		// time out and tear down the connection.
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		g.mu.Lock()
		// Cap the inbound queue at 1024 frames to bound memory if the
		// driver's poll loop stalls. Drop oldest on overflow; the driver
		// will see a gap in transId acks and handle it on resubscribe.
		if len(g.queue) >= 1024 {
			g.queue = g.queue[len(g.queue)-512:]
		}
		g.queue = append(g.queue, string(data))
		g.mu.Unlock()
	}
}

// Send writes one text frame. Returns ErrNoCapability-style errors as
// plain errors so the Lua side gets a string.
func (g *gorillaWS) Send(text string) error {
	g.mu.Lock()
	c := g.conn
	open := g.open
	g.mu.Unlock()
	if c == nil || !open {
		return fmt.Errorf("ws not open")
	}
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.WriteMessage(websocket.TextMessage, []byte(text))
}

func (g *gorillaWS) PopMessages() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.queue
	g.queue = nil
	return out
}

func (g *gorillaWS) IsOpen() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

func (g *gorillaWS) Close() error {
	g.mu.Lock()
	c := g.conn
	wasOpen := g.open
	g.open = false
	g.conn = nil
	g.mu.Unlock()
	if c == nil {
		return nil
	}
	if wasOpen {
		// Polite close — the read pump will see ErrClosed and exit.
		_ = c.SetWriteDeadline(time.Now().Add(1 * time.Second))
		_ = c.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}
	return c.Close()
}

// wsHostAllowed reuses the HTTP allowlist matching rules for ws://+wss://
// URLs so the operator only learns one model. Empty list = any host.
// Scheme must be ws or wss.
func wsHostAllowed(rawURL string, allowed []string) (bool, string) {
	u, err := net_url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false, "invalid URL"
	}
	switch strings.ToLower(u.Scheme) {
	case "ws", "wss":
	default:
		return false, fmt.Sprintf("scheme %q not supported (ws/wss only)", u.Scheme)
	}
	if len(allowed) == 0 {
		return true, ""
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		if u.Scheme == "wss" {
			port = "443"
		} else {
			port = "80"
		}
	}
	for _, raw := range allowed {
		entry := strings.ToLower(strings.TrimSpace(raw))
		if entry == "" {
			continue
		}
		eHost, ePort, hasPort := splitHostPortLower(entry)
		if !hasPort {
			if entry == host {
				return true, ""
			}
			continue
		}
		if eHost == host && ePort == port {
			return true, ""
		}
	}
	return false, fmt.Sprintf("host %q (port %s) not in allowed_hosts", host, port)
}
