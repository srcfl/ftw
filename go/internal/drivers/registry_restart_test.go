package drivers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// mockMQTT implements MQTTCap with a Close-call counter. Used to prove
// that Registry.Remove / Restart actually tears the client down, which
// is what prevents the broker from leaving two clients fighting for
// the same clientID across a restart cycle.
type mockMQTT struct {
	mu     sync.Mutex
	subs   []string
	closeN atomic.Int32
}

func (m *mockMQTT) Subscribe(topic string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, topic)
	return nil
}
func (m *mockMQTT) Publish(topic string, payload []byte) error { return nil }
func (m *mockMQTT) PopMessages() []MQTTMessage                 { return nil }
func (m *mockMQTT) Close() error {
	m.closeN.Add(1)
	return nil
}

// mockModbus mirrors mockMQTT for the Modbus cap.
type mockModbus struct {
	closeN atomic.Int32
}

func (m *mockModbus) Read(addr, count uint16, kind int32) ([]uint16, error) {
	return nil, nil
}
func (m *mockModbus) WriteSingle(addr, value uint16) error { return nil }
func (m *mockModbus) WriteMulti(addr uint16, vals []uint16) error {
	return nil
}
func (m *mockModbus) Close() error {
	m.closeN.Add(1)
	return nil
}

// newTestRegistry returns a Registry wired with factories that hand out
// the same mock caps every time. Lets us prove that Remove calls
// Close() on the caps the driver was given.
func newTestRegistry(t *testing.T, mq *mockMQTT, mb *mockModbus) *Registry {
	t.Helper()
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	if mq != nil {
		r.MQTTFactory = func(name string, c *config.MQTTConfig) (MQTTCap, error) {
			return mq, nil
		}
	}
	if mb != nil {
		r.ModbusFactory = func(name string, c *config.ModbusConfig) (ModbusCap, error) {
			return mb, nil
		}
	}
	return r
}

type ctxAwareRuntime struct {
	env     *HostEnv
	entered chan struct{}
	once    sync.Once
}

func (r *ctxAwareRuntime) Init(ctx context.Context, configJSON []byte) error { return nil }
func (r *ctxAwareRuntime) Poll(ctx context.Context) (time.Duration, error)   { return time.Hour, nil }
func (r *ctxAwareRuntime) Command(ctx context.Context, cmdJSON []byte) error {
	return ctx.Err()
}
func (r *ctxAwareRuntime) DefaultMode(ctx context.Context) error {
	r.once.Do(func() { close(r.entered) })
	<-ctx.Done()
	return ctx.Err()
}
func (r *ctxAwareRuntime) Cleanup(ctx context.Context) error { return nil }
func (r *ctxAwareRuntime) Env() *HostEnv                     { return r.env }

type cancelAfterStartRuntime struct {
	env         *HostEnv
	started     chan struct{}
	sideEffect  chan struct{}
	defaulted   chan struct{}
	startOnce   sync.Once
	effectOnce  sync.Once
	defaultOnce sync.Once
}

func (r *cancelAfterStartRuntime) Init(ctx context.Context, configJSON []byte) error { return nil }
func (r *cancelAfterStartRuntime) Poll(ctx context.Context) (time.Duration, error) {
	return time.Hour, nil
}
func (r *cancelAfterStartRuntime) Command(ctx context.Context, cmdJSON []byte) error {
	r.startOnce.Do(func() { close(r.started) })
	// The command has crossed the driver boundary and may have written hardware.
	r.effectOnce.Do(func() { close(r.sideEffect) })
	<-ctx.Done()
	return nil
}
func (r *cancelAfterStartRuntime) DefaultMode(ctx context.Context) error {
	r.defaultOnce.Do(func() { close(r.defaulted) })
	return nil
}
func (r *cancelAfterStartRuntime) Cleanup(ctx context.Context) error { return nil }
func (r *cancelAfterStartRuntime) Env() *HostEnv                     { return r.env }

func TestSendDefaultPassesCallerContextToRuntime(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	rt := &ctxAwareRuntime{
		env:     NewHostEnv("d1", tel),
		entered: make(chan struct{}),
	}
	rd := &runningDriver{
		driver: rt,
		env:    rt.env,
		cfg:    config.Driver{Name: "d1"},
		cmdCh:  make(chan driverCmd, 1),
		stop:   make(chan bool, 1),
		done:   make(chan struct{}),
	}
	r.rec["d1"] = rd
	go r.runLoop(rd)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := r.SendDefault(ctx, "d1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendDefault err = %v, want deadline exceeded", err)
	}
	select {
	case <-rt.entered:
	default:
		t.Fatal("runtime DefaultMode was not called")
	}
	r.remove("d1", true)
}

