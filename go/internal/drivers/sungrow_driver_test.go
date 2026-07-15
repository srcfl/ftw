package drivers

import (
	"context"
	"encoding/json"
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
