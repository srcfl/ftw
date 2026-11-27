package drivers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestMeasurementSetupBlocksWritesInEveryLuaPhase(t *testing.T) {
	var blocked atomic.Bool
	blocked.Store(true)
	env := NewHostEnv("meter", telemetry.NewStore())
	env.ControlBlocked = blocked.Load
	env.SetupAuthPaths = []string{"/oauth/token"}
	for _, verb := range []string{"modbus.write", "mqtt.publish", "http.post", "http.patch", "http.put", "websocket.send"} {
		if err := env.allowWrite(verb); err == nil {
			t.Fatal("setup allowed", verb)
		}
	}
	if !env.allowAuthPost("https://vendor.example/oauth/token") || env.allowAuthPost("https://vendor.example/device/charge") {
		t.Fatal("setup did not confine authentication to the declared path")
	}
	blocked.Store(false)
	if err := env.allowWrite("modbus.write"); err != nil {
		t.Fatal(err)
	}
	blocked.Store(true)
	path := filepath.Join(t.TempDir(), "meter.lua")
	// A physical command hook would fail loudly if any lifecycle path ran it.
	if err := os.WriteFile(path, []byte(`
function driver_init(config) host.emit_metric("setup_started", 1) end
function driver_poll() return 100 end
function driver_command() error("COMMAND MUST NOT RUN") end
function driver_default_mode() error("DEFAULT MUST NOT RUN") end
function driver_cleanup() error("CLEANUP MUST NOT RUN") end
`), 0600); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(telemetry.NewStore())
	reg.ControlBlocked = blocked.Load
	if err := reg.Add(context.Background(), config.Driver{Name: "meter", Lua: path}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Send(context.Background(), "meter", []byte(`{"action":"battery","power_w":1000}`)); err == nil || !strings.Contains(err.Error(), "measurement") {
		t.Fatal("setup sent a hardware command", err)
	}
	if err := reg.SendDefault(context.Background(), "meter"); err == nil {
		t.Fatal("setup sent a default")
	}
	reg.Remove("meter")
}

func TestSetupStanddownHasNoCommandFields(t *testing.T) {
	for _, raw := range []string{`{"action":"ev_pause"}`, `{"action":"ev_set_current","power_w":0}`} {
		if !IsEVStanddown([]byte(raw)) {
			t.Fatal("standdown rejected", raw)
		}
	}
	for _, raw := range []string{`{"action":"ev_set_current","power_w":null}`, `{"action":"ev_set_current"}`, `{"action":"ev_set_current","power_w":1}`, `{"action":"ev_pause","power_w":1000}`, `{"action":"ev_pause","extra":true}`, `null`} {
		if IsEVStanddown([]byte(raw)) {
			t.Fatal("command passed as standdown", raw)
		}
	}
}

func TestSetupIdentityBlockKeepsSafeDefaultAndEVStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "safe.lua")
	if err := os.WriteFile(path, []byte(`
local defaults=0
function driver_init() end
function driver_poll() return 100 end
function driver_default_mode() defaults=defaults+1 end
function driver_command(action)
 if action ~= "ev_pause" then error("new dispatch must be blocked") end
 if defaults < 2 then error("safe default did not run") end
end
`), 0600); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(telemetry.NewStore())
	reg.CommandBlocked = func() bool { return true }
	if err := reg.Add(context.Background(), config.Driver{Name: "charger", Lua: path}); err != nil {
		t.Fatal(err)
	}
	defer reg.Remove("charger")
	if err := reg.Send(context.Background(), "charger", []byte(`{"action":"ev_set_current","power_w":1000}`)); err == nil {
		t.Fatal("new dispatch passed identity block")
	}
	if err := reg.SendDefault(context.Background(), "charger"); err != nil {
		t.Fatal("identity block prevented safe default", err)
	}
	if err := reg.Send(context.Background(), "charger", []byte(`{"action":"ev_pause"}`)); err != nil {
		t.Fatal("identity block prevented EV stop", err)
	}
}
