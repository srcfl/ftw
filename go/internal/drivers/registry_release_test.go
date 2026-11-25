package drivers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// releaseHookDriver records which stop hook ran, as metrics.
const releaseHookDriver = `
function driver_init(config)
    host.set_poll_interval(1000)
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) return true end
function driver_default_mode()
    host.emit_metric("default_called", 1)
end
function driver_release()
    host.emit_metric("release_called", 1)
    return true
end
function driver_cleanup()
    host.emit_metric("cleanup_called", 1)
end
`

func addAndForgetStartup(t *testing.T, r *Registry, tel *telemetry.Store, cfg config.Driver) {
	t.Helper()
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// Add runs driver_default_mode once at startup; only the stop counts.
	tel.FlushSamples()
}

func TestOwnerRemovalReleasesTheDevice(t *testing.T) {
	path := writeTestDriver(t, releaseHookDriver)
	for _, tc := range []struct {
		name string
		next func(config.Driver) []config.Driver
	}{
		{"removed from config", func(config.Driver) []config.Driver { return nil }},
		{"disabled", func(cfg config.Driver) []config.Driver {
			cfg.Disabled = true
			return []config.Driver{cfg}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := telemetry.NewStore()
			r := NewRegistry(tel)
			t.Cleanup(r.ShutdownAll)
			cfg := config.Driver{Name: "charger", Lua: path}
			addAndForgetStartup(t, r, tel, cfg)
			r.Reload(context.Background(), tc.next(cfg), false)
			emitted := emittedHooks(tel)
			if !emitted["charger:release_called"] {
				t.Fatal("removing the driver did not hand the device back")
			}
			if emitted["charger:default_called"] {
				t.Fatal("removing the driver also ran driver_default_mode, which would undo the hand-back")
			}
			if !emitted["charger:cleanup_called"] {
				t.Fatal("driver_cleanup did not run after release")
			}
			r.mu.Lock()
			blocked := r.recoveryRequired["charger"]
			r.mu.Unlock()
			if blocked {
				t.Fatal("a successful release left the name's recovery gate closed")
			}
		})
	}
}

func TestOtherStopsKeepTheSafeDefault(t *testing.T) {
	path := writeTestDriver(t, releaseHookDriver)
	for _, tc := range []struct {
		name string
		stop func(*Registry, config.Driver) error
	}{
		{"shutdown", func(r *Registry, cfg config.Driver) error { r.ShutdownAll(); return nil }},
		{"restart", func(r *Registry, cfg config.Driver) error { return r.Restart(context.Background(), cfg) }},
		{"config change", func(r *Registry, cfg config.Driver) error {
			cfg.Config = map[string]any{"changed": true}
			r.Reload(context.Background(), []config.Driver{cfg}, false)
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := telemetry.NewStore()
			r := NewRegistry(tel)
			t.Cleanup(r.ShutdownAll)
			cfg := config.Driver{Name: "charger", Lua: path}
			addAndForgetStartup(t, r, tel, cfg)
			if err := tc.stop(r, cfg); err != nil {
				t.Fatal(err)
			}
			emitted := emittedHooks(tel)
			if emitted["charger:release_called"] {
				t.Fatal("a stop that FTW comes back from handed the device to its owner")
			}
			if !emitted["charger:default_called"] {
				t.Fatal("the stop did not run driver_default_mode")
			}
		})
	}
}

func TestRemovalWithoutReleaseUsesDefault(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	t.Cleanup(r.ShutdownAll)
	addAndForgetStartup(t, r, tel, config.Driver{Name: "d1", Lua: writeTestDriver(t, lifecycleHookDriver)})
	r.Reload(context.Background(), nil, false)
	if !emittedHooks(tel)["d1:default_called"] {
		t.Fatal("a driver without driver_release was removed without its safe default")
	}
}

func TestRefusedReleaseBlocksTheName(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	t.Cleanup(r.ShutdownAll)
	addAndForgetStartup(t, r, tel, config.Driver{Name: "charger", Lua: writeTestDriver(t, `
function driver_init(config) host.set_poll_interval(1000) end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) return true end
function driver_default_mode() return true end
function driver_release() return false end
`)})
	r.Reload(context.Background(), nil, false)
	r.mu.Lock()
	blocked := r.recoveryRequired["charger"]
	r.mu.Unlock()
	if !blocked {
		t.Fatal("a refused release cleared the name's recovery gate")
	}
}

func TestSetCommandTimeoutBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		call string
		want time.Duration
	}{
		{"default", "", commandRunTimeout},
		{"longer", "host.set_command_timeout_s(12)", 12 * time.Second},
		{"capped", "host.set_command_timeout_s(600)", maxDriverCommandTimeout},
		{"shorter keeps the floor", "host.set_command_timeout_s(1)", commandRunTimeout},
		{"negative refused", `local err = host.set_command_timeout_s(-1)
    if err == nil then error("negative timeout accepted") end`, commandRunTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := telemetry.NewStore()
			r := NewRegistry(tel)
			t.Cleanup(r.ShutdownAll)
			src := `
function driver_init(config)
    host.set_poll_interval(1000)
    ` + tc.call + `
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) return true end
function driver_default_mode() return true end
`
			if err := r.Add(context.Background(), config.Driver{Name: "d1", Lua: writeTestDriver(t, src)}); err != nil {
				t.Fatal(err)
			}
			r.mu.Lock()
			rd := r.rec["d1"]
			r.mu.Unlock()
			if got := rd.runTimeout(); got != tc.want {
				t.Fatalf("runTimeout = %s, want %s", got, tc.want)
			}
		})
	}
}

// A slow cloud command that finishes after the caller stopped waiting is not
// a failure: it must complete, and must not trip the safe default (for a
// charger, a pause at 0 A).
func TestSlowCommandFinishesWithinDeclaredTime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		declare     string
		wantDone    bool
		wantDefault bool
	}{
		{"declared", "host.set_command_timeout_s(15)", true, false},
		{"undeclared", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tel := telemetry.NewStore()
			r := NewRegistry(tel)
			t.Cleanup(r.ShutdownAll)
			src := `
function driver_init(config)
    host.set_poll_interval(60000)
    ` + tc.declare + `
end
function driver_poll() return 60000 end
function driver_command(action, w, cmd)
    host.sleep(6500)
    host.emit_metric("command_done", 1)
    return true
end
function driver_default_mode()
    host.emit_metric("default_called", 1)
    return true
end
`
			addAndForgetStartup(t, r, tel, config.Driver{Name: "cloud", Lua: writeTestDriver(t, src)})
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			err := r.Send(ctx, "cloud", []byte(`{"action":"ev_set_current","w":1380}`))
			cancel()
			if !errors.Is(err, ErrCommandMayHaveRun) {
				t.Fatalf("Send = %v, want ErrCommandMayHaveRun while the command runs on", err)
			}
			seen := map[string]bool{}
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				for k, v := range emittedHooks(tel) {
					seen[k] = seen[k] || v
				}
				if seen["cloud:command_done"] || seen["cloud:default_called"] {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			time.Sleep(300 * time.Millisecond)
			for k, v := range emittedHooks(tel) {
				seen[k] = seen[k] || v
			}
			if seen["cloud:command_done"] != tc.wantDone {
				t.Fatalf("command finished = %v, want %v", seen["cloud:command_done"], tc.wantDone)
			}
			if seen["cloud:default_called"] != tc.wantDefault {
				t.Fatalf("safe default ran = %v, want %v", seen["cloud:default_called"], tc.wantDefault)
			}
		})
	}
}
