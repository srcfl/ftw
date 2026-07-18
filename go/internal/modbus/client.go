// Package modbus provides a Modbus TCP capability wrapper for drivers.
package modbus

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	sv "github.com/simonvetter/modbus"

	"github.com/srcfl/ftw/go/internal/drivers"
)

// Reconnect backoff for consecutive mute/transport failures.
//
// Some single-session dongles (notably GoodWe WiFi/LAN) accept a new TCP
// handshake while the Modbus layer is still bound to a ghost session left
// by a hard controller reboot. Immediate reconnect loops never give the
// peer time to drop that ghost. The first failure still redials
// immediately (Sungrow/CTEK path); subsequent consecutive failures wait
// 2s, 4s, 8s … up to 60s before the next dial.
const (
	reconnectBackoffBase = 2 * time.Second
	reconnectBackoffMax  = 60 * time.Second
)

// Capability wraps a Modbus TCP client. Each call serializes through
// the mutex; on a transport-level error a single reconnect + retry is
// attempted so a silently closed TCP socket (common on Sungrow + several
// other inverter firmwares after idle timeout or after receiving a write)
// doesn't condemn the driver to silent zeros until a full process restart.
//
// TCP keepalive (15s) is enabled on every socket so half-open peers age
// out within about a minute during normal operation. After a hard reboot
// the dead client's keepalives stop, so consecutive mute timeouts also
// apply exponential reconnect backoff (#522).
type Capability struct {
	mu     sync.Mutex
	client *tcpClient
	url    string
	addr   string
	unitID int
	// requestTimeout is the per-request deadline; reconnect dials reuse it.
	// Defaults to modbusRequestTimeout; tests may lower it.
	requestTimeout time.Duration

	// consecutiveTransportFails counts transport errors since the last
	// successful request. Drives reconnect backoff for mute sessions.
	consecutiveTransportFails int
	lastReconnect             time.Time

	// now/sleep are overridable in tests so backoff does not wall-clock wait.
	now   func() time.Time
	sleep func(time.Duration)
}

// Dial opens a Modbus TCP connection.
func Dial(host string, port, unitID int) (*Capability, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	url := "tcp://" + addr
	cli := newTCPClient(addr, modbusRequestTimeout, modbusTCPKeepAlive)
	if err := cli.Open(); err != nil {
		return nil, err
	}
	if unitID > 0 {
		cli.SetUnitId(uint8(unitID))
	}
	return &Capability{
		client:         cli,
		url:            url,
		addr:           addr,
		unitID:         unitID,
		requestTimeout: modbusRequestTimeout,
		now:            time.Now,
		sleep:          time.Sleep,
	}, nil
}

// Close the underlying connection.
func (c *Capability) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.client.Close()
}

// Read — implements drivers.ModbusCap. Reconnects once on transport error.
func (c *Capability) Read(addr, count uint16, kind int32) ([]uint16, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
		c.noteSuccess()
		return regs, nil
	}
	if !isTransportError(err) {
		return regs, err
	}
	c.noteTransportFail()
	if rerr := c.reconnect(); rerr != nil {
		return nil, fmt.Errorf("read after reconnect: %w (original: %v)", rerr, err)
	}
	regs, err = c.client.ReadRegisters(addr, count, fc)
	if err == nil {
		c.noteSuccess()
	} else if isTransportError(err) {
		c.noteTransportFail()
	}
	return regs, err
}

// WriteSingle — implements drivers.ModbusCap. Reconnects once on transport error.
func (c *Capability) WriteSingle(addr, value uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.client.WriteRegister(addr, value)
	if err == nil {
		c.noteSuccess()
		return nil
	}
	if !isTransportError(err) {
		return err
	}
	c.noteTransportFail()
	if rerr := c.reconnect(); rerr != nil {
		return fmt.Errorf("write after reconnect: %w (original: %v)", rerr, err)
	}
	err = c.client.WriteRegister(addr, value)
	if err == nil {
		c.noteSuccess()
	} else if isTransportError(err) {
		c.noteTransportFail()
	}
	return err
}

// WriteMulti — implements drivers.ModbusCap. Reconnects once on transport error.
func (c *Capability) WriteMulti(addr uint16, values []uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.client.WriteRegisters(addr, values)
	if err == nil {
		c.noteSuccess()
		return nil
	}
	if !isTransportError(err) {
		return err
	}
	c.noteTransportFail()
	if rerr := c.reconnect(); rerr != nil {
		return fmt.Errorf("write-multi after reconnect: %w (original: %v)", rerr, err)
	}
	err = c.client.WriteRegisters(addr, values)
	if err == nil {
		c.noteSuccess()
	} else if isTransportError(err) {
		c.noteTransportFail()
	}
	return err
}

func (c *Capability) noteSuccess() {
	c.consecutiveTransportFails = 0
}

func (c *Capability) noteTransportFail() {
	c.consecutiveTransportFails++
}

// reconnectBackoff returns how long to wait before the next dial given the
// current consecutive transport failure count. First failure: immediate;
// thereafter exponential up to reconnectBackoffMax.
func (c *Capability) reconnectBackoff() time.Duration {
	n := c.consecutiveTransportFails
	if n <= 1 {
		return 0
	}
	shift := n - 2
	if shift > 5 {
		shift = 5
	}
	wait := reconnectBackoffBase << shift
	if wait > reconnectBackoffMax {
		return reconnectBackoffMax
	}
	return wait
}

// reconnect tears down the current socket and dials a fresh one. Caller must
// hold c.mu. Some inverter firmwares leave Modbus TCP sessions stale after idle
// time or a write; a fresh socket is the only reliable recovery. Consecutive
// mute failures wait (exponential backoff) so single-session dongles can drop
// a ghost session left by a hard reboot (#522).
func (c *Capability) reconnect() error {
	if wait := c.reconnectBackoff(); wait > 0 {
		nowFn := c.now
		if nowFn == nil {
			nowFn = time.Now
		}
		sleepFn := c.sleep
		if sleepFn == nil {
			sleepFn = time.Sleep
		}
		elapsed := time.Duration(0)
		if !c.lastReconnect.IsZero() {
			elapsed = nowFn().Sub(c.lastReconnect)
		}
		if elapsed < wait {
			delay := wait - elapsed
			slog.Info("modbus reconnect backoff",
				"url", c.url,
				"fails", c.consecutiveTransportFails,
				"wait", delay)
			sleepFn(delay)
		}
	}

	_ = c.client.Close()
	timeout := c.requestTimeout
	if timeout <= 0 {
		timeout = modbusRequestTimeout
	}
	cli := newTCPClient(c.addr, timeout, modbusTCPKeepAlive)
	if err := cli.Open(); err != nil {
		return err
	}
	if c.unitID > 0 {
		cli.SetUnitId(uint8(c.unitID))
	}
	c.client = cli
	nowFn := c.now
	if nowFn == nil {
		nowFn = time.Now
	}
	c.lastReconnect = nowFn()
	slog.Info("modbus reconnected", "url", c.url, "fails", c.consecutiveTransportFails)
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
		errors.Is(err, syscall.ETIMEDOUT) {
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
	if errors.As(err, &netErr) && netErr.Timeout() {
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
