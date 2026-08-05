package drivers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type readResultModbus struct {
	registers       []uint16
	err             error
	errorsByAddress map[uint16]error
}

func (m readResultModbus) Read(address uint16, _ uint16, _ int32) ([]uint16, error) {
	if err := m.errorsByAddress[address]; err != nil {
		return nil, err
	}
	return m.registers, m.err
}
func (readResultModbus) WriteSingle(uint16, uint16) error  { return nil }
func (readResultModbus) WriteMulti(uint16, []uint16) error { return nil }
func (readResultModbus) Close() error                      { return nil }

func TestModbusDriverDoesNotEmitWhenTransportIsDown(t *testing.T) {
	tel := telemetry.NewStore()
	driver := newModbusEvidenceDriver(t, tel, readResultModbus{
		err: fmt.Errorf("%w: i/o timeout", ErrModbusTransport),
	})

	_, err := driver.Poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not reach the device") {
		t.Fatalf("Poll error = %v, want an unreachable-device error", err)
	}
	if health := tel.DriverHealth("modbus-evidence"); health != nil && health.LastSuccess != nil {
		t.Fatalf("failed read advanced LastSuccess: %+v", health)
	}
	if readings := tel.ReadingsByDriver("modbus-evidence"); len(readings) != 0 {
		t.Fatalf("failed read stored %d readings", len(readings))
	}
}

// One unreachable register still condemns the poll: we cannot tell which of
// the readings that did arrive were current, so none of them are trusted.
// This is the invariant that stops dispatch on a stale site meter.
func TestModbusDriverDoesNotEmitWhenOneReadHitsTheTransport(t *testing.T) {
	tel := telemetry.NewStore()
	driver := newModbusEvidenceDriver(t, tel, readResultModbus{
		registers: []uint16{321},
		errorsByAddress: map[uint16]error{
			11: fmt.Errorf("%w: connection reset", ErrModbusTransport),
		},
	})

	_, err := driver.Poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "1 of 2 modbus reads could not reach the device") {
		t.Fatalf("Poll error = %v, want partial-transport error", err)
	}
	if readings := tel.ReadingsByDriver("modbus-evidence"); len(readings) != 0 {
		t.Fatalf("partial transport failure stored %d readings", len(readings))
	}
	if health := tel.DriverHealth("modbus-evidence"); health != nil && health.LastSuccess != nil {
		t.Fatalf("partial transport failure advanced LastSuccess: %+v", health)
	}
}

// A device that answers and refuses a register is alive, and its other
// readings are current. A string inverter has no battery registers and will
// refuse them on every poll for as long as it is installed; the old rule
// threw away the whole poll each time, which made the driver useless on that
// hardware. sungrow.lua marks 19 of its 20 reads optional for exactly this
// reason — the host now honours that instead of overruling it.
func TestModbusDriverEmitsWhenDeviceRefusesAnOptionalRegister(t *testing.T) {
	tel := telemetry.NewStore()
	driver := newModbusEvidenceDriver(t, tel, readResultModbus{
		registers: []uint16{321},
		errorsByAddress: map[uint16]error{
			11: errors.New("modbus exception 2: illegal data address"),
		},
	})

	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("a refused optional register failed the poll: %v", err)
	}
	reading := tel.Get("modbus-evidence", telemetry.DerMeter)
	if reading == nil || reading.RawW != 321 {
		t.Fatalf("meter reading = %+v, want 321 W kept", reading)
	}
	if health := tel.DriverHealth("modbus-evidence"); health == nil || health.LastSuccess == nil {
		t.Fatalf("a live device did not advance LastSuccess: %+v", health)
	}
}

// Refusal is only evidence of life when something else actually read. A
// device refusing everything has told us nothing we can use.
func TestModbusDriverDoesNotEmitWhenDeviceRefusesEverything(t *testing.T) {
	tel := telemetry.NewStore()
	driver := newModbusEvidenceDriver(t, tel, readResultModbus{
		err: errors.New("modbus exception 2: illegal data address"),
	})

	_, err := driver.Poll(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused all 2 modbus reads") {
		t.Fatalf("Poll error = %v, want an all-refused error", err)
	}
	if readings := tel.ReadingsByDriver("modbus-evidence"); len(readings) != 0 {
		t.Fatalf("all-refused poll stored %d readings", len(readings))
	}
}