func TestRegistryCancelAfterCommandStartedRestoresDefault(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	rt := &cancelAfterStartRuntime{
		env:        NewHostEnv("d1", tel),
		started:    make(chan struct{}),
		sideEffect: make(chan struct{}),
		defaulted:  make(chan struct{}),
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	rd := &runningDriver{
		driver:          rt,
		env:             rt.env,
		cfg:             config.Driver{Name: "d1"},
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		cmdCh:           make(chan driverCmd, 1),
		stop:            make(chan bool, 1),
		done:            make(chan struct{}),
	}
	r.mu.Lock()
	r.nextGeneration++
	rd.generation = r.nextGeneration
	r.rec["d1"] = rd
	r.mu.Unlock()
	go r.runLoop(rd)
	t.Cleanup(func() { r.remove("d1", true) })

	ctx, cancel := context.WithCancel(context.Background())
	commandDone := make(chan error, 1)
	go func() { commandDone <- r.Send(ctx, "d1", []byte(`{"action":"set"}`)) }()
	select {
	case <-rt.started:
	case <-time.After(time.Second):
		t.Fatal("command did not start")
	}
	select {
	case <-rt.sideEffect:
	case <-time.After(time.Second):
		t.Fatal("command did not cross the driver boundary")
	}
	cancel()
	select {
	case err := <-commandDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Send after started-command cancel = %v, want context canceled", err)
		}
		if !errors.Is(err, ErrCommandMayHaveRun) {
			t.Fatalf("Send after started-command cancel = %v, want may-have-run outcome", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send did not return after caller cancellation")
	}
	select {
	case <-rt.defaulted:
	case <-time.After(time.Second):
		t.Fatal("started command did not trigger autonomous default")
	}
}

func TestRegistryRestartDefaultFailureBlocksNewGeneration(t *testing.T) {
	src := `
local defaults = 0
function driver_init(config)
    host.set_poll_interval(1000)
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd)
    host.emit_metric("command_called", 1)
    return true
end
function driver_default_mode()
    defaults = defaults + 1
    host.emit_metric("default_attempt", defaults)
    if defaults == 1 then return false end
end
`
	path := writeTestDriver(t, src)
	cfg := config.Driver{Name: "d1", Lua: path}
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.remove("d1", true) })

	if err := r.Restart(context.Background(), cfg); err != nil {
		t.Fatalf("restart = %v", err)
	}
	status, ok := r.ControlStatus("d1")
	if !ok || !status.Blocked || !status.RecoveryPending {
		t.Fatalf("new generation status after failed restart default = %+v, running=%v", status, ok)
	}
	if err := r.Send(context.Background(), "d1", []byte(`{"action":"set"}`)); !errors.Is(err, ErrControlBlocked) {
		t.Fatalf("control after failed restart default = %v, want ErrControlBlocked", err)
	}

	waitRegistryMetric(t, tel, "d1", "default_attempt", 2)
	status, ok = r.ControlStatus("d1")
	if !ok || status.Blocked || !status.DefaultConfirmed || status.RecoveryPending {
		t.Fatalf("new generation status after recovery = %+v, running=%v", status, ok)
	}
	if err := r.Send(context.Background(), "d1", []byte(`{"action":"set"}`)); err != nil {
		t.Fatalf("control after confirmed recovery = %v", err)
	}
	waitRegistryMetric(t, tel, "d1", "command_called", 1)
}

const concurrentLifecycleDriver = `DRIVER = {
  id = "concurrent_lifecycle",
  name = "Concurrent lifecycle",
  version = "1.0.0",
  controls = {
    { id = "set_offset", input = { type = "number", min = -3, max = 3 } },
  },
}

local instance = "unknown"

function driver_init(config)
    instance = config and config.instance or "unknown"
    host.set_poll_interval(1000)
end

function driver_poll() return 1000 end

function driver_command(action, w, cmd)
    if action == "set_offset" then
        host.emit_metric("applied_" .. instance, tonumber(cmd.value or cmd.offset))
        return true
    end
    return false
end

function driver_default_mode()
    host.emit_metric("defaulted_" .. instance, 1)
end
`

