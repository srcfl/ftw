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
				if got, ok := st.LoadConfig("driver_secret:oauth-test:refresh_token"); !ok || got != "synthetic-B" {
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
