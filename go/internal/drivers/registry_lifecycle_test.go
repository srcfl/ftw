package drivers

import (
	"context"
	"errors"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// lifecycleHookDriver records which hand-back hooks ran. Metrics outlive
// Registry.Remove, so the test can read them after the driver is gone.
const lifecycleHookDriver = `
function driver_init(config)
    host.set_poll_interval(1000)
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) return true end
function driver_default_mode()
    host.emit_metric("default_called", 1)
end
function driver_cleanup()
    host.emit_metric("cleanup_called", 1)
end
`

func TestObserveOnlyStopNeverWritesDefaultOrCleanup(t *testing.T) {
	path := writeTestDriver(t, lifecycleHookDriver)
	for _, tc := range []struct {
		name string
		stop func(*Registry, config.Driver) error
	}{
		{"remove", func(r *Registry, cfg config.Driver) error { r.Remove(cfg.Name); return nil }},
		{"shutdown", func(r *Registry, cfg config.Driver) error { r.ShutdownAll(); return nil }},
		{"restart", func(r *Registry, cfg config.Driver) error { return r.Restart(context.Background(), cfg) }},
		{"restart by name", func(r *Registry, cfg config.Driver) error {
			return r.RestartByName(context.Background(), cfg.Name)
		}},
		{"reload", func(r *Registry, cfg config.Driver) error {
			cfg.Config = map[string]any{"changed": true}
			r.Reload(context.Background(), []config.Driver{cfg}, false)
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := telemetry.NewStore()
			r := NewRegistry(tel)
			cfg := config.Driver{Name: "vpp", Lua: path, ObserveOnly: true}
			if err := r.Add(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(r.ShutdownAll)
			if err := tc.stop(r, cfg); err != nil {
				t.Fatal(err)
			}
			for _, metric := range []string{"default_called", "cleanup_called"} {
				if _, _, ok := tel.LatestMetric("vpp", metric); ok {
					t.Fatalf("stopping an observe_only driver ran %s", metric)
				}
			}
		})
	}
}

func TestSendDefaultRefusesObserveOnly(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	cfg := config.Driver{Name: "vpp", Lua: writeTestDriver(t, lifecycleHookDriver), ObserveOnly: true}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.ShutdownAll)
	if err := r.SendDefault(context.Background(), "vpp"); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("SendDefault = %v, want ErrObserveOnly", err)
	}
	if _, _, ok := tel.LatestMetric("vpp", "default_called"); ok {
		t.Fatal("SendDefault ran driver_default_mode on an observe_only driver")
	}
}

func TestRemoveRunsDriverCleanupAfterShutdownDefault(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	if err := r.Add(context.Background(), config.Driver{Name: "d1", Lua: writeTestDriver(t, lifecycleHookDriver)}); err != nil {
		t.Fatal(err)
	}
	r.Remove("d1")
	if _, _, ok := tel.LatestMetric("d1", "cleanup_called"); !ok {
		t.Fatal("driver_cleanup did not run on Remove; its hand-back (e.g. releasing a PV curtailment) was lost")
	}
}

func TestProbeTeardownSkipsDriverCleanup(t *testing.T) {
	path := writeTestDriver(t, lifecycleHookDriver)
	failingInit := writeTestDriver(t, `
function driver_init(config) error("device unreachable") end
function driver_poll() return 1000 end
function driver_cleanup()
    host.emit_metric("cleanup_called", 1)
end
`)
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	if err := r.AddProbe(context.Background(), config.Driver{Name: "probe", Lua: path}); err != nil {
		t.Fatal(err)
	}
	r.RemoveProbe("probe")
	if err := r.AddProbe(context.Background(), config.Driver{Name: "failed-probe", Lua: failingInit}); err == nil {
		t.Fatal("probe with failing driver_init was accepted")
	}
	for _, name := range []string{"probe", "failed-probe"} {
		if _, _, ok := tel.LatestMetric(name, "cleanup_called"); ok {
			t.Fatalf("%s teardown ran driver_cleanup, which may write the device", name)
		}
	}
}

func TestFailedAddClosesCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, src string
	}{
		{"driver_init error", `
function driver_init(config) error("device unreachable") end
function driver_poll() return 1000 end
`},
		{"load error", `function driver_init(config`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mq, mb := &mockMQTT{}, &mockModbus{}
			r := newTestRegistry(t, mq, mb)
			cfg := config.Driver{
				Name: "d1",
				Lua:  writeTestDriver(t, tc.src),
				Capabilities: config.Capabilities{
					MQTT:   &config.MQTTConfig{Host: "localhost", Port: 1883},
					Modbus: &config.ModbusConfig{Host: "localhost", Port: 502, UnitID: 1},
				},
			}
			if err := r.Add(context.Background(), cfg); err == nil {
				t.Fatal("failing driver was accepted")
			}
			if got := mq.closeN.Load(); got != 1 {
				t.Errorf("MQTT Close called %d times after failed Add, want 1", got)
			}
			if got := mb.closeN.Load(); got != 1 {
				t.Errorf("Modbus Close called %d times after failed Add, want 1", got)
			}
		})
	}
}

func TestFailedCapabilityDialClosesEarlierCapabilities(t *testing.T) {
	mq := &mockMQTT{}
	r := newTestRegistry(t, mq, nil)
	r.ModbusFactory = func(string, *config.ModbusConfig) (ModbusCap, error) {
		return nil, errors.New("connection refused")
	}
	cfg := config.Driver{
		Name: "d1",
		Lua:  writeTestDriver(t, registryRestartTestDriver),
		Capabilities: config.Capabilities{
			MQTT:   &config.MQTTConfig{Host: "localhost", Port: 1883},
			Modbus: &config.ModbusConfig{Host: "localhost", Port: 502, UnitID: 1},
		},
	}
	if err := r.Add(context.Background(), cfg); err == nil {
		t.Fatal("Add succeeded without its Modbus capability")
	}
	if got := mq.closeN.Load(); got != 1 {
		t.Fatalf("MQTT Close called %d times after Modbus dial failed, want 1", got)
	}
}

func TestReplacementSurvivesCanceledCaller(t *testing.T) {
	path := writeTestDriver(t, lifecycleHookDriver)
	for _, tc := range []struct {
		name    string
		replace func(context.Context, *Registry, config.Driver) error
	}{
		{"restart", func(ctx context.Context, r *Registry, cfg config.Driver) error { return r.Restart(ctx, cfg) }},
		{"restart by name", func(ctx context.Context, r *Registry, cfg config.Driver) error {
			return r.RestartByName(ctx, cfg.Name)
		}},
		{"reload", func(ctx context.Context, r *Registry, cfg config.Driver) error {
			cfg.Config = map[string]any{"changed": true}
			r.Reload(ctx, []config.Driver{cfg}, false)
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry(telemetry.NewStore())
			cfg := config.Driver{Name: "meter", Lua: path}
			if err := r.Add(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(r.ShutdownAll)
			// The HTTP client disconnected after the old generation stopped.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := tc.replace(ctx, r, cfg); err != nil {
				t.Fatalf("replacement with canceled caller = %v", err)
			}
			status, ok := r.ControlStatus("meter")
			if !ok {
				t.Fatal("driver stayed stopped after the caller disconnected")
			}
			if status.Blocked || !status.DefaultConfirmed {
				t.Fatalf("replacement status = %+v, want confirmed startup default", status)
			}
		})
	}
}