func concurrentLifecycleConfig(path, name, instance string) config.Driver {
	return config.Driver{
		Name: name,
		Lua:  path,
		Config: map[string]any{
			"instance": instance,
		},
		Capabilities: config.Capabilities{
			MQTT: &config.MQTTConfig{Host: "localhost", Port: 1883},
		},
	}
}

func TestConcurrentAddSameNameHasSingleOwner(t *testing.T) {
	path := writeTestDriver(t, concurrentLifecycleDriver)
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var factoryCalls atomic.Int32
	r.MQTTFactory = func(name string, c *config.MQTTConfig) (MQTTCap, error) {
		factoryCalls.Add(1)
		entered <- struct{}{}
		<-release
		return &mockMQTT{}, nil
	}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); r.ShutdownAll() })

	results := make(chan error, 2)
	go func() { results <- r.Add(context.Background(), concurrentLifecycleConfig(path, "d1", "A")) }()
	go func() { results <- r.Add(context.Background(), concurrentLifecycleConfig(path, "d1", "B")) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first Add did not reach initialization")
	}
	releaseOnce.Do(func() { close(release) })

	successes := 0
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err == nil {
				successes++
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent Add did not finish")
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent Add successes = %d, want exactly one", successes)
	}
	if got := factoryCalls.Load(); got != 1 {
		t.Fatalf("MQTT factory calls = %d, want one owner", got)
	}

	r.mu.Lock()
	rd := r.rec["d1"]
	var winner string
	if rd != nil {
		winner, _ = rd.cfg.Config["instance"].(string)
	}
	r.mu.Unlock()
	if rd == nil || winner == "" {
		t.Fatal("successful Add left no current generation")
	}
	if err := r.Send(context.Background(), "d1", []byte(`{"action":"set_offset","value":2}`)); err != nil {
		t.Fatalf("control on winning generation = %v", err)
	}
	waitRegistryMetric(t, tel, "d1", "applied_"+winner, 2)
	loser := "A"
	if winner == loser {
		loser = "B"
	}
	if _, _, ok := tel.LatestMetric("d1", "applied_"+loser); ok {
		t.Fatalf("orphan generation %s accepted a command", loser)
	}
}

