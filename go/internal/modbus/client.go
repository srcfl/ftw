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
)

// Capability wraps a Modbus TCP client. Each call serializes through the
// mutex. The first transport error gets one immediate reconnect and retry.
// Repeated mute sessions use a non-blocking reconnect cooldown so a
// single-session dongle can release its old socket without blocking the
// driver's poll and command loop.
type Capability struct {
	mu             sync.Mutex
	client         *tcpClient
	url            string
	addr           string
	unitID         int
	requestTimeout time.Duration

	consecutiveTransportFailures int
	nextReconnectAt              time.Time
	now                          func() time.Time
}

// Dial opens a Modbus TCP connection.
func Dial(host string, port, unitID int) (*Capability, error) {
	if err := validateEndpoint(host, port, unitID); err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	url := "tcp://" + addr
	cli := newTCPClient(addr, modbusRequestTimeout, modbusTCPKeepAlive)
	capability := &Capability{
		url:            url,
		addr:           addr,
		unitID:         unitID,
		requestTimeout: modbusRequestTimeout,
		now:            time.Now,
	}
	if err := cli.Open(); err != nil {
		if !isRetryableDialError(err) {
			return nil, err
		}
		capability.noteTransportFailure()
		slog.Warn("modbus initial connection unavailable; polling will retry",
			"url", url, "err", err)
		return capability, nil
	}
	if unitID > 0 {
		cli.SetUnitId(uint8(unitID))
	}
	capability.client = cli
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

// Close the underlying connection.
func (c *Capability) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeClient()
}

// Read — implements drivers.ModbusCap. Reconnects once on transport error.
func (c *Capability) Read(addr, count uint16, kind int32) ([]uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureClient(); err != nil {
		return nil, err
	}
	var fc byte
	switch kind {
	case drivers.ModbusInput:
		fc = modbusReadInputRegisters
	case drivers.ModbusHolding:
		fc = modbusReadHoldingRegisters
	default:
		fc = modbusReadInputRegisters
	}
	regs, err := c.client.ReadRegisters(addr, count, fc)
	if err == nil {
		c.noteLiveResponse()
		return regs, nil
	}
	if !isTransportError(err) {
		c.noteLiveResponse()
		return regs, err
	}
	if rerr := c.prepareTransportRetry(); rerr != nil {
		return nil, fmt.Errorf("read after reconnect: %w (original: %v)", rerr, err)
	}
	regs, err = c.client.ReadRegisters(addr, count, fc)
	c.finishRequest(err)
	return regs, err
}

// WriteSingle — implements drivers.ModbusCap. Reconnects once on transport error.
func (c *Capability) WriteSingle(addr, value uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureClient(); err != nil {
		return err
	}
	err := c.client.WriteRegister(addr, value)
	if err == nil {
		c.noteLiveResponse()
		return nil
	}
	if !isTransportError(err) {
		c.noteLiveResponse()
		return err
	}
	if rerr := c.prepareTransportRetry(); rerr != nil {
		return fmt.Errorf("write after reconnect: %w (original: %v)", rerr, err)
	}
	err = c.client.WriteRegister(addr, value)
	c.finishRequest(err)
	return err
}

// WriteMulti — implements drivers.ModbusCap. Reconnects once on transport error.
func (c *Capability) WriteMulti(addr uint16, values []uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureClient(); err != nil {
		return err
	}
	err := c.client.WriteRegisters(addr, values)
	if err == nil {
		c.noteLiveResponse()
		return nil
	}
	if !isTransportError(err) {
		c.noteLiveResponse()
		return err
	}
	if rerr := c.prepareTransportRetry(); rerr != nil {
		return fmt.Errorf("write-multi after reconnect: %w (original: %v)", rerr, err)
	}
	err = c.client.WriteRegisters(addr, values)
	c.finishRequest(err)
	return err
}

func (c *Capability) ensureClient() error {
	if c.client != nil {
		return nil
	}
	if remaining := c.reconnectDelay(); remaining > 0 {
		return fmt.Errorf("modbus reconnect backoff active for %s", remaining.Round(time.Millisecond))
	}
	return c.reconnect()
}

func (c *Capability) prepareTransportRetry() error {
	c.noteTransportFailure()
	_ = c.closeClient()
	if c.consecutiveTransportFailures > 1 {
		return fmt.Errorf("modbus reconnect backoff active for %s", c.reconnectDelay().Round(time.Millisecond))
	}
	return c.reconnect()
}

func (c *Capability) finishRequest(err error) {
	if err == nil || !isTransportError(err) {
		c.noteLiveResponse()
		return
	}
	c.noteTransportFailure()
	_ = c.closeClient()
}

func (c *Capability) noteLiveResponse() {
	c.consecutiveTransportFailures = 0
	c.nextReconnectAt = time.Time{}
}

func (c *Capability) noteTransportFailure() {
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

func (c *Capability) reconnectBackoff() time.Duration {
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

func (c *Capability) reconnectDelay() time.Duration {
	if c.nextReconnectAt.IsZero() {
		return 0
	}
	remaining := c.nextReconnectAt.Sub(c.nowTime())
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (c *Capability) nowTime() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Capability) closeClient() error {
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
func (c *Capability) reconnect() error {
	_ = c.closeClient()
	timeout := c.requestTimeout
	if timeout <= 0 {
		timeout = modbusRequestTimeout
	}
	cli := newTCPClient(c.addr, timeout, modbusTCPKeepAlive)
	if err := cli.Open(); err != nil {
		c.noteTransportFailure()
		return err
	}
	if c.unitID > 0 {
		cli.SetUnitId(uint8(c.unitID))
	}
	c.client = cli
	slog.Info("modbus reconnected", "url", c.url)
	return nil
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
