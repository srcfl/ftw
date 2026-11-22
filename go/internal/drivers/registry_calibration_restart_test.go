package drivers

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestBatteryCalibrationAcrossDriverRestart(t *testing.T) {
	t.Run("restart by name", func(t *testing.T) {
		testBatteryCalibrationAcrossReplacement(t, func(ctx context.Context, registry *Registry, cfg config.Driver) error {
			return registry.RestartByName(ctx, cfg.Name)
		})
	})
	t.Run("configuration reload", func(t *testing.T) {
		testBatteryCalibrationAcrossReplacement(t, func(ctx context.Context, registry *Registry, cfg config.Driver) error {
			cfg.Config = map[string]any{"changed": true}
			registry.Reload(ctx, []config.Driver{cfg}, false)
			return nil
		})
	})
}

func testBatteryCalibrationAcrossReplacement(t *testing.T, replace func(context.Context, *Registry, config.Driver) error) {
	t.Helper()
	const driver = `
function driver_init(config)
    host.set_poll_interval(20)
end

function driver_poll()
    host.emit_metric("calibration_test_poll", 1)
    return 20
end

function driver_command(action, w, cmd)
    if action == "calibration_active" then host.set_calibrating(true) end
    if action == "calibration_complete" then host.set_calibrating(false) end
    if action == "battery" then host.emit_metric("battery_command_called", 1) end
    return true
end

function driver_default_mode() end
`
	tel := telemetry.NewStore()
	registry := NewRegistry(tel)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	path := writeTestDriver(t, driver)
	cfg := config.Driver{Name: "battery", Lua: path}
	if err := registry.Add(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.ShutdownAll)

	waitForPoll := func(want float64) {
		t.Helper()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			value, _, ok := tel.LatestMetric(cfg.Name, "calibration_test_poll")
			health := tel.DriverHealth(cfg.Name)
			if ok && value == want && health != nil && health.IsOnline() && health.LastSuccess != nil {
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatalf("waiting for generation %.0f to poll: %v", want, ctx.Err())
			}
		}
	}
	send := func(action string) error {
		return registry.Send(ctx, cfg.Name, []byte(`{"action":"`+action+`","power_w":1000}`))
	}
	report := func(action string) {
		t.Helper()
		if err := send(action); err != nil {
			t.Fatalf("driver report %s: %v", action, err)
		}
	}
	checkBlocked := func(stage string) {
		t.Helper()
		if err := send("battery"); !errors.Is(err, ErrCommandIneligible) {
			_, _, called := tel.LatestMetric(cfg.Name, "battery_command_called")
			t.Errorf("%s: battery command returned %v, Lua received command: %t; want ErrCommandIneligible", stage, err, called)
		}
	}

	waitForPoll(1)
	report("calibration_active")
	checkBlocked("before restart")

	if err := os.WriteFile(path, []byte(driver+`
function driver_poll()
    host.emit_metric("calibration_test_poll", 2)
    return 20
end
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replace(ctx, registry, cfg); err != nil {
		t.Fatal(err)
	}
	waitForPoll(2)
	checkBlocked("new driver is polling but has not reported calibration status")

	report("calibration_active")
	checkBlocked("new driver confirms calibration")
	report("calibration_complete")
	if err := send("battery"); err != nil {
		t.Fatalf("after calibration completes: %v", err)
	}
	report("calibration_active")
	registry.Remove(cfg.Name)
	if health := tel.DriverHealth(cfg.Name); health != nil {
		t.Fatalf("removal retained driver health: %+v", health)
	}
}
