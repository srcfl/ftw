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

func testReadOnlyPolicy() *RuntimePolicy {
	return &RuntimePolicy{
		PackageID: "com.sourceful.driver.read-only", Version: "1.0.0",
		ArtifactSHA256: strings.Repeat("b", 64), RuntimeABI: "gopher-lua-source-v1",
		HostAPIProfile: "sourceful.host/ftw-core/v1", ReadOnly: true,
		Permissions: map[string]bool{"modbus.read": true},
	}
}

func loadReadOnlyPolicyDriver(t *testing.T, source string, env *HostEnv) *LuaDriver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "read-only.lua")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	driver, err := NewLuaDriverWithPolicy(path, env, testReadOnlyPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func TestSignedReadOnlyPolicyDeniesWritesInEveryLifecyclePhase(t *testing.T) {
	source := `
function driver_init(config) host.modbus_write(10, 1) end
function driver_poll() host.modbus_write(10, 2) return 1000 end
function driver_command(action, value, command) host.modbus_write(10, 3) end
function driver_default_mode() host.modbus_write(10, 4) end
function driver_cleanup() host.modbus_write(10, 5) end
`
	modbus := &writeCountingModbus{}
	driver := loadReadOnlyPolicyDriver(t, source, NewHostEnv("read-only", telemetry.NewStore()).WithModbus(modbus))
	if err := driver.Init(context.Background(), map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := driver.Command(context.Background(), []byte(`{"action":"test","power_w":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := driver.DefaultMode(); err != nil {
		t.Fatal(err)
	}
	driver.Cleanup()
	if got := modbus.writes.Load(); got != 0 {
		t.Fatalf("signed read-only driver reached hardware: %d writes", got)
	}
}

// The host accepts no signed policy that may write, and a policy that reaches
// a HostEnv without validation still cannot open a write path.
func TestRuntimePolicyThatMayWriteIsRefused(t *testing.T) {
	policy := testReadOnlyPolicy()
	policy.ReadOnly = false
	policy.Permissions["modbus.write"] = true
	if err := policy.validate(); err == nil {
		t.Fatal("a signed policy that is not read-only was accepted")
	}
	env := &HostEnv{RuntimePolicy: policy}
	for _, permission := range []string{"modbus.write", "mqtt.publish", "http.post", "http.patch"} {
		if err := env.allowWrite(permission); err == nil {
			t.Fatalf("%s: a managed driver was allowed to write", permission)
		}
	}
}

// host.sleep had a 100 ms limit meant for the removed control runtime, which
// also applied to signed read-only drivers. The same file now sleeps the same
// way whichever source it came from.
func TestSignedReadOnlyDriverSleepsLikeABundledOne(t *testing.T) {
	source := `
function driver_init(config)
    local err = host.sleep(150)
    host.emit_metric("sleep_ok", err == nil and 1 or 0)
end
`
	tel := telemetry.NewStore()
	driver := loadReadOnlyPolicyDriver(t, source, NewHostEnv("read-only-sleep", tel))
	defer driver.Cleanup()
	start := time.Now()
	if err := driver.Init(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("host.sleep(150) returned after %s", elapsed)
	}
	if value, _, ok := tel.LatestMetric("read-only-sleep", "sleep_ok"); !ok || value != 1 {
		t.Fatalf("host.sleep returned an error for a signed read-only driver (metric=%v present=%v)", value, ok)
	}
}
