// Package modbus provides a Modbus TCP capability wrapper for drivers.
package modbus

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	sv "github.com/simonvetter/modbus"

	"github.com/srcfl/ftw/go/internal/drivers"
)

const (
	reconnectBackoffBase = 2 * time.Second
	reconnectBackoffMax  = 60 * time.Second

	// Many inverters accept exactly one Modbus TCP session and evict the
	// previous one on every new connect. Redials above this rate almost
	// always mean another client is fighting for the device's only
	// session — worth a named warning instead of a silent log flood.
	// Threshold calibrated against the 2026-08-29 incident: the war ran
	// 20-30 redials/min sustained, while the same inverter alone (its own
	// short idle timeout) peaks at 7-8/min — 10 splits them cleanly.
	reconnectObserveWindow  = time.Minute
	evictionRedialThreshold = 10
	evictionWarnMinInterval = time.Minute
	reconnectLogMinInterval = time.Minute
)

// sharedConn owns the single Modbus TCP session for one endpoint. Every
// Capability for the same host:port serializes through this mutex and
// socket, because devices that allow only one session (Sungrow hybrids,
// most single-session dongles) RST the old connection on every new
// connect — two FTW-side clients would evict each other on every
// request. The first transport error gets one immediate reconnect and
// retry. Repeated mute sessions use a non-blocking reconnect cooldown so
// a single-session device can release its old socket without blocking
// the driver's poll and command loop.
type sharedConn struct {
	mu                   sync.Mutex
	client               *tcpClient
	url                  string
	addr                 string
	allowUnverifiedLocal bool
	requestTimeout       time.Duration

	// activeUnitID mirrors what the live client was last programmed
	// with, so capabilities with different unit ids can share the
	// session and only touch the wire header when the id changes.
	// A fresh tcpClient defaults to unit 1.
	activeUnitID int

	consecutiveTransportFailures int
	nextReconnectAt              time.Time
	now                          func() time.Time

	// Redial observability: recent successful redials (pruned to
	// reconnectObserveWindow), plus rate-limit state for the reconnect
	// INFO line and the eviction WARN.
	recentRedials           []time.Time
	lastReconnectLogAt      time.Time
	suppressedReconnectLogs int
	lastEvictionWarnAt      time.Time

	// refs counts live Capabilities; guarded by registryMu, not mu.
	refs int
}

var (
	registryMu    sync.Mutex
	endpointConns = map[string]*sharedConn{}
)

// Capability is one user's handle on an endpoint's shared session —
// implements drivers.ModbusCap. Each driver (and each driver-test or
// fingerprint probe) gets its own handle with its own unit id; they all
// borrow the same underlying socket so FTW never opens a second
// connection to a device it is already talking to.
type Capability struct {
	conn   *sharedConn
	unitID int
	url    string
	closed bool // guarded by registryMu; Close is idempotent per handle
}

// Dial opens a Modbus TCP connection.
func Dial(host string, port, unitID int) (*Capability, error) {
	return DialWithOptions(host, port, unitID, false)
}

// DialWithOptions returns a capability on the endpoint's single shared
// connection, dialing it only if no other capability holds it, with the
// core's explicit policy for unauthenticated mDNS names.
func DialWithOptions(host string, port, unitID int, allowUnverifiedLocal bool) (*Capability, error) {
	if err := validateEndpoint(host, port, unitID); err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	url := "tcp://" + addr

	registryMu.Lock()
	if existing := endpointConns[addr]; existing != nil {
		existing.refs++
		refs := existing.refs
		mismatch := existing.allowUnverifiedLocal != allowUnverifiedLocal
		registryMu.Unlock()
		slog.Info("modbus endpoint shared — reusing the device's single session",
			"url", url, "users", refs, "unit_id", unitID)
		if mismatch {
			slog.Warn("modbus endpoint shared with a different allow_unverified_local policy; the first dialer's policy stays in effect",
				"url", url)
		}
		return &Capability{conn: existing, unitID: unitID, url: url}, nil
	}
	conn := &sharedConn{
		url:                  url,
		addr:                 addr,
		allowUnverifiedLocal: allowUnverifiedLocal,
		requestTimeout:       modbusRequestTimeout,
		activeUnitID:         1,
		now:                  time.Now,
		refs:                 1,
	}
	endpointConns[addr] = conn
	registryMu.Unlock()

	capability := &Capability{conn: conn, unitID: unitID, url: url}
	conn.mu.Lock()
	cli := newTCPClientWithOptions(addr, modbusRequestTimeout, modbusTCPKeepAlive, allowUnverifiedLocal)
	if err := cli.Open(); err != nil {
		if !isRetryableDialError(err) {
			conn.mu.Unlock()
			// Permanent config problem — don't leave a dead entry for
			// later dialers to share.
			releaseConn(capability)
			return nil, err
		}
		conn.noteTransportFailure()
		conn.mu.Unlock()
		slog.Warn("modbus initial connection unavailable; polling will retry",
			"url", url, "err", err)
		return capability, nil
	}
	if unitID > 0 {
		cli.SetUnitId(uint8(unitID))
		conn.activeUnitID = unitID
	}
	conn.client = cli
	conn.mu.Unlock()
	return capability, nil
}

