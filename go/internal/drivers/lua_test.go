package drivers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Minimal driver that emits meter + battery via host.emit and bumps the
// poll counter. Exercises the full host API.
const testDriverSrc = `
host.set_make("TestMaker")
host.set_sn("SN-42")
tick = 0
function driver_init(config)
    host.log("info", "init called")
    assert(config ~= nil, "config should be passed")
    assert(config.foo == "bar", "config.foo should be 'bar'")
end
function driver_poll()
    tick = tick + 1
    host.emit("meter", { w = tick * 100 })
    host.emit("battery", { w = -500, soc = 0.87 })
    return 1000
end
function driver_command(action, w, cmd)
    host.log("info", "cmd: " .. tostring(action) .. " w=" .. tostring(w))
    assert(cmd.action == action, "cmd.action matches")
end
`

func TestLuaDriverLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lua")
	if err := os.WriteFile(path, []byte(testDriverSrc), 0644); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	env := NewHostEnv("test", tel)
	env.BatteryCapacityWh = 9600 // declared physical battery — emits flow through

	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

	// Init with config.
	if err := d.Init(context.Background(), map[string]any{"foo": "bar"}); err != nil {
		t.Fatalf("init: %v", err)
	}

	// SN + make captured.
	mk, sn := env.Identity()
	if mk != "TestMaker" || sn != "SN-42" {
		t.Errorf("identity: got (%q, %q)", mk, sn)
	}

	// Poll three times, check telemetry.
	for i := 0; i < 3; i++ {
		next, err := d.Poll(context.Background())
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if next != 1000*time.Millisecond {
			t.Errorf("next poll: %v", next)
		}
	}
	meter := tel.Get("test", telemetry.DerMeter)
	if meter == nil || meter.RawW != 300 {
		t.Errorf("meter: %+v", meter)
	}
	bat := tel.Get("test", telemetry.DerBattery)
	if bat == nil || bat.SoC == nil || *bat.SoC != 0.87 {
		t.Errorf("battery: %+v (soc=%v)", bat, bat.SoC)
	}

	// Command.
	err = d.Command(context.Background(), []byte(`{"action":"set","w":-1500}`))
	if err != nil {
		t.Fatalf("command: %v", err)
	}
}

// A hybrid inverter without a physical battery (operator-declared via
// battery_capacity_wh = 0) still polls battery registers and emits via
// host.emit("battery", …). The host must drop those emits so phantom
// SoC readings never reach the telemetry store, the /api/status drivers
// map, or the frontend's Combined view (which would otherwise mean-
// average a real battery's SoC with the phantom 0 %).
func TestEmitBatteryDroppedWhenCapacityZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lua")
	src := `
function driver_init(config) end
function driver_poll()
    host.emit("meter", { w = 1000 })
    host.emit("battery", { w = 0, soc = 0.0 })
    host.emit("pv", { w = -500 })
    return 1000
end
`
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	env := NewHostEnv("hybrid-no-batt", tel)
	env.BatteryCapacityWh = 0

	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if got := tel.Get("hybrid-no-batt", telemetry.DerBattery); got != nil {
		t.Errorf("battery reading should be dropped for capacity-0 driver; got %+v", got)
	}
	if got := tel.Get("hybrid-no-batt", telemetry.DerMeter); got == nil || got.RawW != 1000 {
		t.Errorf("meter still expected; got %+v", got)
	}
	if got := tel.Get("hybrid-no-batt", telemetry.DerPV); got == nil || got.RawW != -500 {
		t.Errorf("pv still expected; got %+v", got)
	}
	// Driver is alive — health success must still be recorded so the
	// watchdog doesn't flip it offline.
	h := tel.DriverHealthMut("hybrid-no-batt")
	if h == nil || h.TickCount == 0 {
		t.Errorf("expected health tick recorded; got %+v", h)
	}
}

func TestEmitBatteryPassesWhenCapacitySet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lua")
	src := `
function driver_init(config) end
function driver_poll()
    host.emit("battery", { w = -500, soc = 0.42 })
    return 1000
end
`
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	env := NewHostEnv("real-batt", tel)
	env.BatteryCapacityWh = 9600

	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()

	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	bat := tel.Get("real-batt", telemetry.DerBattery)
	if bat == nil || bat.SoC == nil || *bat.SoC != 0.42 {
		t.Errorf("battery reading should pass through; got %+v", bat)
	}
}

func TestLuaDriverMissingFile(t *testing.T) {
	env := NewHostEnv("test", telemetry.NewStore())
	_, err := NewLuaDriver("/nonexistent/path.lua", env)
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLuaDriverSyntaxError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.lua")
	os.WriteFile(path, []byte("function (x"), 0644)
	env := NewHostEnv("bad", telemetry.NewStore())
	_, err := NewLuaDriver(path, env)
	if err == nil {
		t.Error("expected parse error")
	}
}
