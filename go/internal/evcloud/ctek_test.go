package evcloud

import (
	"errors"
	"strings"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

// fakeCTekClient is a stub ctekClient that returns canned register
// content. Lets us test the picker without standing up a real Modbus
// server.
type fakeCTekClient struct {
	regs []uint16
	err  error
}

func (f *fakeCTekClient) ReadHolding(addr, count uint16) ([]uint16, error) {
	if f.err != nil {
		return nil, f.err
	}
	if int(count) != len(f.regs) {
		return nil, errors.New("count mismatch")
	}
	return f.regs, nil
}

func (f *fakeCTekClient) Close() error { return nil }

// asciiRegs packs an ASCII serial into 6 big-endian u16 registers, padding
// with NUL when the input is shorter than 12 chars.
func asciiRegs(s string) []uint16 {
	b := []byte(s)
	if len(b) < 12 {
		b = append(b, make([]byte, 12-len(b))...)
	}
	out := make([]uint16, 6)
	for i := 0; i < 6; i++ {
		out[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return out
}

func TestCTekListChargersHappyPath(t *testing.T) {
	dialed := struct {
		host   string
		port   int
		unitID int
	}{}
	dialer := func(host string, port, unitID int) (ctekClient, error) {
		dialed.host = host
		dialed.port = port
		dialed.unitID = unitID
		return &fakeCTekClient{regs: asciiRegs("123456789012")}, nil
	}

	c := NewCTEK().WithDialer(dialer)
	got, err := c.ListChargers(&config.EVCharger{
		Provider: "ctek",
		Modbus:   &config.EVChargerModbus{Host: "10.0.0.5", Port: 1502, UnitID: 2},
	})
	if err != nil {
		t.Fatalf("ListChargers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("chargers: got %d, want 1", len(got))
	}
	if got[0].ID != "123456789012" {
		t.Errorf("serial: got %q, want %q", got[0].ID, "123456789012")
	}
	if dialed.host != "10.0.0.5" || dialed.port != 1502 || dialed.unitID != 2 {
		t.Errorf("dialer args: got %+v, want {10.0.0.5 1502 2}", dialed)
	}
}

func TestCTekListChargersDefaultsPortAndUnit(t *testing.T) {
	var seenPort, seenUnit int
	dialer := func(host string, port, unitID int) (ctekClient, error) {
		seenPort = port
		seenUnit = unitID
		return &fakeCTekClient{regs: asciiRegs("SN-ABC")}, nil
	}
	c := NewCTEK().WithDialer(dialer)
	_, err := c.ListChargers(&config.EVCharger{
		Provider: "ctek",
		Modbus:   &config.EVChargerModbus{Host: "host"},
	})
	if err != nil {
		t.Fatalf("ListChargers: %v", err)
	}
	if seenPort != 502 || seenUnit != 1 {
		t.Errorf("defaults: got port=%d unit=%d, want 502/1", seenPort, seenUnit)
	}
}

func TestCTekListChargersRequiresHost(t *testing.T) {
	c := NewCTEK()
	_, err := c.ListChargers(&config.EVCharger{
		Provider: "ctek",
		Modbus:   &config.EVChargerModbus{},
	})
	if err == nil {
		t.Fatal("expected error for empty host")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("error should mention host: %v", err)
	}
}

func TestCTekListChargersDialFailure(t *testing.T) {
	dialer := func(host string, port, unitID int) (ctekClient, error) {
		return nil, errors.New("connection refused")
	}
	c := NewCTEK().WithDialer(dialer)
	_, err := c.ListChargers(&config.EVCharger{
		Provider: "ctek",
		Modbus:   &config.EVChargerModbus{Host: "host"},
	})
	if err == nil {
		t.Fatal("expected dial error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("dial error not wrapped: %v", err)
	}
}

func TestCTekListChargersEmptySerialIsError(t *testing.T) {
	dialer := func(host string, port, unitID int) (ctekClient, error) {
		return &fakeCTekClient{regs: []uint16{0, 0, 0, 0, 0, 0}}, nil
	}
	c := NewCTEK().WithDialer(dialer)
	_, err := c.ListChargers(&config.EVCharger{
		Provider: "ctek",
		Modbus:   &config.EVChargerModbus{Host: "host"},
	})
	if err == nil {
		t.Fatal("expected error for empty serial")
	}
}

func TestDecodeCTEKSerialTrimsNulAndSpace(t *testing.T) {
	got := decodeCTEKSerial(asciiRegs("AB12  "))
	if got != "AB12" {
		t.Errorf("decoded: got %q, want %q", got, "AB12")
	}
}

func TestCTekDescribe(t *testing.T) {
	d := NewCTEK().Describe()
	if d.Name != "ctek" || d.Transport != TransportModbus || d.NeedsAuth {
		t.Errorf("descriptor: got %+v", d)
	}
	if d.DefaultPort != 502 || d.DefaultUnitID != 1 {
		t.Errorf("defaults: got port=%d unit=%d, want 502/1", d.DefaultPort, d.DefaultUnitID)
	}
}
