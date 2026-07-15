package evcloud

import (
	"errors"
	"fmt"
	"strings"
	"time"

	sv "github.com/simonvetter/modbus"

	"github.com/srcfl/ftw/go/internal/config"
)

// ctekDefaultPort is the standard Modbus/TCP port. Overridable via
// EVCharger.Modbus.Port when the device is behind NAT or on a custom
// listener.
const ctekDefaultPort = 502

// ctekDefaultUnitID is what CSOS uses for the first outlet on
// single-outlet stations. Dual-outlet stations expose EVSE2 on unit 2.
const ctekDefaultUnitID = 1

// ctekProbeTimeout bounds the one-shot dial + serial read so a wedged
// LAN can't tie up the wizard request goroutine. The probe is purely
// for the charger-picker UX, not the driver's polling loop, so 5 s is
// generous.
const ctekProbeTimeout = 5 * time.Second

// CTEK register addresses for identity. Same as drivers/ctek.lua for
// API v1. The picker only needs the serial; the runtime driver does
// the rest at poll time.
const (
	ctekRegSerialBase  = 0x1003 // 6 regs → 12 ASCII chars, big-endian byte order
	ctekRegSerialCount = 6
)

// ctekDialFn is injectable for tests — production code dials a real
// modbus socket via simonvetter; tests substitute a fake.
type ctekDialFn func(host string, port, unitID int) (ctekClient, error)

// ctekClient is the minimal surface the picker needs from a modbus
// client. simonvetter.ModbusClient satisfies this; tests can swap in
// a fake without standing up a real Modbus server.
type ctekClient interface {
	ReadHolding(addr, count uint16) ([]uint16, error)
	Close() error
}

func init() { Register("ctek", NewCTEK()) }

// CTEK implements Provider for CTEK Chargestorm Connected 2/3 wallboxes
// reached over Modbus/TCP (Automation API v1). The picker probes the
// identity registers (0x1003..0x1008) to read the serial; the actual
// telemetry + control path lives in drivers/ctek.lua.
type CTEK struct {
	dial ctekDialFn
}

// NewCTEK builds a CTEK provider using the production simonvetter dialer.
func NewCTEK() *CTEK {
	return &CTEK{dial: ctekDialReal}
}

// WithDialer returns a copy of c using the supplied dialer. For tests.
func (c *CTEK) WithDialer(d ctekDialFn) *CTEK {
	cp := *c
	cp.dial = d
	return &cp
}

// Describe is the wizard's hook for rendering a CTEK-flavored form
// (Modbus transport, no auth, port 502 / unit 1 defaults).
func (c *CTEK) Describe() Descriptor {
	return Descriptor{
		Name:          "ctek",
		Label:         "CTEK Chargestorm",
		Transport:     TransportModbus,
		NeedsAuth:     false,
		DefaultPort:   ctekDefaultPort,
		DefaultUnitID: ctekDefaultUnitID,
		LuaDriver:     "drivers/ctek.lua",
	}
}

// ListChargers dials the configured Modbus host and reads the identity
// registers. Returns one Charger entry — CTEK exposes a single outlet
// per unit ID; multi-outlet stations are picked by configuring two
// EVCharger blocks (different unit_id) in two separate setups.
func (c *CTEK) ListChargers(cfg *config.EVCharger) ([]Charger, error) {
	if cfg == nil || cfg.Modbus == nil {
		return nil, errors.New("ctek: modbus block required")
	}
	host := cfg.Modbus.Host
	if host == "" {
		return nil, errors.New("ctek: modbus.host required")
	}
	port := cfg.Modbus.Port
	if port == 0 {
		port = ctekDefaultPort
	}
	unitID := cfg.Modbus.UnitID
	if unitID == 0 {
		unitID = ctekDefaultUnitID
	}

	cli, err := c.dial(host, port, unitID)
	if err != nil {
		return nil, fmt.Errorf("ctek: dial %s:%d (unit %d): %w", host, port, unitID, err)
	}
	defer cli.Close()

	regs, err := cli.ReadHolding(ctekRegSerialBase, ctekRegSerialCount)
	if err != nil {
		return nil, fmt.Errorf("ctek: read serial: %w", err)
	}
	serial := decodeCTEKSerial(regs)
	if serial == "" {
		return nil, errors.New("ctek: empty serial — wrong API version or wrong unit_id?")
	}
	return []Charger{{ID: serial, Name: "CTEK Chargestorm " + serial}}, nil
}

// decodeCTEKSerial unpacks 6 big-endian u16 registers into 12 ASCII chars.
// Trailing NULs and spaces are trimmed since some firmware right-pads.
func decodeCTEKSerial(regs []uint16) string {
	if len(regs) != ctekRegSerialCount {
		return ""
	}
	buf := make([]byte, 0, 12)
	for _, r := range regs {
		hi := byte(r >> 8)
		lo := byte(r & 0xff)
		if hi == 0 && lo == 0 {
			break
		}
		buf = append(buf, hi)
		if lo == 0 {
			break
		}
		buf = append(buf, lo)
	}
	return strings.TrimRight(strings.TrimSpace(string(buf)), "\x00")
}

// ctekDialReal is the production dialer — opens a real Modbus/TCP socket
// via simonvetter, sets the unit ID, and returns a thin adapter that
// implements ctekClient.
func ctekDialReal(host string, port, unitID int) (ctekClient, error) {
	url := fmt.Sprintf("tcp://%s:%d", host, port)
	cli, err := sv.NewClient(&sv.ClientConfiguration{
		URL:     url,
		Timeout: ctekProbeTimeout,
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
	return &ctekRealClient{cli: cli}, nil
}

type ctekRealClient struct{ cli *sv.ModbusClient }

func (r *ctekRealClient) ReadHolding(addr, count uint16) ([]uint16, error) {
	return r.cli.ReadRegisters(addr, count, sv.HOLDING_REGISTER)
}

func (r *ctekRealClient) Close() error { return r.cli.Close() }