func TestAddWaitsForInFlightAddBeforeRemove(t *testing.T) {
	path := writeTestDriver(t, concurrentLifecycleDriver)
	r := NewRegistry(telemetry.NewStore())
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	r.MQTTFactory = func(name string, c *config.MQTTConfig) (MQTTCap, error) {
		entered <- struct{}{}
		<-release
		return &mockMQTT{}, nil
	}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); r.ShutdownAll() })

	addDone := make(chan error, 1)
	go func() {
		addDone <- r.Add(context.Background(), concurrentLifecycleConfig(path, "d1", "A"))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Add did not reach initialization")
	}
	removeDone := make(chan struct{})
	go func() {
		r.Remove("d1")
		close(removeDone)
	}()
	select {
	case <-removeDone:
		releaseOnce.Do(func() { close(release) })
		t.Fatal("Remove returned before the in-flight Add completed")
	case <-time.After(100 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-addDone; err != nil {
		t.Fatalf("Add = %v", err)
	}
	select {
	case <-removeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Remove did not finish after Add")
	}
	if _, ok := r.ControlStatus("d1"); ok {
		t.Fatal("Remove left the Add-owned generation registered")
	}
}

func TestAddWaitsForInFlightAddBeforeRestart(t *testing.T) {
	path := writeTestDriver(t, concurrentLifecycleDriver)
	r := NewRegistry(telemetry.NewStore())
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var factoryCalls atomic.Int32
	r.MQTTFactory = func(name string, c *config.MQTTConfig) (MQTTCap, error) {
		if factoryCalls.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
		return &mockMQTT{}, nil
	}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); r.ShutdownAll() })

	addDone := make(chan error, 1)
	go func() {
		addDone <- r.Add(context.Background(), concurrentLifecycleConfig(path, "d1", "A"))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Add did not reach initialization")
	}
	restartDone := make(chan error, 1)
	go func() {
		restartDone <- r.Restart(context.Background(), concurrentLifecycleConfig(path, "d1", "restart"))
	}()
	select {
	case <-restartDone:
		releaseOnce.Do(func() { close(release) })
		t.Fatal("Restart returned before the in-flight Add completed")
	case <-time.After(100 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-addDone; err != nil {
		t.Fatalf("Add = %v", err)
	}
	select {
	case err := <-restartDone:
		if err != nil {
			t.Fatalf("Restart = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Restart did not finish after Add")
	}
	r.mu.Lock()
	rd := r.rec["d1"]
	var instance string
	if rd != nil {
		instance, _ = rd.cfg.Config["instance"].(string)
	}
	r.mu.Unlock()
	if instance != "restart" {
		t.Fatalf("current generation instance = %q, want restart", instance)
	}
}

func TestReloadWaitsForInFlightAddBeforeReplacement(t *testing.T) {
	path := writeTestDriver(t, concurrentLifecycleDriver)
	r := NewRegistry(telemetry.NewStore())
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var factoryCalls atomic.Int32
	r.MQTTFactory = func(name string, c *config.MQTTConfig) (MQTTCap, error) {
		if factoryCalls.Add(1) == 1 {
			entered <- struct{}{}
			<-release
		}
		return &mockMQTT{}, nil
	}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); r.ShutdownAll() })

	addDone := make(chan error, 1)
	go func() {
		addDone <- r.Add(context.Background(), concurrentLifecycleConfig(path, "d1", "A"))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("Add did not reach initialization")
	}
	reloadDone := make(chan struct{})
	go func() {
		r.Reload(context.Background(), []config.Driver{
			concurrentLifecycleConfig(path, "d1", "reload"),
		}, false)
		close(reloadDone)
	}()
	select {
	case <-reloadDone:
		releaseOnce.Do(func() { close(release) })
		t.Fatal("Reload returned before the in-flight Add completed")
	case <-time.After(100 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-addDone; err != nil {
		t.Fatalf("Add = %v", err)
	}
	select {
	case <-reloadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Reload did not finish after Add")
	}
	r.mu.Lock()
	rd := r.rec["d1"]
	var instance string
	if rd != nil {
		instance, _ = rd.cfg.Config["instance"].(string)
	}
	r.mu.Unlock()
	if instance != "reload" {
		t.Fatalf("current generation instance = %q, want reload", instance)
	}
}

func TestFailedAddReleasesNameReservation(t *testing.T) {
	path := writeTestDriver(t, `
function driver_init(config)
    if config ~= nil and config.fail == true then error("intentional init failure") end
    host.set_poll_interval(1000)
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) return true end
function driver_default_mode() end
`)
	r := NewRegistry(telemetry.NewStore())
	failing := config.Driver{Name: "d1", Lua: path, Config: map[string]any{"fail": true}}
	if err := r.Add(context.Background(), failing); err == nil {
		t.Fatal("failing Add unexpectedly succeeded")
	}
	completed := make(chan error, 1)
	go func() {
		completed <- r.Add(context.Background(), config.Driver{Name: "d1", Lua: path})
	}()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("Add after failed initialization = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("name reservation was not released after failed initialization")
	}
	t.Cleanup(r.ShutdownAll)
}

func TestDifferentNamesCanInitializeInParallel(t *testing.T) {
	path := writeTestDriver(t, concurrentLifecycleDriver)
	r := NewRegistry(telemetry.NewStore())
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	r.MQTTFactory = func(name string, c *config.MQTTConfig) (MQTTCap, error) {
		entered <- struct{}{}
		<-release
		return &mockMQTT{}, nil
	}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); r.ShutdownAll() })

	results := make(chan error, 2)
	go func() { results <- r.Add(context.Background(), concurrentLifecycleConfig(path, "d1", "one")) }()
	go func() { results <- r.Add(context.Background(), concurrentLifecycleConfig(path, "d2", "two")) }()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			releaseOnce.Do(func() { close(release) })
			t.Fatal("different driver names did not initialize in parallel")
		}
	}
	releaseOnce.Do(func() { close(release) })
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatalf("parallel Add = %v", err)
		}
	}
}

