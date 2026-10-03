package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestDriverRegistryRotatedSecretSurvivesStartup(t *testing.T) {
	for _, scenario := range []struct {
		name, phase string
		managed     bool
	}{{"init", "init", false}, {"first_poll", "first_poll", false}, {"managed_init", "init", true}, {"managed_first_poll", "first_poll", true}} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "oauth.lua")
			// Synthetic tokens only. A remains in the config document after the
			// driver rotates to B, just as it does after real OAuth consent.
			source := fmt.Sprintf(`
local token
local rotated = false
local function rotate()
  if token == "synthetic-A" and not rotated then
    local ok = host.persist_secret("refresh_token", "synthetic-B")
    host.emit_metric("persist_ok", ok and 1 or 0)
    rotated = true
  end
end
function driver_init(config)
  token = config.refresh_token
  host.emit_metric("started_with_B", token == "synthetic-B" and 1 or 0)
  host.set_poll_interval(10)
  if %q == "init" then rotate() end
end
function driver_poll() rotate() return 60000 end
function driver_command() end
function driver_default_mode() end
`, scenario.phase)
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := config.Driver{Name: "oauth-test", Lua: path, Config: map[string]any{"refresh_token": "synthetic-A"}}
			dbPath := filepath.Join(dir, "state.db")
			start := func() (*state.Store, *telemetry.Store, func()) {
				t.Helper()
				st, err := state.Open(dbPath)
				if err != nil {
					t.Fatal(err)
				}
				tel := telemetry.NewStore()
				reg := newDriverRegistry(tel, st)
				if scenario.managed {
					reg.RuntimePolicyResolver = func(config.Driver) (*drivers.RuntimePolicy, error) {
						return &drivers.RuntimePolicy{
							PackageID: "com.sourceful.driver.myuplink", Version: "1.2.2",
							ArtifactSHA256: fmt.Sprintf("%064x", 1), RuntimeABI: "gopher-lua-source-v1",
							HostAPIProfile: "sourceful.host/ftw-core/v1", ReadOnly: true,
							Permissions: map[string]bool{"http.get": true, "http.post": true}, AuthPostPath: "/oauth/token",
							ConfigSecrets: []string{"refresh_token"},
						}, nil
					}
				}
				stop := func() { reg.ShutdownAll(); st.Close() }
				if err := reg.Add(context.Background(), cfg); err != nil {
					stop()
					t.Fatal(err)
				}
				return st, tel, stop
			}
			metric := func(tel *telemetry.Store, key string) float64 {
				t.Helper()
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					if value, _, ok := tel.LatestMetric(cfg.Name, key); ok {
						return value
					}
					time.Sleep(time.Millisecond)
				}
				t.Fatalf("driver did not emit %s", key)
				return 0
			}
			st, tel, stop := start()
			func() {
				defer stop()
				if got := metric(tel, "started_with_B"); got != 0 {
					t.Errorf("first start did not use config token A")
				}
				if got := metric(tel, "persist_ok"); got != 1 {
					t.Errorf("secret persistence during %s failed", scenario.phase)
				}
			if got, ok := st.LoadConfig(config.DriverSecretStateKey(cfg.SecretOwner(), "refresh_token")); !ok || got != "synthetic-B" {
				t.Errorf("rotated token B was not stored")
			}
			}()
			_, tel, stop = start()
			defer stop()
			if got := metric(tel, "started_with_B"); got != 1 {
				t.Error("restart used stale config token A instead of persisted token B")
			}
			if cfg.Config["refresh_token"] != "synthetic-A" {
				t.Error("rotation changed the source config")
			}
		})
	}
}

func oauthProbeSource(want string) string {
	return fmt.Sprintf(`
function driver_init(config)
  host.emit_metric("started_with_target", config.refresh_token == %q and 1 or 0)
  host.emit_metric("started_with_B", config.refresh_token == "synthetic-B" and 1 or 0)
  host.set_poll_interval(10)
end
function driver_poll() return 60000 end
function driver_command() end
function driver_default_mode() end
`, want)
}

func waitDriverMetric(t *testing.T, tel *telemetry.Store, name, key string) float64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if value, _, ok := tel.LatestMetric(name, key); ok {
			return value
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("driver %s did not emit %s", name, key)
	return 0
}

func TestDriverRegistryRotatedSecretFollowsOwnerNotName(t *testing.T) {
	dir := t.TempDir()
	lua := filepath.Join(dir, "oauth.lua")
	if err := os.WriteFile(lua, []byte(oauthProbeSource("synthetic-B")), 0600); err != nil {
		t.Fatal(err)
	}
	path, database := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state.db")
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveConfig(config.DriverSecretStateKey("old-name", "refresh_token"), "synthetic-B"); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Parse([]byte(`
site:
  name: Test
fuse:
  max_amps: 16
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    capabilities:
      mqtt:
        host: 192.168.1.153
api:
  port: 8080
`), dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConfigDatabase = database
	oauth := config.Driver{
		Name: "old-name", Lua: lua, Capabilities: config.Capabilities{Standalone: true},
		Config: map[string]any{"refresh_token": "synthetic-A"},
	}
	cfg.Drivers = append(cfg.Drivers, oauth)
	if err := config.SaveStored(st, path, cfg); err != nil {
		t.Fatal(err)
	}
	owner := cfg.Drivers[len(cfg.Drivers)-1].CredentialOwner
	if owner == "" || owner == "old-name" {
		t.Fatalf("credential_owner = %q", owner)
	}

	start := func(d config.Driver) (*telemetry.Store, func()) {
		t.Helper()
		tel := telemetry.NewStore()
		reg := newDriverRegistry(tel, st)
		if err := reg.Add(context.Background(), d); err != nil {
			reg.ShutdownAll()
			t.Fatal(err)
		}
		return tel, func() { reg.ShutdownAll() }
	}

	renamed := cfg.Drivers[len(cfg.Drivers)-1]
	renamed.Name = "renamed"
	tel, stop := start(renamed)
	if got := waitDriverMetric(t, tel, "renamed", "started_with_target"); got != 1 {
		t.Error("rename discarded rotated token B")
	}
	stop()

	if err := os.WriteFile(lua, []byte(oauthProbeSource("synthetic-C")), 0600); err != nil {
		t.Fatal(err)
	}
	replaced := config.Driver{
		Name: "old-name", Lua: lua, Capabilities: config.Capabilities{Standalone: true},
		Config: map[string]any{"refresh_token": "synthetic-C"},
	}
	replacedCfg := *cfg
	replacedCfg.Drivers = append([]config.Driver(nil), cfg.Drivers[:len(cfg.Drivers)-1]...)
	replacedCfg.Drivers = append(replacedCfg.Drivers, replaced)
	replacedCfg.Revision = cfg.Revision
	if err := config.SaveStored(st, path, &replacedCfg); err != nil {
		t.Fatal(err)
	}
	got := replacedCfg.Drivers[len(replacedCfg.Drivers)-1]
	if got.CredentialOwner == owner {
		t.Fatal("reused name kept the previous credential_owner")
	}
	tel, stop = start(got)
	defer stop()
	if got := waitDriverMetric(t, tel, "old-name", "started_with_B"); got != 0 {
		t.Error("reused name received leftover rotated token B")
	}
	if got := waitDriverMetric(t, tel, "old-name", "started_with_target"); got != 1 {
		t.Error("reused name did not start with the new account token C")
	}
}
