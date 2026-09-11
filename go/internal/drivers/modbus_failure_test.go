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

// probeGiveUpSource is the catalog pattern that took Pixii offline after a
// network blip: three failed reads permanently skip the register, and a
// transport outage looks identical to an unimplemented point.
const probeGiveUpSource = `
PROTOCOL = "modbus"
local GIVE_UP_AFTER = 3
local read_failures = {}
local function probe_read(addr, count, kind)
    if (read_failures[addr] or 0) >= GIVE_UP_AFTER then return nil end
    local ok, regs = pcall(host.modbus_read, addr, count, kind)
    if ok and regs and regs[1] ~= nil then
        read_failures[addr] = nil
        return regs
    end
    read_failures[addr] = (read_failures[addr] or 0) + 1
    return nil
end
function driver_init() end
function driver_poll()
    local regs = probe_read(10, 1, "holding")
    probe_read(11, 1, "holding")
    local watts = 0
    if regs then watts = regs[1] end
    host.emit("meter", { w = watts })
    return 1000
end
`

type toggleModbus struct {
	registers       []uint16
	errorsByAddress map[uint16]error
	down            bool
	reads           map[uint16]int
}

func (m *toggleModbus) Read(addr, count uint16, _ int32) ([]uint16, error) {
	if m.reads == nil {
		m.reads = map[uint16]int{}
	}
	m.reads[addr]++
	if m.down {
		return nil, fmt.Errorf("%w: no route to host", ErrModbusTransport)
	}
	if err := m.errorsByAddress[addr]; err != nil {
		return nil, err
	}
	return m.registers, nil
}
func (m *toggleModbus) WriteSingle(uint16, uint16) error  { return nil }
func (m *toggleModbus) WriteMulti(uint16, []uint16) error { return nil }
func (m *toggleModbus) Close() error                      { return nil }

func newGiveUpDriver(t *testing.T, tel *telemetry.Store, modbus ModbusCap) *LuaDriver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "give_up.lua")
	if err := os.WriteFile(path, []byte(probeGiveUpSource), 0o644); err != nil {
		t.Fatal(err)
	}
	driver, err := NewLuaDriver(path, NewHostEnv("give-up", tel).WithModbus(modbus))
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	t.Cleanup(driver.Cleanup)
	if err := driver.Init(context.Background(), nil); err != nil {
		t.Fatalf("init driver: %v", err)
	}
	return driver
}

// A short network blip used to permanently silence every register: three
// failed polls skip them all, the TCP session comes back, and the driver
// never asks again. The host must reload and resume on the next poll.
func TestGiveUpDriverRecoversAfterTransportBlip(t *testing.T) {
	tel := telemetry.NewStore()
	bus := &toggleModbus{registers: []uint16{321}}
	driver := newGiveUpDriver(t, tel, bus)

	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("live poll: %v", err)
	}
	if reading := tel.Get("give-up", telemetry.DerMeter); reading == nil || reading.RawW != 321 {
		t.Fatalf("live meter = %+v, want 321 W", reading)
	}

	bus.down = true
	for poll := 1; poll <= 3; poll++ {
		if _, err := driver.Poll(context.Background()); err == nil {
			t.Fatalf("outage poll %d succeeded", poll)
		}
	}

	bus.down = false
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("recovery poll after the link returned: %v", err)
	}
	if driver.reprobes() != 1 {
		t.Fatalf("reprobes = %d, want 1 reload once the driver had nothing left to ask", driver.reprobes())
	}
	reading := tel.Get("give-up", telemetry.DerMeter)
	if reading == nil || reading.RawW != 321 {
		t.Fatalf("recovered meter = %+v, want 321 W", reading)
	}
	health := tel.DriverHealth("give-up")
	if health == nil || health.LastSuccess == nil {
		t.Fatalf("recovery did not advance LastSuccess: %+v", health)
	}
}

// An unimplemented register must stay skipped. Reloading would undo the
// absent-register fix and fail the poll forever on that firmware.
func TestGiveUpOnAbsentRegisterDoesNotReload(t *testing.T) {
	tel := telemetry.NewStore()
	bus := &toggleModbus{
		registers: []uint16{321},
		errorsByAddress: map[uint16]error{
			11: errors.New("modbus exception 2: illegal data address"),
		},
	}
	driver := newGiveUpDriver(t, tel, bus)

	for poll := 1; poll <= 6; poll++ {
		if _, err := driver.Poll(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
	}
	if driver.reprobes() != 0 {
		t.Fatalf("reprobes = %d, want 0: an absent optional register is not a dead link", driver.reprobes())
	}
	if bus.reads[11] > 3 {
		t.Fatalf("absent register 11 was read %d times, want at most 3", bus.reads[11])
	}
	if bus.reads[10] < 6 {
		t.Fatalf("present register 10 was read %d times, want every poll", bus.reads[10])
	}
	if reading := tel.Get("give-up", telemetry.DerMeter); reading == nil || reading.RawW != 321 {
		t.Fatalf("meter = %+v, want 321 W kept while 11 is absent", reading)
	}
}

func pollLiveThenGiveUp(t *testing.T, driver *LuaDriver, bus *toggleModbus) {
	t.Helper()
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("live poll: %v", err)
	}
	bus.down = true
	for poll := 1; poll <= 3; poll++ {
		if _, err := driver.Poll(context.Background()); err == nil {
			t.Fatalf("outage poll %d succeeded", poll)
		}
	}
}