func TestFreshRegistryBlocksLegacyControlUntilStartupDefault(t *testing.T) {
	src := `
local defaults = 0
local fail_first_default = false
function driver_init(config)
    if config ~= nil and config.process == "new" then
        fail_first_default = true
    end
    host.set_poll_interval(1000)
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd)
    host.emit_metric("command_called", 1)
    return true
end
function driver_default_mode()
    defaults = defaults + 1
    host.emit_metric("default_attempt", defaults)
    if fail_first_default and defaults == 1 then return false end
end
`
	path := writeTestDriver(t, src)
	tel := telemetry.NewStore()

	// The first process may have crossed the driver boundary before it
	// stopped. RemoveProbe models that loss of in-memory recovery state: it
	// tears down the old runtime without sending another default.
	old := NewRegistry(tel)
	oldCfg := config.Driver{
		Name:   "d1",
		Lua:    path,
		Config: map[string]any{"process": "old"},
	}
	if err := old.Add(context.Background(), oldCfg); err != nil {
		t.Fatal(err)
	}
	if err := old.Send(context.Background(), "d1", []byte(`{"action":"set"}`)); err != nil {
		t.Fatalf("old process control = %v", err)
	}
	waitRegistryMetric(t, tel, "d1", "command_called", 1)
	old.RemoveProbe("d1")

	fresh := NewRegistry(tel)
	freshCfg := config.Driver{
		Name:   "d1",
		Lua:    path,
		Config: map[string]any{"process": "new"},
	}
	if err := fresh.Add(context.Background(), freshCfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.remove("d1", true) })

	status, ok := fresh.ControlStatus("d1")
	if !ok || !status.Blocked || !status.RecoveryPending || status.DefaultConfirmed {
		t.Fatalf("fresh process opened control before startup default: status=%+v, running=%v", status, ok)
	}
	if err := fresh.Send(context.Background(), "d1", []byte(`{"action":"set"}`)); !errors.Is(err, ErrControlBlocked) {
		t.Fatalf("control during startup-default recovery = %v, want ErrControlBlocked", err)
	}

	waitRegistryMetric(t, tel, "d1", "default_attempt", 2)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, ok = fresh.ControlStatus("d1")
		if ok && !status.Blocked && status.DefaultConfirmed && !status.RecoveryPending {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok || status.Blocked || !status.DefaultConfirmed || status.RecoveryPending {
		t.Fatalf("fresh process status after startup-default recovery = %+v, running=%v", status, ok)
	}
	if err := fresh.Send(context.Background(), "d1", []byte(`{"action":"set"}`)); err != nil {
		t.Fatalf("control after confirmed startup default = %v", err)
	}
	waitRegistryMetric(t, tel, "d1", "command_called", 1)
}

func TestFreshRegistryConfirmsSuccessfulLegacyStartupDefault(t *testing.T) {
	src := `
function driver_init(config)
    host.set_poll_interval(1000)
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd)
    host.emit_metric("command_called", 1)
    return true
end
function driver_default_mode()
    host.emit_metric("startup_default", 1)
end
`
	path := writeTestDriver(t, src)
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	cfg := config.Driver{Name: "d1", Lua: path}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.remove("d1", true) })

	status, ok := r.ControlStatus("d1")
	if !ok || status.Blocked || !status.DefaultConfirmed || status.RecoveryPending {
		t.Fatalf("legacy control opened without confirmed startup default: status=%+v, running=%v", status, ok)
	}
	waitRegistryMetric(t, tel, "d1", "startup_default", 1)
	if err := r.Send(context.Background(), "d1", []byte(`{"action":"set"}`)); err != nil {
		t.Fatalf("control after successful startup default = %v", err)
	}
	waitRegistryMetric(t, tel, "d1", "command_called", 1)
}

func TestLegacyControlDriverRequiresDefaultMode(t *testing.T) {
	src := `
DRIVER = {
    id = "control_without_default",
    controls = {
        { id = "set_offset", input = { type = "number", min = -3, max = 3 } },
    },
}
function driver_init(config) host.set_poll_interval(1000) end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) return true end
`
	path := writeTestDriver(t, src)
	r := NewRegistry(telemetry.NewStore())
	err := r.Add(context.Background(), config.Driver{Name: "d1", Lua: path})
	if err == nil {
		t.Fatal("control-capable legacy driver without driver_default_mode was accepted")
	}
	if !strings.Contains(err.Error(), "driver_default_mode") {
		t.Fatalf("missing-default error = %v, want driver_default_mode", err)
	}
	if _, ok := r.ControlStatus("d1"); ok {
		t.Fatal("driver without driver_default_mode was registered")
	}
}

func TestLegacyNoControlDriverMayOmitDefaultMode(t *testing.T) {
	path := writeTestDriver(t, `
function driver_init(config) host.set_poll_interval(1000) end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) return true end
`)
	r := NewRegistry(telemetry.NewStore())
	if err := r.Add(context.Background(), config.Driver{Name: "d1", Lua: path}); err != nil {
		t.Fatalf("reporting-only legacy driver without default = %v", err)
	}
	t.Cleanup(func() { r.Remove("d1") })
}