func isRetryableDialError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// A valid local hostname may not resolve until the device starts
		// advertising it. Endpoint syntax has already been validated, so
		// resolution failures belong to the normal reconnect loop.
		return true
	}
	return isTransportError(err)
}

func validateEndpoint(host string, port, unitID int) error {
	if host == "" || host != strings.TrimSpace(host) || !validHost(host) {
		return fmt.Errorf("invalid modbus host %q", host)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid modbus port %d", port)
	}
	if unitID < 0 || unitID > 247 {
		return fmt.Errorf("invalid modbus unit id %d", unitID)
	}
	return nil
}

func validHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if zoneAt := strings.LastIndexByte(host, '%'); zoneAt > 0 && zoneAt < len(host)-1 &&
		net.ParseIP(host[:zoneAt]) != nil && !strings.ContainsAny(host[zoneAt+1:], " \t\r\n/") {
		return true
	}
	if len(host) > 253 {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || !asciiAlphaNum(label[0]) || !asciiAlphaNum(label[len(label)-1]) {
			return false
		}
		for i := 1; i+1 < len(label); i++ {
			if !asciiAlphaNum(label[i]) && label[i] != '-' {
				return false
			}
		}
	}
	return true
}

func asciiAlphaNum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// Close releases this handle's reference on the shared session. The
// socket closes only when the last handle is gone, so a driver-test or
// fingerprint probe finishing does not tear down the live driver's
// connection.
func (c *Capability) Close() error {
	if released := releaseConn(c); !released {
		return nil
	}
	c.conn.mu.Lock()
	defer c.conn.mu.Unlock()
	return c.conn.closeClient()
}

// releaseConn drops the handle's reference and reports whether it was
// the last one, deregistering the endpoint if so.
func releaseConn(c *Capability) bool {
	registryMu.Lock()
	defer registryMu.Unlock()
	if c.closed {
		return false
	}
	c.closed = true
	c.conn.refs--
	if c.conn.refs > 0 {
		return false
	}
	if endpointConns[c.conn.addr] == c.conn {
		delete(endpointConns, c.conn.addr)
	}
	return true
}

// Read — implements drivers.ModbusCap. Reconnects once on transport error.
func (c *Capability) Read(addr, count uint16, kind int32) ([]uint16, error) {
	conn := c.conn
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if err := conn.ensureClient(); err != nil {
		return nil, err
	}
	conn.applyUnit(c.unitID)
	var fc byte
	switch kind {
	case drivers.ModbusInput:
		fc = modbusReadInputRegisters
	case drivers.ModbusHolding:
		fc = modbusReadHoldingRegisters
	default:
		fc = modbusReadInputRegisters
	}
	regs, err := conn.client.ReadRegisters(addr, count, fc)
	if err == nil {
		conn.noteLiveResponse()
		return regs, nil
	}
	if !isTransportError(err) {
		conn.noteLiveResponse()
		return regs, err
	}
	if rerr := conn.prepareTransportRetry(); rerr != nil {
		return nil, fmt.Errorf("read after reconnect: %w (original: %v)", rerr, err)
	}
	conn.applyUnit(c.unitID)
	regs, err = conn.client.ReadRegisters(addr, count, fc)
	conn.finishRequest(err)
	return regs, markTransport(err)
}

// markTransport tags a genuine link failure so the driver host can tell it
// from a device that answered and simply refused the register. Anything the
// device replied to — an illegal-address exception, say — passes through
// untouched, because it is evidence the device is alive.
func markTransport(err error) error {
	if err == nil || !isTransportError(err) {
		return err
	}
	if errors.Is(err, drivers.ErrModbusTransport) {
		return err
	}
	return fmt.Errorf("%w: %v", drivers.ErrModbusTransport, err)
}