// Reads skipped by a reconnect backoff are downstream of one transport
// failure, not fresh evidence of several. The message has to say so, or it
// points the reader at a flood of failures that never happened.
func TestBackoffSkipsAreNotCountedAsTransportFailures(t *testing.T) {
	ev := pollEvidence{Attempts: 20, Successes: 12, Transport: 1, Skipped: 7}
	if ev.fresh() {
		t.Error("a transport failure must still condemn the poll")
	}
	got := ev.describe()
	if !strings.Contains(got, "1 of 20 modbus reads could not reach the device") {
		t.Errorf("describe() = %q, want the real failure count", got)
	}
	if !strings.Contains(got, "7 more skipped while reconnecting") {
		t.Errorf("describe() = %q, want the skipped reads named separately", got)
	}
}

func TestModbusDriverEmitsAfterSuccessfulRead(t *testing.T) {
	tel := telemetry.NewStore()
	driver := newModbusEvidenceDriver(t, tel, readResultModbus{registers: []uint16{321}})

	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	reading := tel.Get("modbus-evidence", telemetry.DerMeter)
	if reading == nil || reading.RawW != 321 {
		t.Fatalf("meter reading = %+v, want 321 W", reading)
	}
	if health := tel.DriverHealth("modbus-evidence"); health == nil || health.LastSuccess == nil {
		t.Fatalf("successful read did not advance LastSuccess: %+v", health)
	}
}

func newModbusEvidenceDriver(t *testing.T, tel *telemetry.Store, modbus ModbusCap) *LuaDriver {
	t.Helper()
	source := `
function driver_init() end
function driver_poll()
    local ok, registers = pcall(host.modbus_read, 10, 1, "holding")
    local watts = 0
    if ok and registers then watts = registers[1] end
    host.emit("meter", { w = watts })
    pcall(host.modbus_read, 11, 1, "holding")
    host.emit("pv", { w = watts })
    return 1000
end
`
	path := filepath.Join(t.TempDir(), "modbus_evidence.lua")
	if err := os.WriteFile(path, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	driver, err := NewLuaDriver(path, NewHostEnv("modbus-evidence", tel).WithModbus(modbus))
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	t.Cleanup(driver.Cleanup)
	if err := driver.Init(context.Background(), nil); err != nil {
		t.Fatalf("init driver: %v", err)
	}
	return driver
}

// The reported case, at the shape it actually occurs: a string inverter
// refusing its battery registers on every poll, for as long as it is
// installed. Under the old rule this driver never emitted a single reading
// on that hardware. Run several polls, because a fix that works once and
// then latches would look identical in a single-poll test.
func TestStringInverterKeepsReportingAcrossPolls(t *testing.T) {
	tel := telemetry.NewStore()
	driver := newModbusEvidenceDriver(t, tel, readResultModbus{
		registers: []uint16{4200},
		errorsByAddress: map[uint16]error{
			// The battery block this model does not have.
			11: errors.New("modbus exception 2: illegal data address"),
		},
	})

	for poll := 1; poll <= 5; poll++ {
		if _, err := driver.Poll(context.Background()); err != nil {
			t.Fatalf("poll %d failed on a healthy string inverter: %v", poll, err)
		}
		reading := tel.Get("modbus-evidence", telemetry.DerMeter)
		if reading == nil || reading.RawW != 4200 {
			t.Fatalf("poll %d: meter reading = %+v, want 4200 W", poll, reading)
		}
	}
	if health := tel.DriverHealth("modbus-evidence"); health == nil || health.LastSuccess == nil {
		t.Fatalf("string inverter never advanced LastSuccess: %+v", health)
	}
}

// And the guarantee that has to survive the fix: a site meter that goes
// unreachable still stops dispatch. AGENTS.md — "stale site-meter data stops
// dispatch" — depends on this poll failing, not on it being forgiving.
func TestUnreachableMeterStillFailsEveryPoll(t *testing.T) {
	tel := telemetry.NewStore()
	driver := newModbusEvidenceDriver(t, tel, readResultModbus{
		err: fmt.Errorf("%w: connection refused", ErrModbusTransport),
	})

	for poll := 1; poll <= 3; poll++ {
		if _, err := driver.Poll(context.Background()); err == nil {
			t.Fatalf("poll %d succeeded against an unreachable device", poll)
		}
	}
	if readings := tel.ReadingsByDriver("modbus-evidence"); len(readings) != 0 {
		t.Fatalf("unreachable device stored %d readings", len(readings))
	}
}
