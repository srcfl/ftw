package drivers

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

type pixiiTestModbus struct {
	regs        map[uint16]uint16
	writeSingle []struct {
		addr  uint16
		value uint16
	}
	writeMulti []struct {
		addr   uint16
		values []uint16
	}
}

func newPixiiTestModbus() *pixiiTestModbus {
	return &pixiiTestModbus{regs: map[uint16]uint16{
		40000: 0x5375, // "Su"
		40001: 0x6e53, // "nS"
		40132: 94,     // SoC %
		40137: 7,      // SunSpec 802 ChaSt TESTING
		40138: 0,      // remote control
		40143: 2,      // initializing
		40144: 42,     // vendor state, opaque
		40147: 0,
		40148: 1, // event bitfield
	}}
}

func (m *pixiiTestModbus) Read(addr, count uint16, kind int32) ([]uint16, error) {
	out := make([]uint16, count)
	for i := range out {
		out[i] = m.regs[addr+uint16(i)]
	}
	return out, nil
}

func (m *pixiiTestModbus) WriteSingle(addr, value uint16) error {
	m.writeSingle = append(m.writeSingle, struct {
		addr  uint16
		value uint16
	}{addr: addr, value: value})
	m.regs[addr] = value
	return nil
}

func (m *pixiiTestModbus) WriteMulti(addr uint16, values []uint16) error {
	cp := append([]uint16(nil), values...)
	m.writeMulti = append(m.writeMulti, struct {
		addr   uint16
		values []uint16
	}{addr: addr, values: cp})
	for i, value := range values {
		m.regs[addr+uint16(i)] = value
	}
	return nil
}

func (m *pixiiTestModbus) Close() error { return nil }

func TestPixiiTroubleshootingStatusAndSetpoint(t *testing.T) {
	path := filepath.Join("..", "..", "..", "drivers", "pixii.lua")
	tel := telemetry.NewStore()
	modbus := newPixiiTestModbus()
	env := NewHostEnv("pixii", tel).WithModbus(modbus)
	env.BatteryCapacityWh = 10000

	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load pixii driver: %v", err)
	}
	defer d.Cleanup()

	if err := d.Init(context.Background(), map[string]any{"troubleshooting_mode": true}); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	got := tel.Get("pixii", telemetry.DerBattery)
	if got == nil {
		t.Fatal("expected battery reading")
	}
	if got.SoC == nil || *got.SoC != 0.94 {
		t.Fatalf("SoC = %v, want 0.94", got.SoC)
	}
	var data map[string]any
	if err := json.Unmarshal(got.Data, &data); err != nil {
		t.Fatalf("decode battery data: %v", err)
	}
	if data["charge_status"] != "testing" {
		t.Fatalf("charge_status = %v, want testing", data["charge_status"])
	}
	if data["control_mode"] != "remote" {
		t.Fatalf("control_mode = %v, want remote", data["control_mode"])
	}

	if err := d.Command(context.Background(), []byte(`{"action":"battery","power_w":-1000}`)); err != nil {
		t.Fatalf("command: %v", err)
	}
	if len(modbus.writeMulti) == 0 {
		t.Fatal("expected setpoint write")
	}
	last := modbus.writeMulti[len(modbus.writeMulti)-1]
	if last.addr != 39905 || len(last.values) != 2 || last.values[0] != 0 || last.values[1] != 1000 {
		t.Fatalf("setpoint write = addr %d values %v, want addr 39905 [0 1000]", last.addr, last.values)
	}

	samples := tel.FlushSamples()
	var sawStatus, sawSetpoint bool
	for _, sample := range samples {
		if sample.Driver == "pixii" && sample.Metric == "battery_charge_status_code" && sample.Value == 7 {
			sawStatus = true
		}
		if sample.Driver == "pixii" && sample.Metric == "pixii_setpoint_ems_w" && sample.Value == -1000 {
			sawSetpoint = true
		}
	}
	if !sawStatus {
		t.Fatal("expected battery_charge_status_code=7 metric")
	}
	if !sawSetpoint {
		t.Fatal("expected pixii_setpoint_ems_w=-1000 metric")
	}
}
