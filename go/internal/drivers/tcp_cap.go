package drivers

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// TCPCap is the host's raw TCP socket capability. One driver = one upstream
// connection (matches MQTTCap / WSCap shape). Used by drivers that talk to
// passthrough Serial-to-Ethernet gateways or other protocols that stream
// unsolicited bytes over a TCP socket — e.g. Dutch P1 smart-meter readers
// streaming DSMR telegrams on port 23.
//
// Read-only by design today: the targets we have don't expect input from us,
// and giving a driver write access to an arbitrary TCP socket is a much
// larger blast-radius decision (CSRF-style escalations against internal
// devices). Add tcp_send only when an actual driver needs it.
type TCPCap interface {
	Open(addr string) error
	// PopBytes returns and clears any bytes buffered since the last call.
	// EOF is signalled by IsOpen() flipping to false; an empty return with
	// IsOpen()==false means the read pump exited and the driver should
	// Close + Open again to recover.
	PopBytes() []byte
	IsOpen() bool
	Close() error
}

// netTCP is the production TCPCap impl: one connection per driver, a
// background read pump that appends bytes to an in-memory buffer, and a
// non-blocking PopBytes drain. Mirrors gorillaWS's concurrency model.
type netTCP struct {
	driverName string
	allowed    []string // empty = any host

	mu     sync.Mutex
	conn   net.Conn
	open   bool
	buf    []byte
	stop   chan struct{}
}

// NewNetTCP returns a TCPCap bound to a driver name. The connection is not
// opened until the driver calls host.tcp_open.
func NewNetTCP(driverName string, allowedHosts []string) TCPCap {
	cp := append([]string(nil), allowedHosts...)
	return &netTCP{driverName: driverName, allowed: cp}
}

// tcpHostAllowed checks `host:port` style addresses against the allowlist.
// Same matching semantics as the WS/HTTP lists: bare entry matches any port
// on that host; "host:port" requires an exact match. Empty list = any host.
func tcpHostAllowed(addr string, allowed []string) (bool, string) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false, "invalid address (need host:port)"
	}
	host = strings.ToLower(host)
	if len(allowed) == 0 {
		return true, ""
	}
	for _, raw := range allowed {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		eHost, ePort, hasPort := splitHostPortLower(entry)
		if !hasPort {
			if strings.ToLower(entry) == host {
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

func (n *netTCP) Open(addr string) error {
	n.mu.Lock()
	if n.open {
		n.mu.Unlock()
		return nil
	}
	n.mu.Unlock()

	if ok, reason := tcpHostAllowed(addr, n.allowed); !ok {
		return fmt.Errorf("tcp: %s", reason)
	}

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("tcp dial: %w", err)
	}

	n.mu.Lock()
	n.conn = conn
	n.open = true
	n.stop = make(chan struct{})
	// Drop any buffered bytes from a prior connection so a re-Open doesn't
	// deliver stale frame fragments mixed in with the new stream.
	n.buf = nil
	n.mu.Unlock()

	go n.readPump()
	return nil
}

func (n *netTCP) readPump() {
	n.mu.Lock()
	c := n.conn
	stop := n.stop
	n.mu.Unlock()
	if c == nil {
		return
	}
	// Small read buffer — P1 telegrams are ~1 KB and arrive once per second,
	// so a 4 KB scratch covers a full telegram with margin.
	scratch := make([]byte, 4096)
	for {
		// Generous read deadline: P1 readers stream one telegram per second,
		// but a brief network blip mustn't tear down a live connection on
		// healthy infrastructure. 60 s matches the WS pump and is well
		// beyond the watchdog timeout, so a real silence flips the driver
		// offline via the watchdog path long before the deadline fires.
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		nRead, err := c.Read(scratch)
		if nRead > 0 {
			n.mu.Lock()
			// Cap the inbound buffer at 64 KiB so a stalled driver poll
			// can't blow memory. Drop oldest on overflow — keeps the most
			// recent telegram which is what the driver actually wants.
			if len(n.buf)+nRead > 65536 {
				keep := 32768
				if len(n.buf) > keep {
					n.buf = n.buf[len(n.buf)-keep:]
				}
			}
			n.buf = append(n.buf, scratch[:nRead]...)
			n.mu.Unlock()
		}
		if err != nil {
			n.mu.Lock()
			n.open = false
			n.mu.Unlock()
			select {
			case <-stop:
			default:
				close(stop)
			}
			return
		}
	}
}

func (n *netTCP) PopBytes() []byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := n.buf
	n.buf = nil
	return out
}

func (n *netTCP) IsOpen() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.open
}

func (n *netTCP) Close() error {
	n.mu.Lock()
	c := n.conn
	n.open = false
	n.conn = nil
	n.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Close()
}
