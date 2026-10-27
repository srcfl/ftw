package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type modbusWrite struct {
	addr  uint16
	value uint16
}

type recordingModbus struct {
	regs   map[uint16]uint16
	writes []modbusWrite
}

func newRecordingModbus() *recordingModbus {
	return &recordingModbus{regs: make(map[uint16]uint16)}
}

func (m *recordingModbus) Read(addr uint16, count uint16, _ int32) ([]uint16, error) {
	out := make([]uint16, count)
	for i := range out {
		out[i] = m.regs[addr+uint16(i)]
	}
	return out, nil
}

func (m *recordingModbus) WriteSingle(addr uint16, value uint16) error {
	m.writes = append(m.writes, modbusWrite{addr: addr, value: value})
	m.regs[addr] = value
	return nil
}

func (m *recordingModbus) WriteMulti(addr uint16, values []uint16) error {
	for i, value := range values {
		if err := m.WriteSingle(addr+uint16(i), value); err != nil {
			return err
		}
	}
	return nil
}

func (m *recordingModbus) Close() error { return nil }

func (m *recordingModbus) resetWrites() { m.writes = nil }

func TestSungrowZeroBatteryCommandForcesIdle(t *testing.T) {
	tel := telemetry.NewStore()
	modbus := newRecordingModbus()
	env := NewHostEnv("sungrow", tel).WithModbus(modbus)
	env.BatteryCapacityWh = 9600

	// Answer register 4999 as an SH8.0RT so the driver classifies this as a
	// hybrid. Sungrow ships two families behind one driver and an SG string
	// inverter implements none of 13049-13051, so the driver refuses a battery
	// setpoint until the model has named itself a hybrid or a battery register
	// has answered. Without this the mock answers 0 everywhere, detection
	// settles on "unknown", and the refusal is correct rather than a bug --
	// see srcfl/device-drivers#40 and #43.
	modbus.regs[4999] = 0x0E0E

	d, err := NewLuaDriver("../../../drivers/sungrow.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	modbus.resetWrites()

	cmd, _ := json.Marshal(map[string]any{"action": "battery", "power_w": 0})
	if err := d.Command(context.Background(), cmd); err != nil {
		t.Fatalf("command: %v", err)
	}

	wantWrites := []modbusWrite{
		{addr: 13049, value: 2},
		{addr: 13050, value: 0xCC},
		{addr: 13051, value: 0},
	}
	if len(modbus.writes) < len(wantWrites) {
		t.Fatalf("writes = %+v, want at least %+v", modbus.writes, wantWrites)
	}
	for i, want := range wantWrites {
		if got := modbus.writes[i]; got != want {
			t.Fatalf("write %d = %+v, want %+v; all writes %+v", i, got, want, modbus.writes)
		}
	}
	if got := modbus.regs[13049]; got != 2 {
		t.Fatalf("EMS mode = %d, want forced mode 2", got)
	}
	if got := modbus.regs[13051]; got != 0 {
		t.Fatalf("force power = %d, want 0W", got)
	}
}

func TestSungrowDefaultModeReturnsToSelfConsumption(t *testing.T) {
	tel := telemetry.NewStore()
	modbus := newRecordingModbus()
	env := NewHostEnv("sungrow", tel).WithModbus(modbus)
	env.BatteryCapacityWh = 9600

	d, err := NewLuaDriver("../../../drivers/sungrow.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

	if err := d.DefaultMode(); err != nil {
		t.Fatalf("default mode: %v", err)
	}

	wantWrites := []modbusWrite{
		{addr: 13050, value: 0xCC},
		{addr: 13051, value: 0},
		{addr: 13049, value: 0},
	}
	if len(modbus.writes) < len(wantWrites) {
		t.Fatalf("writes = %+v, want at least %+v", modbus.writes, wantWrites)
	}
	for i, want := range wantWrites {
		if got := modbus.writes[i]; got != want {
			t.Fatalf("write %d = %+v, want %+v; all writes %+v", i, got, want, modbus.writes)
		}
	}
	if got := modbus.regs[13049]; got != 0 {
		t.Fatalf("EMS mode = %d, want self-consumption mode 0", got)
	}
}

func TestSungrowInitResetsStaleForcedControlState(t *testing.T) {
	tel := telemetry.NewStore()
	modbus := newRecordingModbus()
	modbus.regs[13049] = 2
	modbus.regs[13050] = 0xBB
	modbus.regs[13051] = 1780
	env := NewHostEnv("sungrow", tel).WithModbus(modbus)
	d, err := NewLuaDriver("../../../drivers/sungrow.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	wantPrefix := []modbusWrite{
		{addr: 13050, value: 0xCC},
		{addr: 13051, value: 0},
		{addr: 13049, value: 0},
	}
	if len(modbus.writes) < len(wantPrefix) {
		t.Fatalf("writes = %+v, want prefix %+v", modbus.writes, wantPrefix)
	}
	for i, want := range wantPrefix {
		if got := modbus.writes[i]; got != want {
			t.Fatalf("write %d = %+v, want %+v; all writes %+v", i, got, want, modbus.writes)
		}
	}
	if got := modbus.regs[13049]; got != 0 {
		t.Errorf("EMS mode after init = %d, want self-consumption 0", got)
	}
	if got := modbus.regs[13050]; got != 0xCC {
		t.Errorf("EMS command after init = 0x%X, want stop 0xCC", got)
	}
	if got := modbus.regs[13051]; got != 0 {
		t.Errorf("EMS power after init = %d W, want 0", got)
	}
}

func TestSungrowCurtailUsesActivePowerLimitPair(t *testing.T) {
	tel := telemetry.NewStore()
	modbus := newRecordingModbus()
	modbus.regs[5000] = 80 // 8.0 kW rated AC power
	env := NewHostEnv("sungrow", tel).WithModbus(modbus)
	d, err := NewLuaDriver("../../../drivers/sungrow.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{"_supports_pv_curtail": true}); err != nil {
		t.Fatalf("init: %v", err)
	}

	modbus.resetWrites()
	cmd, _ := json.Marshal(map[string]any{"action": "curtail", "power_w": 4000})
	if err := d.Command(context.Background(), cmd); err != nil {
		t.Fatalf("curtail: %v", err)
	}
	// Ratio first, enable last keeps a partially-applied command inert and
	// works through gateways that reject a combined FC 0x10 write.
	want := []modbusWrite{{addr: 13089, value: 500}, {addr: 13088, value: 0xAA}}
	if len(modbus.writes) != len(want) {
		t.Fatalf("curtail writes = %+v, want %+v", modbus.writes, want)
	}
	for i := range want {
		if modbus.writes[i] != want[i] {
			t.Errorf("curtail write %d = %+v, want %+v", i, modbus.writes[i], want[i])
		}
	}

	modbus.resetWrites()
	cmd, _ = json.Marshal(map[string]any{"action": "curtail_disable"})
	if err := d.Command(context.Background(), cmd); err != nil {
		t.Fatalf("curtail_disable: %v", err)
	}
	want = []modbusWrite{{addr: 13088, value: 0x55}, {addr: 13089, value: 1000}}
	if len(modbus.writes) != len(want) {
		t.Fatalf("release writes = %+v, want %+v", modbus.writes, want)
	}
	for i := range want {
		if modbus.writes[i] != want[i] {
			t.Errorf("release write %d = %+v, want %+v", i, modbus.writes[i], want[i])
		}
	}
}

func TestSungrowFeedInCurtailPreservesConfiguredInstallerLimit(t *testing.T) {
	tel := telemetry.NewStore()
	modbus := newRecordingModbus()
	modbus.regs[5000] = 80
	modbus.regs[13073] = 8000
	modbus.regs[13086] = 0xAA
	env := NewHostEnv("sungrow", tel).WithModbus(modbus)
	d, err := NewLuaDriver("../../../drivers/sungrow.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), map[string]any{
		"_supports_pv_curtail": true,
		"pv_curtail_method":    "feed_in",
		"feed_in_release_w":    8000,
	}); err != nil {
		t.Fatalf("init: %v", err)
	}

	modbus.resetWrites()
	cmd, _ := json.Marshal(map[string]any{"action": "curtail", "power_w": 4000})
	if err := d.Command(context.Background(), cmd); err != nil {
		t.Fatalf("curtail: %v", err)
	}
	want := []modbusWrite{{addr: 13073, value: 4000}}
	if len(modbus.writes) != len(want) || modbus.writes[0] != want[0] {
		t.Fatalf("feed-in curtail writes = %+v, want %+v", modbus.writes, want)
	}

	modbus.resetWrites()
	cmd, _ = json.Marshal(map[string]any{"action": "curtail_disable"})
	if err := d.Command(context.Background(), cmd); err != nil {
		t.Fatalf("curtail_disable: %v", err)
	}
	want = []modbusWrite{{addr: 13073, value: 8000}}
	if len(modbus.writes) != len(want) || modbus.writes[0] != want[0] {
		t.Fatalf("feed-in release writes = %+v, want %+v", modbus.writes, want)
	}
	if got := modbus.regs[13086]; got != 0xAA {
		t.Errorf("Feed-in Limitation enable changed to 0x%X, want preserved 0xAA", got)
	}
}

// sungrow.lua reads power every second and its running state, counters and
// fault bits on a full read every five seconds, timed by host.millis. Move
// the driver's clock past one so the next poll is a full read.
func untilNextFullRead(env *HostEnv) {
	env.Start = env.Start.Add(-5 * time.Second)
}

func TestSungrowPollSurfacesAndClearsThermalDeviceFault(t *testing.T) {
	tel := telemetry.NewStore()
	modbus := newRecordingModbus()
	// An SH hybrid identifies itself at 4999, which is how the driver knows the
	// 13xxx block below exists at all. Without it the mock looks like a device
	// that does not answer the register, and the driver deliberately holds off
	// reading a battery block it has no reason to believe is there.
	modbus.regs[4999] = 0x0E0E  // SH10RT
	modbus.regs[12999] = 0x0100 // Fault
	modbus.regs[13055] = 0x0002 // System fault 2, bit 1: high ambient temperature
	modbus.regs[5007] = 761     // 76.1 C
	env := NewHostEnv("sungrow", tel).WithModbus(modbus)
	d, err := NewLuaDriver("../../../drivers/sungrow.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("fault poll: %v", err)
	}
	health := tel.DriverHealth("sungrow")
	if health == nil || !health.DeviceFault {
		t.Fatalf("health = %+v, want device fault", health)
	}
	if !strings.Contains(health.DeviceFaultReason, "high ambient temperature") {
		t.Errorf("fault reason = %q, want thermal diagnosis", health.DeviceFaultReason)
	}

	modbus.regs[12999] = 0x0040 // Running on-grid
	modbus.regs[13055] = 0
	untilNextFullRead(env)
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("recovery poll: %v", err)
	}
	health = tel.DriverHealth("sungrow")
	if health == nil || health.DeviceFault {
		t.Fatalf("health after recovery = %+v, want fault cleared", health)
	}
}

