package drivers

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/goburrow/serial"
	"github.com/srcfl/ftw/go/internal/config"
)

const serialReadSlice = 50 * time.Millisecond

type localSerialCap struct {
	mu         sync.Mutex
	port       serial.Port
	maxTimeout time.Duration
}

// OpenSerial opens one local serial device for a driver's read-only host grant.
func OpenSerial(cfg *config.SerialConfig) (SerialCap, error) {
	if cfg == nil {
		return nil, errors.New("serial config is required")
	}
	maxTimeout := time.Duration(cfg.ReadTimeoutMS) * time.Millisecond
	portTimeout := min(maxTimeout, serialReadSlice)
	port, err := serial.Open(&serial.Config{
		Address:  cfg.Address,
		BaudRate: cfg.BaudRate,
		DataBits: cfg.DataBits,
		StopBits: cfg.StopBits,
		Parity:   cfg.Parity,
		Timeout:  portTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.Address, err)
	}
	return &localSerialCap{port: port, maxTimeout: maxTimeout}, nil
}

func (c *localSerialCap) Read(maxBytes int, timeout time.Duration) ([]byte, error) {
	if maxBytes <= 0 || maxBytes > 2<<20 {
		return nil, fmt.Errorf("serial read size %d is outside 1..%d", maxBytes, 2<<20)
	}
	if timeout <= 0 || timeout > c.maxTimeout {
		timeout = c.maxTimeout
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.port == nil {
		return nil, errors.New("serial device is closed")
	}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, maxBytes)
	for {
		n, err := c.port.Read(buf)
		if n > 0 {
			return buf[:n], nil
		}
		if err != nil && !errors.Is(err, serial.ErrTimeout) && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return []byte{}, nil
		}
	}
}

func (c *localSerialCap) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.port == nil {
		return nil
	}
	err := c.port.Close()
	c.port = nil
	return err
}
