package modbus

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/drivers"
)

func TestEngineSharesOneSocketPerHostPort(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	engine := NewEngine()

	a, err := engine.Open(host, port, 1, false)
	if err != nil {
		t.Fatalf("open a: %v", err)
	}
	defer a.Close()
	b, err := engine.Open(host, port, 2, false)
	if err != nil {
		t.Fatalf("open b: %v", err)
	}
	defer b.Close()

	if got := engine.sessionCount(); got != 1 {
		t.Fatalf("sessions = %d, want 1", got)
	}
	if got := engine.refCount(sessionKey(host, port)); got != 2 {
		t.Fatalf("refs = %d, want 2", got)
	}

	regs, err := a.Read(1, 1, drivers.ModbusHolding)
	if err != nil {
		t.Fatalf("read a: %v", err)
	}
	if len(regs) != 1 || regs[0] != 0x1111 {
		t.Fatalf("read a = %v, want [0x1111]", regs)
	}
	regs, err = b.Read(1, 1, drivers.ModbusHolding)
	if err != nil {
		t.Fatalf("read b: %v", err)
	}
	if len(regs) != 1 || regs[0] != 0x1111 {
		t.Fatalf("read b = %v, want [0x1111]", regs)
	}

	if got := slave.Accepts(); got != 1 {
		t.Fatalf("backend accepts = %d, want 1 shared socket", got)
	}
	units := slave.Units()
	if len(units) < 2 || units[0] != 1 || units[1] != 2 {
		t.Fatalf("unit IDs = %v, want [1 2 …]", units)
	}

	if err := a.Close(); err != nil {
		t.Fatalf("close a: %v", err)
	}
	if engine.sessionCount() != 1 {
		t.Fatal("closing one handle dropped the shared session")
	}
	if _, err := b.Read(1, 1, drivers.ModbusHolding); err != nil {
		t.Fatalf("read after peer close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close b: %v", err)
	}
	if engine.sessionCount() != 0 {
		t.Fatalf("sessions after last close = %d, want 0", engine.sessionCount())
	}
}

func TestDialJoinsTheSharedSession(t *testing.T) {
	slave := startTestSlave(t)
	host, port := slave.Addr()
	engine := NewEngine()

	pooled, err := engine.Open(host, port, 1, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pooled.Close()
	viaDial, err := Dial(host, port, 1)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer viaDial.Close()

	if _, err := pooled.Read(1, 1, drivers.ModbusHolding); err != nil {
		t.Fatalf("pooled read: %v", err)
	}
	if _, err := viaDial.Read(1, 1, drivers.ModbusHolding); err != nil {
		t.Fatalf("dial read: %v", err)
	}
	if got := slave.Accepts(); got != 1 {
		t.Fatalf("accepts = %d, want 1 (Open and Dial share)", got)
	}
}

func TestEngineOpenRejectsBadEndpoint(t *testing.T) {
	engine := NewEngine()
	if _, err := engine.Open("", 502, 1, false); err == nil {
		t.Fatal("expected error for empty host")
	}
}