// A hybrid whose device-type register is unreadable must still find its way to
// the 13xxx block. The driver holds off while detection is unresolved -- an SG
// string inverter cannot afford a burst of failed reads, because the host fails
// the whole poll on any failure -- and then falls back to probing. If that
// fallback did not work, an SH inverter with a quiet 4999 would silently lose
// its battery and its fault channel forever.
func TestSungrowProbesTheHybridBlockWhenTheDeviceTypeNeverAnswers(t *testing.T) {
	tel := telemetry.NewStore()
	modbus := newRecordingModbus()
	// 4999 left at 0: the documented "not present" sentinel, indistinguishable
	// from a register the inverter does not implement.
	modbus.regs[12999] = 0x0100 // Fault
	modbus.regs[13055] = 0x0002
	modbus.regs[5007] = 761
	env := NewHostEnv("sungrow", tel).WithModbus(modbus)
	d, err := NewLuaDriver("../../../drivers/sungrow.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

	// Detection gets three tries in total, one of them in driver_init, so it
	// settles within a couple of polls rather than retrying for the session.
	var faulted bool
	for poll := 0; poll < 4; poll++ {
		untilNextFullRead(env)
		if _, err := d.Poll(context.Background()); err != nil {
			t.Fatalf("poll %d: %v", poll, err)
		}
		if health := tel.DriverHealth("sungrow"); health != nil && health.DeviceFault {
			faulted = true
			break
		}
	}
	if !faulted {
		t.Fatalf("a hybrid with an unreadable device type never reached its " +
			"running state; the probing fallback is not working")
	}
}

// offlineModbus fails every read and write the way FTW's Modbus client does
// while the inverter is unreachable: the dial error the site rig logged for
// reads, and the reconnect backoff for writes.
type offlineModbus struct {
	*recordingModbus
	offline bool
}

func (m *offlineModbus) Read(addr uint16, count uint16, kind int32) ([]uint16, error) {
	if m.offline {
		return nil, errors.New("dial tcp 127.0.0.1:502: connect: connection refused")
	}
	return m.recordingModbus.Read(addr, count, kind)
}

func (m *offlineModbus) WriteSingle(addr uint16, value uint16) error {
	if m.offline {
		return fmt.Errorf("%w for 1.243s", ErrModbusBackoff)
	}
	return m.recordingModbus.WriteSingle(addr, value)
}

func (m *offlineModbus) WriteMulti(addr uint16, values []uint16) error {
	if m.offline {
		return fmt.Errorf("%w for 1.243s", ErrModbusBackoff)
	}
	return m.recordingModbus.WriteMulti(addr, values)
}

// An inverter that drops off the network for a while is not an inverter
// without registers. The random site rig (27 October 2026) took the Sungrow
// offline while it held a forced discharge. sungrow 1.6.0 counted the failed
// writes as the device refusing the EMS block, then reported the release as
// held without writing it, so the inverter kept discharging for up to five
// minutes after it came back. It also counted the failed reads as missing
// registers and dropped 13000, which signs the battery.
func TestSungrowOutageKeepsTheReleaseAndTheBatterySign(t *testing.T) {
	tel := telemetry.NewStore()
	modbus := &offlineModbus{recordingModbus: newRecordingModbus()}
	modbus.regs[4999] = 0x0E0E // SH10RT
	modbus.regs[13000] = 0x0004
	modbus.regs[13021] = 900 // battery power, unsigned; 13000 says discharging
	modbus.regs[13022] = 550
	env := NewHostEnv("sungrow", tel).WithModbus(modbus)
	env.BatteryCapacityWh = 9600
	d, err := NewLuaDriver("../../../drivers/sungrow.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	ctx := context.Background()

	if _, err := d.Poll(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	cmd, _ := json.Marshal(map[string]any{"action": "battery", "power_w": -3000})
	if err := d.Command(ctx, cmd); err != nil {
		t.Fatalf("forced discharge: %v", err)
	}

	modbus.offline = true
	for i := 0; i < 6; i++ {
		untilNextFullRead(env)
		_, _ = d.Poll(ctx)
		if err := d.DefaultMode(); err == nil {
			t.Fatalf("default mode %d reported success while nothing reached the device", i)
		}
	}
	modbus.offline = false

	if err := d.DefaultMode(); err != nil {
		t.Fatalf("default mode after the device returned: %v", err)
	}
	if got := modbus.regs[13049]; got != 0 {
		t.Fatalf("EMS mode = %d after the device returned, want 0: the "+
			"release was skipped and the forced discharge kept running", got)
	}

	untilNextFullRead(env)
	if _, err := d.Poll(ctx); err != nil {
		t.Fatalf("poll after the device returned: %v", err)
	}
	reading := tel.Get("sungrow", telemetry.DerBattery)
	if reading == nil || reading.RawW != -900 {
		t.Fatalf("battery reading = %+v, want -900 W: 13000, which signs it, "+
			"was dropped during the outage", reading)
	}
}
