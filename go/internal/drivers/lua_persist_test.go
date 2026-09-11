package drivers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestManagedPersistSecretScope(t *testing.T) {
	for _, tc := range []struct {
		name, key        string
		valueBytes       int
		change           func(*RuntimePolicy)
		unwired, allowed bool
	}{
		{name: "declared", key: "refresh_token", allowed: true},
		{name: "undeclared", key: "other_token"},
		{name: "other_namespace", key: "other:refresh_token"},
		{name: "listed_colon", key: "other:refresh_token", change: func(p *RuntimePolicy) { p.ConfigSecrets = []string{"other:refresh_token"} }},
		{name: "listed_path", key: "../refresh_token", change: func(p *RuntimePolicy) { p.ConfigSecrets = []string{"../refresh_token"} }},
		{name: "listed_long_key", key: strings.Repeat("a", 65), change: func(p *RuntimePolicy) { p.ConfigSecrets = []string{strings.Repeat("a", 65)} }},
		{name: "no_auth_path", key: "refresh_token", change: func(p *RuntimePolicy) { p.AuthPostPath = "" }},
		{name: "no_http_get", key: "refresh_token", change: func(p *RuntimePolicy) { p.Permissions = nil }},
		{name: "not_read_only", key: "refresh_token", change: func(p *RuntimePolicy) { p.ReadOnly = false }},
		{name: "broad_http_write", key: "refresh_token", change: func(p *RuntimePolicy) { p.Permissions["http.patch"] = true }},
		{name: "unwired", key: "refresh_token", unwired: true},
		{name: "at_value_limit", key: "refresh_token", valueBytes: 1 << 20, allowed: true},
		{name: "above_value_limit", key: "refresh_token", valueBytes: (1 << 20) + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := &RuntimePolicy{
				PackageID: "com.sourceful.driver.myuplink", Version: "1.2.2",
				ArtifactSHA256: strings.Repeat("a", 64), RuntimeABI: "gopher-lua-source-v1",
				HostAPIProfile: "sourceful.host/ftw-core/v1", ReadOnly: true,
				Permissions: map[string]bool{"http.get": true, "http.post": true}, AuthPostPath: "/oauth/token",
				ConfigSecrets: []string{"refresh_token"},
			}
			if tc.change != nil {
				tc.change(policy)
			}
			tel := telemetry.NewStore()
			env := NewHostEnv("managed", tel)
			calls := 0
			if !tc.unwired {
				env.PersistSecret = func(key, value string) error {
					calls++
					if key != tc.key || len(value) != tc.valueBytes {
						t.Error("callback arguments changed")
					}
					return nil
				}
			}
			path := filepath.Join(t.TempDir(), "managed.lua")
			source := fmt.Sprintf(`function driver_init()
local ok, err = host.persist_secret(%q, string.rep("x", %d))
host.emit_metric("persist_ok", ok and 1 or 0)
if not ok and not err then error("missing denial reason") end
end`, tc.key, tc.valueBytes)
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			d, err := NewLuaDriverWithPolicy(path, env, policy)
			if err != nil {
				if tc.name == "not_read_only" || tc.name == "broad_http_write" {
					return
				}
				if tc.name == "no_auth_path" || tc.name == "no_http_get" {
					return
				}
				t.Fatal(err)
			}
			defer d.Cleanup()
			if err := d.Init(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.allowed {
				want = 1
			}
			if calls != want {
				t.Fatalf("persistence calls = %d, want %d", calls, want)
			}
			if value, _, ok := tel.LatestMetric("managed", "persist_ok"); !ok || value != float64(want) {
				t.Fatalf("persist result = %v, present=%v, want %d", value, ok, want)
			}
		})
	}
}

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