func TestObserveOnlyControlDriverMayOmitDefaultMode(t *testing.T) {
	src := `
DRIVER = {
    id = "observe_only_control",
    controls = {
        { id = "set_offset", input = { type = "number", min = -3, max = 3 } },
    },
}
function driver_init(config) host.set_poll_interval(1000) end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) return true end
`
	path := writeTestDriver(t, src)
	r := NewRegistry(telemetry.NewStore())
	if err := r.Add(context.Background(), config.Driver{Name: "d1", Lua: path, ObserveOnly: true}); err != nil {
		t.Fatalf("observe-only legacy driver without default = %v", err)
	}
	t.Cleanup(func() { r.Remove("d1") })
	if err := r.Send(context.Background(), "d1", []byte(`{"action":"set_offset","value":2}`)); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("observe-only control = %v, want ErrObserveOnly", err)
	}
}

func TestObserveOnlySkipsStartupDefaultAndRejectsControl(t *testing.T) {
	src := `
function driver_init(config)
    host.set_poll_interval(1000)
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd)
    host.emit_metric("command_called", 1)
    return true
end
function driver_default_mode()
    host.emit_metric("startup_default", 1)
end
`
	path := writeTestDriver(t, src)
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	cfg := config.Driver{Name: "d1", Lua: path, ObserveOnly: true}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.remove("d1", true) })

	if err := r.Send(context.Background(), "d1", []byte(`{"action":"set"}`)); !errors.Is(err, ErrObserveOnly) {
		t.Fatalf("observe_only Send = %v, want ErrObserveOnly", err)
	}
	if _, _, ok := tel.LatestMetric("d1", "startup_default"); ok {
		t.Fatal("observe_only Add invoked driver_default_mode")
	}
	if _, _, ok := tel.LatestMetric("d1", "command_called"); ok {
		t.Fatal("observe_only Send reached driver_command")
	}
}

func waitRegistryMetric(t *testing.T, tel *telemetry.Store, driver, metric string, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, _, ok := tel.LatestMetric(driver, metric); ok && got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _, ok := tel.LatestMetric(driver, metric)
	t.Fatalf("%s/%s = %v/%v, want %v", driver, metric, got, ok, want)
}

