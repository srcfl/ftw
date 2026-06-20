package drivers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// TestHostPersistSecret verifies a driver can durably write a config
// secret (e.g. a rotated OAuth refresh_token) back through the
// host.persist_secret capability, and that the (key, value) pair reaches
// the injected HostEnv.PersistSecret hook unchanged.
func TestHostPersistSecret(t *testing.T) {
	tel := telemetry.NewStore()
	var gotKey, gotVal string
	var called int
	env := NewHostEnv("dummy", tel)
	env.PersistSecret = func(key, value string) error {
		called++
		gotKey, gotVal = key, value
		return nil
	}

	src := `
		function driver_init() end
		function driver_poll()
			local ok, err = host.persist_secret("refresh_token", "RT2")
			if ok then host.emit_metric("persist_ok", 1) end
			return 60000
		end
		function driver_command() end
		function driver_default_mode() end
		function driver_cleanup() end
	`
	path := filepath.Join(t.TempDir(), "drv.lua")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if called != 1 {
		t.Fatalf("PersistSecret called %d times, want 1", called)
	}
	if gotKey != "refresh_token" || gotVal != "RT2" {
		t.Errorf("PersistSecret got (%q, %q), want (refresh_token, RT2)", gotKey, gotVal)
	}
	if v, _, ok := tel.LatestMetric("dummy", "persist_ok"); !ok || v != 1 {
		t.Errorf("persist_ok = %v (ok=%v), want 1 — host.persist_secret should return ok=true", v, ok)
	}
}

// TestHostPersistSecretNotGranted verifies that without a PersistSecret
// hook the primitive returns ok=false + an error string rather than
// panicking, so a driver can degrade gracefully.
func TestHostPersistSecretNotGranted(t *testing.T) {
	tel := telemetry.NewStore()
	env := NewHostEnv("dummy", tel) // no PersistSecret wired

	src := `
		function driver_init() end
		function driver_poll()
			local ok, err = host.persist_secret("k", "v")
			if not ok and err then host.emit_metric("got_err", 1) end
			return 60000
		end
		function driver_command() end
		function driver_default_mode() end
		function driver_cleanup() end
	`
	path := filepath.Join(t.TempDir(), "drv.lua")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if v, _, ok := tel.LatestMetric("dummy", "got_err"); !ok || v != 1 {
		t.Errorf("got_err = %v (ok=%v), want 1 — primitive should return an error when not granted", v, ok)
	}
}
