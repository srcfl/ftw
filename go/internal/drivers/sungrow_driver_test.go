package drivers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

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

	d, err := NewLuaDriver("../../../drivers/sungrow.lua", env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

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

func TestSungrowPollSurfacesAndClearsThermalDeviceFault(t *testing.T) {
	tel := telemetry.NewStore()
	modbus := newRecordingModbus()
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
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("recovery poll: %v", err)
	}
	health = tel.DriverHealth("sungrow")
	if health == nil || health.DeviceFault {
		t.Fatalf("health after recovery = %+v, want fault cleared", health)
	}
}
