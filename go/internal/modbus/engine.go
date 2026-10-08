package modbus

import (
	"errors"
	"net"
	"strconv"
	"sync"

	"github.com/srcfl/ftw/go/internal/drivers"
)

// Engine is the process wiring for Modbus TCP: drivers, fingerprinting
// and the optional LAN proxy all Dial through it. The actual one-socket-
// per-endpoint pool lives in Dial — master already shares sessions there
// (#987) — so Open is a named entry point, not a second pool.
type Engine struct {
	mu          sync.Mutex
	writeOwners map[string]bool
}

// NewEngine returns the process Modbus wiring. Session sharing is in Dial.
func NewEngine() *Engine {
	return &Engine{}
}

// Open returns a ModbusCap on the shared session for host:port. unitID is
// applied per request so two handles on the same socket can address
// different slaves. The socket stays up until every handle (drivers and
// the proxy pin) has Closed.
func (e *Engine) Open(host string, port, unitID int, allowUnverifiedLocal bool) (drivers.ModbusCap, error) {
	if e == nil {
		return nil, errors.New("modbus engine is nil")
	}
	cap, err := DialWithOptions(host, port, unitID, allowUnverifiedLocal)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	owned := e.writeOwners[sessionKey(host, port)]
	e.mu.Unlock()
	if owned {
		cap.conn.mu.Lock()
		cap.conn.externalWriteOwner = true
		cap.conn.mu.Unlock()
	}
	return cap, nil
}

// SetWriteOwners installs startup policy even when a listener cannot start.
// Existing handles share the same gate; new probes cannot acquire a writer.
func (e *Engine) SetWriteOwners(owners map[string]bool) {
	e.mu.Lock()
	e.writeOwners = make(map[string]bool, len(owners))
	for key, owned := range owners {
		e.writeOwners[key] = owned
	}
	e.mu.Unlock()
	registryMu.Lock()
	var conns []*sharedConn
	for key, owned := range owners {
		if conn := endpointConns[key]; owned && conn != nil {
			conns = append(conns, conn)
		}
	}
	registryMu.Unlock()
	for _, conn := range conns {
		conn.mu.Lock()
		conn.externalWriteOwner = true
		conn.mu.Unlock()
	}
}

func (e *Engine) sessionCount() int {
	registryMu.Lock()
	defer registryMu.Unlock()
	return len(endpointConns)
}

func (e *Engine) refCount(key string) int {
	registryMu.Lock()
	defer registryMu.Unlock()
	s := endpointConns[key]
	if s == nil {
		return 0
	}
	return s.refs
}

func sessionKey(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