// A missing driver file must not become a per-poll reload loop. The PR
// promised that a failed reload latches until process restart.
func TestGiveUpFailedReloadLatches(t *testing.T) {
	tel := telemetry.NewStore()
	bus := &toggleModbus{registers: []uint16{321}}
	driver := newGiveUpDriver(t, tel, bus)
	pollLiveThenGiveUp(t, driver, bus)

	src, err := os.ReadFile(driver.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(driver.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Poll(context.Background()); err == nil {
		t.Fatal("expected reprobe error after the driver file disappeared")
	}
	if err := os.WriteFile(driver.Path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("given-up poll after a failed reload: %v", err)
	}
	if driver.reprobes() != 0 {
		t.Fatalf("reprobes = %d, want 0: a failed reload latches", driver.reprobes())
	}
}

// driver_init must succeed on the candidate VM before we throw the old one
// away. Otherwise default-mode runs against an uninitialized state.
func TestGiveUpKeepsOldVMWhenReprobeInitFails(t *testing.T) {
	const withDefault = probeGiveUpSource + `
function driver_default_mode()
    host.emit("meter", { w = 42 })
end
`
	const failingInit = `
PROTOCOL = "modbus"
function driver_init()
    error("init failed")
end
function driver_poll()
    host.modbus_read(10, 1, "holding")
    host.emit("meter", { w = 999 })
    return 1000
end
function driver_default_mode()
    error("new vm")
end
`
	tel := telemetry.NewStore()
	bus := &toggleModbus{registers: []uint16{321}}
	path := filepath.Join(t.TempDir(), "give_up.lua")
	if err := os.WriteFile(path, []byte(withDefault), 0o644); err != nil {
		t.Fatal(err)
	}
	driver, err := NewLuaDriver(path, NewHostEnv("give-up", tel).WithModbus(bus))
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	t.Cleanup(driver.Cleanup)
	if err := driver.Init(context.Background(), nil); err != nil {
		t.Fatalf("init driver: %v", err)
	}

	pollLiveThenGiveUp(t, driver, bus)
	readsAfterGiveUp := bus.reads[10]
	if err := os.WriteFile(path, []byte(failingInit), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Poll(context.Background()); err == nil {
		t.Fatal("expected reprobe to fail when driver_init errors")
	}
	if driver.reprobes() != 0 {
		t.Fatalf("reprobes = %d, want 0", driver.reprobes())
	}
	if err := driver.DefaultMode(); err != nil {
		t.Fatalf("default-mode on the kept VM: %v", err)
	}
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("given-up poll on the kept VM: %v", err)
	}
	if bus.reads[10] != readsAfterGiveUp {
		t.Fatalf("register 10 reads = %d, want %d: new VM would have probed again", bus.reads[10], readsAfterGiveUp)
	}
}

// Failed attempts must not arm reprobe. A device that never answered would
// otherwise reload-loop once its give-up tables emptied.
func TestGiveUpNeverOnlineDoesNotReload(t *testing.T) {
	tel := telemetry.NewStore()
	bus := &toggleModbus{registers: []uint16{321}, down: true}
	driver := newGiveUpDriver(t, tel, bus)

	for poll := 1; poll <= 3; poll++ {
		if _, err := driver.Poll(context.Background()); err == nil {
			t.Fatalf("never-online poll %d succeeded", poll)
		}
	}
	for poll := 4; poll <= 6; poll++ {
		if _, err := driver.Poll(context.Background()); err != nil {
			t.Fatalf("given-up poll %d: %v", poll, err)
		}
	}
	if driver.reprobes() != 0 {
		t.Fatalf("reprobes = %d, want 0 before the device has ever been read", driver.reprobes())
	}

	bus.down = false
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("quiet poll after the link appeared: %v", err)
	}
	if driver.reprobes() != 0 {
		t.Fatalf("reprobes = %d, want 0: never-online stays given-up until restart", driver.reprobes())
	}
	if reading := tel.Get("give-up", telemetry.DerMeter); reading != nil {
		t.Fatalf("never-online driver stored meter %+v", reading)
	}
}

// A blip that outlasts the first reload must keep retrying. Latch-after-
// every-reload would recover a 15-second flap and miss the 9-minute one
// that took Pixii offline.
func TestGiveUpDriverRecoversAfterSustainedOutage(t *testing.T) {
	tel := telemetry.NewStore()
	bus := &toggleModbus{registers: []uint16{321}}
	driver := newGiveUpDriver(t, tel, bus)
	pollLiveThenGiveUp(t, driver, bus)

	for poll := 1; poll <= 6; poll++ {
		if _, err := driver.Poll(context.Background()); err == nil {
			t.Fatalf("sustained-outage poll %d succeeded", poll)
		}
	}
	if driver.reprobes() < 1 {
		t.Fatalf("reprobes = %d, want at least one retry while the link stayed down", driver.reprobes())
	}

	bus.down = false
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatalf("recovery poll after a sustained outage: %v", err)
	}
	reading := tel.Get("give-up", telemetry.DerMeter)
	if reading == nil || reading.RawW != 321 {
		t.Fatalf("recovered meter = %+v, want 321 W", reading)
	}
}
