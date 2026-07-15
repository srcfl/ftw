// Package modbus provides a Modbus TCP capability wrapper for drivers.
package modbus

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"time"

	sv "github.com/simonvetter/modbus"

	"github.com/srcfl/ftw/go/internal/drivers"
)

// Capability wraps a simonvetter/modbus client. Each call serializes through
// the mutex; on a transport-level error a single reconnect + retry is
// attempted so a silently closed TCP socket (common on Sungrow + several
// other inverter firmwares after idle timeout or after receiving a write)
// doesn't condemn the driver to silent zeros until a full process restart.
type Capability struct {
	mu     sync.Mutex
	client *sv.ModbusClient
	url    string
	unitID int
}

// Dial opens a Modbus TCP connection.
func Dial(host string, port, unitID int) (*Capability, error) {
	url := fmt.Sprintf("tcp://%s:%d", host, port)
	cli, err := sv.NewClient(&sv.ClientConfiguration{
		URL:     url,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if err := cli.Open(); err != nil {
		return nil, err
	}
	if unitID > 0 {
		_ = cli.SetUnitId(uint8(unitID))
	}
	return &Capability{client: cli, url: url, unitID: unitID}, nil
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
	var rt sv.RegType
	switch kind {
	case drivers.ModbusInput:
		rt = sv.INPUT_REGISTER
	case drivers.ModbusHolding:
		rt = sv.HOLDING_REGISTER
	default:
		rt = sv.INPUT_REGISTER
	}
	regs, err := c.client.ReadRegisters(addr, count, rt)
	if err == nil || !isTransportError(err) {
		return regs, err
	}
	if rerr := c.reconnect(); rerr != nil {
		return nil, fmt.Errorf("read after reconnect: %w (original: %v)", rerr, err)
	}
	return c.client.ReadRegisters(addr, count, rt)
}

// WriteSingle — implements drivers.ModbusCap. Reconnects once on transport error.
func (c *Capability) WriteSingle(addr, value uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.client.WriteRegister(addr, value)
	if err == nil || !isTransportError(err) {
		return err
	}
	if rerr := c.reconnect(); rerr != nil {
		return fmt.Errorf("write after reconnect: %w (original: %v)", rerr, err)
	}
	return c.client.WriteRegister(addr, value)
}

// WriteMulti — implements drivers.ModbusCap. Reconnects once on transport error.
func (c *Capability) WriteMulti(addr uint16, values []uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.client.WriteRegisters(addr, values)
	if err == nil || !isTransportError(err) {
		return err
	}
	if rerr := c.reconnect(); rerr != nil {
		return fmt.Errorf("write-multi after reconnect: %w (original: %v)", rerr, err)
	}
	return c.client.WriteRegisters(addr, values)
}

// reconnect tears down the current socket and dials a fresh one. Caller
// must hold c.mu. The simonvetter client doesn't self-heal from a closed
// TCP socket — subsequent reads return errors forever until Open() is
// called again.
func (c *Capability) reconnect() error {
	_ = c.client.Close()
	cli, err := sv.NewClient(&sv.ClientConfiguration{
		URL:     c.url,
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return err
	}
	if err := cli.Open(); err != nil {
		return err
	}
	if c.unitID > 0 {
		_ = cli.SetUnitId(uint8(c.unitID))
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
