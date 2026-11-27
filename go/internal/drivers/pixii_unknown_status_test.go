package drivers

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Exercise the bundled Lua in FTW's host, including its persistent
// calibration state.
func TestPixiiUnknownStatusPreservesCalibration(t *testing.T) {
	tel := telemetry.NewStore()
	modbus := newPixiiTestModbus()
	env := NewHostEnv("pixii", tel).WithModbus(modbus)
	env.BatteryCapacityWh = 10000
	d, err := NewLuaDriver(filepath.Join("..", "..", "..", "drivers", "pixii.lua"), env)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		code        uint16
		calibrating bool
		label       string
	}{
		{0xffff, false, "unknown"},
		{7, true, "testing"},
		{0xffff, true, "unknown"},
		{99, true, "unknown_99"},
		{4, false, "charging"},
	} {
		modbus.regs[40137] = step.code
		if _, err := d.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		health := tel.DriverHealth("pixii")
		if health == nil || health.Calibrating != step.calibrating || health.CanControl() == step.calibrating ||
			health.DeviceFault || !health.IsOnline() {
			t.Fatalf("charge status %d: health = %+v, want calibrating=%v and no fault", step.code, health, step.calibrating)
		}
		reading := tel.Get("pixii", telemetry.DerBattery)
		if reading == nil || tel.Get("pixii", telemetry.DerMeter) == nil {
			t.Fatal("lost battery or meter telemetry")
		}
		var data map[string]any
		if err := json.Unmarshal(reading.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["charge_status"] != step.label {
			t.Fatalf("charge status %d: label=%v, want %s", step.code, data["charge_status"], step.label)
		}
	}
}