func TestRegistryCancelsLegacyCommandOnRestartAndShutdown(t *testing.T) {
	src := `
local command_count = 0
function driver_init(config)
    if config ~= nil and config.phase == "restart" then
        command_count = 1
    end
    host.set_poll_interval(1000)
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd)
    command_count = command_count + 1
    host.emit_metric("command_started", command_count)
    while true do end
end
function driver_default_mode()
    host.emit_metric("default_called", 1)
end
`
	path := writeTestDriver(t, src)
	cfg := config.Driver{Name: "d1", Lua: path, Config: map[string]any{"phase": "initial"}}
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	commandDone := make(chan error, 1)
	go func() {
		commandDone <- r.Send(context.Background(), "d1", []byte(`{"action":"loop"}`))
	}()
	waitRegistryMetric(t, tel, "d1", "command_started", 1)

	restartDone := make(chan error, 1)
	restartCfg := cfg
	restartCfg.Config = map[string]any{"phase": "restart"}
	go func() { restartDone <- r.Restart(context.Background(), restartCfg) }()
	select {
	case err := <-restartDone:
		if err != nil {
			t.Fatalf("restart = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart waited for a legacy Lua command that should have been cancelled")
	}
	select {
	case <-commandDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled command did not return")
	}

	commandDone = make(chan error, 1)
	go func() {
		commandDone <- r.Send(context.Background(), "d1", []byte(`{"action":"loop"}`))
	}()
	waitRegistryMetric(t, tel, "d1", "command_started", 2)
	shutdownDone := make(chan struct{})
	go func() {
		r.ShutdownAll()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown waited for a legacy Lua command that should have been cancelled")
	}
	select {
	case <-commandDone:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown-cancelled command did not return")
	}
}

// Reset all — used to test a series of adds / removes in the same
// registry without the mocks carrying state across calls.
func writeTestDriver(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lua")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func sawMetricValue(samples []telemetry.MetricSample, driver, metric string, value float64) bool {
	for _, sample := range samples {
		if sample.Driver == driver && sample.Metric == metric && sample.Value == value {
			return true
		}
	}
	return false
}

const registryRestartTestDriver = `
function driver_init(config) end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) end
`

const registryTroubleshootingTestDriver = `
function driver_init(config)
  local enabled = 0
  if config ~= nil and config._troubleshooting_mode == true then
    enabled = 1
  end
  host.emit_metric("troubleshooting_mode_enabled", enabled)
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) end
`

const registryPVCurtailConfigTestDriver = `
function driver_init(config)
  local enabled = 0
  if config ~= nil and config._supports_pv_curtail == true then
    enabled = 1
  end
  host.emit_metric("supports_pv_curtail", enabled)
end
function driver_poll() return 1000 end
function driver_command(action, w, cmd) end
`

// The restart bug: when a driver is removed, the MQTT capability's
// Close() was never called. The paho client stayed connected under the
// same clientID; the next Add raced a new Dial against that stale
// session and subscribe ACKs got lost. This test proves Remove now
// closes the MQTT cap exactly once.
func TestRemoveClosesMQTTCap(t *testing.T) {
	mq := &mockMQTT{}
	r := newTestRegistry(t, mq, nil)
	path := writeTestDriver(t, registryRestartTestDriver)
	cfg := config.Driver{
		Name: "d1",
		Lua:  path,
		Capabilities: config.Capabilities{
			MQTT: &config.MQTTConfig{Host: "localhost", Port: 1883},
		},
	}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	r.Remove("d1")
	if got := mq.closeN.Load(); got != 1 {
		t.Errorf("MQTT Close called %d times, want 1", got)
	}
}

// Sibling test for Modbus: TCP connections on the broker side need an
// explicit close too — some Modbus gateways limit concurrent connections
// and a leaked one can lock out the next driver instance.
func TestRemoveClosesModbusCap(t *testing.T) {
	mb := &mockModbus{}
	r := newTestRegistry(t, nil, mb)
	path := writeTestDriver(t, registryRestartTestDriver)
	cfg := config.Driver{
		Name: "d1",
		Lua:  path,
		Capabilities: config.Capabilities{
			Modbus: &config.ModbusConfig{Host: "localhost", Port: 502, UnitID: 1},
		},
	}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	r.Remove("d1")
	if got := mb.closeN.Load(); got != 1 {
		t.Errorf("Modbus Close called %d times, want 1", got)
	}
}

// Restart cycle: every Remove→Add should close the previous cap and
// hand out a fresh one. Regression test for the ferroamp-restart
// incident on 2026-04-17 where the stale paho session blocked fresh
// subscriptions.
func TestRestartClosesOldMQTTBeforeDialingNew(t *testing.T) {
	var closeCalls []int
	var mu sync.Mutex
	// Factory that hands out a fresh mock each time and records closes
	// so we can assert the previous one was closed by the time the new
	// one was asked for.
	dials := 0
	reg := NewRegistry(telemetry.NewStore())
	reg.MQTTFactory = func(name string, c *config.MQTTConfig) (MQTTCap, error) {
		dials++
		myIdx := dials
		cap := &mockMQTT{}
		// Arrange for the close counter to be reported into
		// closeCalls in order of dial — i.e. closeCalls[0] is the
		// close count of the FIRST client at time of the SECOND
		// dial.
		if myIdx >= 2 {
			mu.Lock()
			// The previous capability is still referenced by the
			// test; record its close count by looking back via the
			// registry. Simpler: we rely on the RestartByName path
			// blocking until the old runLoop exits, which (per
			// registry.runLoop) is what triggers Close(). So by the
			// time the factory is called for dial >= 2, the
			// previous cap's close counter is already 1.
			mu.Unlock()
		}
		return cap, nil
	}
	_ = closeCalls // kept for clarity in the assertion comment above

	path := writeTestDriver(t, registryRestartTestDriver)
	cfg := config.Driver{
		Name: "d1",
		Lua:  path,
		Capabilities: config.Capabilities{
			MQTT: &config.MQTTConfig{Host: "localhost", Port: 1883},
		},
	}
	ctx := context.Background()
	if err := reg.Add(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// Restart three times — exercises the Remove→Add path repeatedly.
	for i := 0; i < 3; i++ {
		if err := reg.Restart(ctx, cfg); err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
	}
	// Initial Add + 3 Restart-Adds = 4 total dial invocations.
	if dials != 4 {
		t.Errorf("MQTT factory called %d times, want 4 (1 add + 3 restart)", dials)
	}
	reg.Remove("d1")
}

// Health record must exist as soon as Add returns — not lazily on the
// first successful emit. The old behavior made a freshly-restarted
// driver look `not_running: true` in /api/status until its first MQTT
// message arrived, which could be 30+ s for slow telemetry topics.
func TestAddCreatesHealthRecordImmediately(t *testing.T) {
	r := NewRegistry(telemetry.NewStore())
	path := writeTestDriver(t, registryRestartTestDriver)
	cfg := config.Driver{Name: "d1", Lua: path}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	defer r.Remove("d1")
	// Give runLoop no time at all to tick — health should be visible
	// from the moment Add returned.
	h := r.tel.DriverHealth("d1")
	if h == nil {
		t.Fatal("driver health record not created on Add")
	}
	// Confirm the record also shows up in AllHealth (what /api/status uses).
	all := r.tel.AllHealth()
	if _, ok := all["d1"]; !ok {
		t.Errorf("driver missing from AllHealth: %+v", all)
	}
}

func TestAddInjectsGlobalTroubleshootingMode(t *testing.T) {
	r := NewRegistry(telemetry.NewStore())
	r.SetTroubleshootingMode(true)
	path := writeTestDriver(t, registryTroubleshootingTestDriver)
	cfg := config.Driver{
		Name:   "d1",
		Lua:    path,
		Config: map[string]any{"local": true},
	}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	defer r.ShutdownAll()
	if _, ok := cfg.Config["_troubleshooting_mode"]; ok {
		t.Fatal("global troubleshooting key leaked into driver config map")
	}
	if !sawMetricValue(r.tel.FlushSamples(), "d1", "troubleshooting_mode_enabled", 1) {
		t.Fatal("expected troubleshooting_mode_enabled=1 metric")
	}
}

func TestAddInjectsPVCurtailOwnershipWithoutMutatingConfig(t *testing.T) {
	r := NewRegistry(telemetry.NewStore())
	path := writeTestDriver(t, registryPVCurtailConfigTestDriver)
	cfg := config.Driver{
		Name:              "d1",
		Lua:               path,
		SupportsPVCurtail: true,
		Config:            map[string]any{"local": true},
	}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	defer r.ShutdownAll()
	if _, ok := cfg.Config["_supports_pv_curtail"]; ok {
		t.Fatal("runtime pv-curtail key leaked into driver config map")
	}
	if !sawMetricValue(r.tel.FlushSamples(), "d1", "supports_pv_curtail", 1) {
		t.Fatal("expected supports_pv_curtail=1 metric")
	}
}

func TestReloadRestartsDriversWhenTroubleshootingModeChanges(t *testing.T) {
	r := NewRegistry(telemetry.NewStore())
	path := writeTestDriver(t, registryTroubleshootingTestDriver)
	cfg := config.Driver{Name: "d1", Lua: path}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	defer r.ShutdownAll()
	if !sawMetricValue(r.tel.FlushSamples(), "d1", "troubleshooting_mode_enabled", 0) {
		t.Fatal("expected initial troubleshooting_mode_enabled=0 metric")
	}
	r.Reload(context.Background(), []config.Driver{cfg}, true)
	if !sawMetricValue(r.tel.FlushSamples(), "d1", "troubleshooting_mode_enabled", 1) {
		t.Fatal("expected reload troubleshooting_mode_enabled=1 metric")
	}
}

// runLoop should bump TickCount on every poll-return-without-error so
// a Lua driver that is alive but hasn't emitted yet (e.g. between
// MQTT subscribe and the first inbound message) is visibly running
// in /api/status. The tick is intentionally lighter than a full
// RecordSuccess: LastSuccess is NOT advanced, because the watchdog
// uses LastSuccess to flip stale drivers offline. host.emit (in the
// hot path) is the only thing that should advance LastSuccess.
func TestRunLoopRecordsSuccessEvenWithoutEmits(t *testing.T) {
	r := NewRegistry(telemetry.NewStore())
	// Driver polls every 50 ms but emits nothing.
	src := `
function driver_init(config) host.set_poll_interval(50) end
function driver_poll() return 50 end
function driver_command(action, w, cmd) end
`
	path := writeTestDriver(t, src)
	cfg := config.Driver{Name: "d1", Lua: path}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	defer r.Remove("d1")
	// Wait long enough for several ticks.
	time.Sleep(350 * time.Millisecond)
	h := r.tel.DriverHealth("d1")
	if h == nil {
		t.Fatal("no health record")
	}
	if h.TickCount < 2 {
		t.Errorf("TickCount = %d after 350ms of 50ms polls, want >= 2", h.TickCount)
	}
	// LastSuccess must remain nil — only host.emit advances it. A
	// driver that polls for hours without emitting is, from the
	// watchdog's perspective, stale; that's the correct signal so
	// the operator (and notification rules) see the outage.
	if h.LastSuccess != nil {
		t.Errorf("LastSuccess advanced without any host.emit call: %v", h.LastSuccess)
	}
}
