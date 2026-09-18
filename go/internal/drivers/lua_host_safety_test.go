package drivers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

const luaSandboxSrc = `
assert(io == nil and debug == nil and package == nil)
assert(os.execute == nil and os.remove == nil and os.getenv == nil)
assert(type(os.time()) == "number")
assert(load == nil and loadfile == nil and dofile == nil and require == nil)
assert(type(string.upper) == "function" and string.upper("ab") == "AB")
assert(type(math.floor) == "function" and math.floor(3.7) == 3)
assert(type(table.concat) == "function" and table.concat({"x","y"}, ",") == "x,y")
function driver_init(config) end
function driver_poll() return 1000 end
`

func TestBundledAndSignedV1LuaVMsStripUnsafeLibraries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.lua")
	if err := os.WriteFile(path, []byte(luaSandboxSrc), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("bundled", func(t *testing.T) {
		d, err := NewLuaDriver(path, NewHostEnv("bundled-sandbox", telemetry.NewStore()))
		if err != nil {
			t.Fatalf("bundled driver load: %v", err)
		}
		d.Cleanup()
	})

	t.Run("signed_v1", func(t *testing.T) {
		policy := &RuntimePolicy{
			PackageID: "com.sourceful.driver.read-only", Version: "1.0.0",
			ArtifactSHA256: strings.Repeat("b", 64), RuntimeABI: "gopher-lua-source-v1",
			HostAPIProfile: "sourceful.host/ftw-core/v1", ReadOnly: true,
			Permissions: map[string]bool{"modbus.read": true},
		}
		d, err := NewLuaDriverWithPolicy(path, NewHostEnv("signed-v1-sandbox", telemetry.NewStore()), policy)
		if err != nil {
			t.Fatalf("signed v1 driver load: %v", err)
		}
		d.Cleanup()
	})
}

func TestSetWatchdogTimeoutIsCappedAndRejectsNegative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "watchdog.lua")
	src := `
function driver_init(config)
    local err = host.set_watchdog_timeout_s(config.secs)
    if err then
        host.emit_metric("watchdog_err", 1)
        host.log("info", tostring(err))
    else
        host.emit_metric("watchdog_ok", 1)
    end
end
function driver_poll() return 1000 end
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	load := func(t *testing.T, name string, secs int) (*HostEnv, error) {
		t.Helper()
		tel := telemetry.NewStore()
		env := NewHostEnv(name, tel)
		d, err := NewLuaDriver(path, env)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(d.Cleanup)
		err = d.Init(context.Background(), map[string]any{"secs": secs})
		return env, err
	}

	t.Run("tesla_five_minutes", func(t *testing.T) {
		env, err := load(t, "tesla", 5*60)
		if err != nil {
			t.Fatal(err)
		}
		if v, _, ok := env.Telemetry.LatestMetric("tesla", "watchdog_ok"); !ok || v != 1 {
			t.Fatal("5-minute Tesla override should be accepted")
		}
		h := env.Telemetry.DriverHealth("tesla")
		if h == nil || h.WatchdogTimeoutOverride != 5*time.Minute {
			t.Fatalf("override = %v, want 5m", h)
		}
	})

	t.Run("above_cap_is_clamped", func(t *testing.T) {
		env, err := load(t, "capped", 86400*365)
		if err != nil {
			t.Fatal(err)
		}
		if v, _, ok := env.Telemetry.LatestMetric("capped", "watchdog_ok"); !ok || v != 1 {
			t.Fatal("oversize override should be clamped, not rejected")
		}
		h := env.Telemetry.DriverHealth("capped")
		if h == nil || h.WatchdogTimeoutOverride != maxDriverWatchdogTimeout {
			t.Fatalf("override = %v, want cap %v", h, maxDriverWatchdogTimeout)
		}
	})

	t.Run("negative_is_rejected", func(t *testing.T) {
		env, err := load(t, "negative", -1)
		if err != nil {
			t.Fatal(err)
		}
		if v, _, ok := env.Telemetry.LatestMetric("negative", "watchdog_err"); !ok || v != 1 {
			t.Fatal("negative timeout should be rejected")
		}
		h := env.Telemetry.DriverHealth("negative")
		if h != nil && h.WatchdogTimeoutOverride != 0 {
			t.Fatalf("rejected timeout installed override %v", h.WatchdogTimeoutOverride)
		}
	})
}