// WriteSingle — implements drivers.ModbusCap. Reconnects once on transport error.
func (c *Capability) WriteSingle(addr, value uint16) error {
	conn := c.conn
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if err := conn.ensureClient(); err != nil {
		return err
	}
	conn.applyUnit(c.unitID)
	err := conn.client.WriteRegister(addr, value)
	if err == nil {
		conn.noteLiveResponse()
		return nil
	}
	if !isTransportError(err) {
		conn.noteLiveResponse()
		return err
	}
	if rerr := conn.prepareTransportRetry(); rerr != nil {
		return fmt.Errorf("write after reconnect: %w (original: %v)", rerr, err)
	}
	conn.applyUnit(c.unitID)
	err = conn.client.WriteRegister(addr, value)
	conn.finishRequest(err)
	return err
}

// WriteMulti — implements drivers.ModbusCap. Reconnects once on transport error.
func (c *Capability) WriteMulti(addr uint16, values []uint16) error {
	conn := c.conn
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if err := conn.ensureClient(); err != nil {
		return err
	}
	conn.applyUnit(c.unitID)
	err := conn.client.WriteRegisters(addr, values)
	if err == nil {
		conn.noteLiveResponse()
		return nil
	}
	if !isTransportError(err) {
		conn.noteLiveResponse()
		return err
	}
	if rerr := conn.prepareTransportRetry(); rerr != nil {
		return fmt.Errorf("write-multi after reconnect: %w (original: %v)", rerr, err)
	}
	conn.applyUnit(c.unitID)
	err = conn.client.WriteRegisters(addr, values)
	conn.finishRequest(err)
	return err
}

// executePDU runs one raw PDU on the shared session. Exception PDUs are
// returned as data so a proxy can forward them. Transport errors follow
// the same reconnect-once policy as Read. unitID is taken from the
// request so one TCP session can serve several slaves.
func (c *Capability) executePDU(unitID uint8, pdu []byte) ([]byte, error) {
	conn := c.conn
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if err := conn.ensureClient(); err != nil {
		return nil, err
	}
	conn.applyUnit(int(unitID))
	res, err := conn.client.roundTrip(unitID, pdu)
	if err == nil {
		conn.noteLiveResponse()
		return res, nil
	}
	if !isTransportError(err) {
		conn.noteLiveResponse()
		return res, err
	}
	if rerr := conn.prepareTransportRetry(); rerr != nil {
		return nil, fmt.Errorf("pdu after reconnect: %w (original: %v)", rerr, err)
	}
	conn.applyUnit(int(unitID))
	res, err = conn.client.roundTrip(unitID, pdu)
	conn.finishRequest(err)
	return res, markTransport(err)
}

// applyUnit programs the handle's unit id into the live client when it
// differs from what the session was last set to. Caller holds c.mu and
// has ensured the client exists. Unit id 0 keeps whatever the session
// already uses (a fresh client defaults to unit 1), matching the old
// single-owner behavior where 0 meant "never set".
func (c *sharedConn) applyUnit(unitID int) {
	if unitID <= 0 || unitID == c.activeUnitID || c.client == nil {
		return
	}
	c.client.SetUnitId(uint8(unitID))
	c.activeUnitID = unitID
}

func (c *sharedConn) ensureClient() error {
	if c.client != nil {
		return nil
	}
	if remaining := c.reconnectDelay(); remaining > 0 {
		// Not new evidence about the link — the earlier transport failure
		// already supplied that. Marked separately so a poll does not
		// count one dropped packet as a dozen.
		return fmt.Errorf("%w for %s", drivers.ErrModbusBackoff, remaining.Round(time.Millisecond))
	}
	return c.reconnect()
}

func (c *sharedConn) prepareTransportRetry() error {
	c.noteTransportFailure()
	_ = c.closeClient()
	if c.consecutiveTransportFailures > 1 {
		return fmt.Errorf("%w for %s", drivers.ErrModbusBackoff, c.reconnectDelay().Round(time.Millisecond))
	}
	return c.reconnect()
}

func (c *sharedConn) finishRequest(err error) {
	if err == nil || !isTransportError(err) {
		c.noteLiveResponse()
		return
	}
	c.noteTransportFailure()
	_ = c.closeClient()
}

func (c *sharedConn) noteLiveResponse() {
	c.consecutiveTransportFailures = 0
	c.nextReconnectAt = time.Time{}
}

func (c *sharedConn) noteTransportFailure() {
	c.consecutiveTransportFailures++
	wait := c.reconnectBackoff()
	if wait == 0 {
		return
	}
	c.nextReconnectAt = c.nowTime().Add(wait)
	slog.Warn("modbus reconnect scheduled",
		"url", c.url,
		"failures", c.consecutiveTransportFailures,
		"retry_in", wait)
}

