package drivers

import (
	"context"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

const loopingInitDriver = `
function driver_init(config)
    while true do end
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) return true end
function driver_default_mode() end
`

func shortDriverInitTimeout(t *testing.T) {
	t.Helper()
	old := driverInitTimeout
	driverInitTimeout = 200 * time.Millisecond
	t.Cleanup(func() { driverInitTimeout = old })
}

// A driver_init that never returns must not block Reload. Reload strips the
// caller's cancellation, so only the init bound can end the loop.
func TestLoopingDriverInitDoesNotBlockReload(t *testing.T) {
	shortDriverInitTimeout(t)
	path := writeTestDriver(t, loopingInitDriver)
	r := NewRegistry(telemetry.NewStore())
	done := make(chan struct{})
	go func() {
		r.Reload(context.Background(), []config.Driver{{Name: "loopy", Lua: path}}, false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Reload still blocked after 5 s by a looping driver_init")
	}
	if names := r.Names(); len(names) != 0 {
		t.Fatalf("driver whose init timed out is registered: %v", names)
	}
}

// Same at boot: Add runs with the process context, which is never cancelled
// while Core runs. A timed-out init is an init failure.
func TestLoopingDriverInitFailsAdd(t *testing.T) {
	shortDriverInitTimeout(t)
	path := writeTestDriver(t, loopingInitDriver)
	r := NewRegistry(telemetry.NewStore())
	done := make(chan error, 1)
	go func() { done <- r.Add(context.Background(), config.Driver{Name: "loopy", Lua: path}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Add returned nil for a driver_init that never finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Add still blocked after 5 s by a looping driver_init")
	}
	if names := r.Names(); len(names) != 0 {
		t.Fatalf("driver whose init timed out is registered: %v", names)
	}
}

// A caller deadline shorter than the init bound still ends driver_init.
func TestLoopingDriverInitHonoursCallerDeadline(t *testing.T) {
	path := writeTestDriver(t, loopingInitDriver)
	r := NewRegistry(telemetry.NewStore())
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Add(ctx, config.Driver{Name: "loopy", Lua: path}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Add returned nil after the caller's deadline")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled ctx did not stop driver_init")
	}
}