func (c *sharedConn) reconnectBackoff() time.Duration {
	if c.consecutiveTransportFailures <= 1 {
		return 0
	}
	shift := c.consecutiveTransportFailures - 2
	if shift > 5 {
		shift = 5
	}
	wait := reconnectBackoffBase << shift
	if wait > reconnectBackoffMax {
		return reconnectBackoffMax
	}
	return wait
}

func (c *sharedConn) reconnectDelay() time.Duration {
	if c.nextReconnectAt.IsZero() {
		return 0
	}
	remaining := c.nextReconnectAt.Sub(c.nowTime())
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (c *sharedConn) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *sharedConn) closeClient() error {
	if c.client == nil {
		return nil
	}
	err := c.client.Close()
	c.client = nil
	return err
}

// reconnect tears down the current socket and dials a fresh one. Caller must
// hold c.mu. Some inverter firmwares leave Modbus TCP sessions stale after idle
// time or a write; a fresh socket is the only reliable recovery.
func (c *sharedConn) reconnect() error {
	_ = c.closeClient()
	timeout := c.requestTimeout
	if timeout <= 0 {
		timeout = modbusRequestTimeout
	}
	cli := newTCPClientWithOptions(c.addr, timeout, modbusTCPKeepAlive, c.allowUnverifiedLocal)
	if err := cli.Open(); err != nil {
		c.noteTransportFailure()
		return err
	}
	c.client = cli
	c.activeUnitID = 1 // fresh client default; applyUnit reprograms per request
	c.noteReconnected()
	return nil
}

// noteReconnected records a successful redial, warns when the redial
// rate says another client keeps taking the device's only session, and
// rate-limits the reconnect INFO line so a redial-per-poll situation
// cannot flood the log ring (observed: ~17k lines in 14 h). Caller
// holds c.mu.
func (c *sharedConn) noteReconnected() {
	now := c.nowTime()
	cutoff := now.Add(-reconnectObserveWindow)
	kept := c.recentRedials[:0]
	for _, t := range c.recentRedials {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	c.recentRedials = append(kept, now)

	if len(c.recentRedials) >= evictionRedialThreshold &&
		(c.lastEvictionWarnAt.IsZero() || now.Sub(c.lastEvictionWarnAt) >= evictionWarnMinInterval) {
		c.lastEvictionWarnAt = now
		slog.Warn("modbus session keeps being replaced — another Modbus client is likely competing for this device's only session",
			"url", c.url,
			"redials_last_minute", len(c.recentRedials))
	}

	if c.lastReconnectLogAt.IsZero() || now.Sub(c.lastReconnectLogAt) >= reconnectLogMinInterval {
		if c.suppressedReconnectLogs > 0 {
			slog.Info("modbus reconnected", "url", c.url,
				"suppressed_repeats", c.suppressedReconnectLogs)
		} else {
			slog.Info("modbus reconnected", "url", c.url)
		}
		c.lastReconnectLogAt = now
		c.suppressedReconnectLogs = 0
		return
	}
	c.suppressedReconnectLogs++
}

// isTransportError classifies an error as a TCP transport failure where
// a reconnect is the correct response. Modbus protocol errors (illegal
// function, illegal address, slave busy) are NOT transport errors —
// they come from a live peer and the connection is still usable.
func isTransportError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ENOTCONN) ||
		errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	// simonvetter's own deadline sentinel. It is a plain string-typed value,
	// NOT a net.Error and not wrapping syscall.ETIMEDOUT, so neither check
	// above catches it. A request that gets no reply before the client
	// timeout can leave the TCP socket ESTABLISHED while the device has gone
	// mute on that session — observed on CTEK CSOS chargers, where a fresh
	// connection answers instantly. Redialing is the correct response.
	if errors.Is(err, sv.ErrRequestTimedOut) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// simonvetter wraps some errors as plain strings; match on the text as
	// a last resort. Narrow set of known-transport messages only.
	msg := err.Error()
	for _, s := range []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"use of closed network connection",
		"i/o timeout",
		"timed out",
		"EOF",
	} {
		if containsFold(msg, s) {
			return true
		}
	}
	return false
}

// containsFold is strings.Contains with a case-insensitive fold. Avoids
// pulling in strings just for one call.
func containsFold(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		ok := true
		for j := 0; j < len(needle); j++ {
			a, b := haystack[i+j], needle[j]
			if a >= 'A' && a <= 'Z' {
				a += 32
			}
			if b >= 'A' && b <= 'Z' {
				b += 32
			}
			if a != b {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
