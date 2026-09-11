package drivers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type blockedRuntime struct {
	env          *HostEnv
	entered      chan struct{}
	release      chan struct{}
	defaulted    chan struct{}
	enteredOnce  sync.Once
	defaultOnce  sync.Once
	commandCalls atomic.Int32
}

func (r *blockedRuntime) Init(context.Context, []byte) error { return nil }
func (r *blockedRuntime) Poll(context.Context) (time.Duration, error) {
	return time.Hour, nil
}
func (r *blockedRuntime) Command(context.Context, []byte) error {
	r.commandCalls.Add(1)
	r.enteredOnce.Do(func() { close(r.entered) })
	<-r.release
	return nil
}
func (r *blockedRuntime) DefaultMode(context.Context) error {
	r.defaultOnce.Do(func() { close(r.defaulted) })
	return nil
}
func (r *blockedRuntime) Cleanup(context.Context) error { return nil }
func (r *blockedRuntime) Env() *HostEnv                 { return r.env }

func TestSendDefaultSurvivesFullStaleCommandQueue(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	runtime := &blockedRuntime{
		env:       NewHostEnv("blocked-queue", tel),
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		defaulted: make(chan struct{}),
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	rd := &runningDriver{
		driver:           runtime,
		env:              runtime.env,
		cfg:              config.Driver{Name: "blocked-queue"},
		generation:       1,
		defaultConfirmed: true,
		lifecycleCtx:     lifecycleCtx,
		lifecycleCancel:  lifecycleCancel,
		cmdCh:            make(chan driverCmd, 2),
		defaultCh:        make(chan driverCmd, 1),
		stop:             make(chan bool, 1),
		done:             make(chan struct{}),
	}
	r.rec[rd.cfg.Name] = rd
	go r.runLoop(rd)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(runtime.release) }) }
	t.Cleanup(func() {
		release()
		if _, ok := r.ControlStatus(rd.cfg.Name); ok {
			r.remove(rd.cfg.Name, true)
		}
	})

	activeDone := make(chan error, 1)
	go func() {
		activeDone <- r.Send(context.Background(), rd.cfg.Name, []byte(`{"action":"active"}`))
	}()
	select {
	case <-runtime.entered:
	case <-time.After(time.Second):
		t.Fatal("active command did not enter the runtime")
	}
	for i := 0; i < cap(rd.cmdCh); i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := r.Send(ctx, rd.cfg.Name, []byte(`{"action":"stale"}`))
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stale command %d = %v, want deadline exceeded", i, err)
		}
	}

	defaultCtx, cancelDefault := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err := r.SendDefault(defaultCtx, rd.cfg.Name)
	cancelDefault()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendDefault = %v, want caller deadline while active command is blocked", err)
	}
	status, ok := r.ControlStatus(rd.cfg.Name)
	if !ok || !status.Blocked || status.DefaultConfirmed || !status.RecoveryPending {
		t.Fatalf("status after accepted default = %+v, running=%v", status, ok)
	}

	release()
	select {
	case err := <-activeDone:
		if !errors.Is(err, ErrCommandMayHaveRun) || !errors.Is(err, context.Canceled) {
			t.Fatalf("active command = %v, want command-may-have-run plus canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active command did not return")
	}
	select {
	case <-runtime.defaulted:
	case <-time.After(time.Second):
		t.Fatal("accepted default did not run")
	}
	if calls := runtime.commandCalls.Load(); calls != 1 {
		t.Fatalf("runtime command calls = %d, want only the active command", calls)
	}
	deadline := time.Now().Add(time.Second)
	for {
		status, ok = r.ControlStatus(rd.cfg.Name)
		if ok && !status.Blocked && status.DefaultConfirmed && !status.RecoveryPending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status after default = %+v, running=%v", status, ok)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSendDefaultWaitsForMutatingHTTPBeforeRunningDefault(t *testing.T) {
	var defaultCalls atomic.Int32
	var commandInFlight atomic.Bool
	var commandApplied atomic.Bool
	var defaultBeforeCommandApplied atomic.Bool
	commandEntered := make(chan struct{})
	defaultAfterCommand := make(chan struct{})
	releaseCommand := make(chan struct{})
	var enteredOnce sync.Once
	var defaultAfterOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseCommand) }) }
	t.Cleanup(release)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/default" {
			defaultCalls.Add(1)
			if commandInFlight.Load() && !commandApplied.Load() {
				defaultBeforeCommandApplied.Store(true)
			}
			if commandApplied.Load() {
				defaultAfterOnce.Do(func() { close(defaultAfterCommand) })
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		commandInFlight.Store(true)
		enteredOnce.Do(func() { close(commandEntered) })
		<-releaseCommand
		commandApplied.Store(true)
		commandInFlight.Store(false)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	driverPath := writeTestDriver(t, `
function driver_init(config) end
function driver_poll() return 60000 end
function driver_command(action, w, cmd)
    local _, err = host.http_patch("`+srv.URL+`/command", "{}")
    return err
end
function driver_default_mode()
    local _, err = host.http_patch("`+srv.URL+`/default", "{}")
    return err
end
`)
	r := NewRegistry(telemetry.NewStore())
	cfg := config.Driver{
		Name: "blocked-http",
		Lua:  driverPath,
		Capabilities: config.Capabilities{
			HTTP: &config.HTTPCapability{AllowWrite: true},
		},
	}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release()
		r.ShutdownAll()
	})
	if got := defaultCalls.Load(); got != 1 {
		t.Fatalf("startup default calls = %d, want 1", got)
	}

	commandDone := make(chan error, 1)
	go func() {
		commandDone <- r.Send(context.Background(), cfg.Name, []byte(`{"action":"battery","power_w":1000}`))
	}()
	select {
	case <-commandEntered:
	case <-time.After(time.Second):
		t.Fatal("command did not enter host.http_patch")
	}

	defaultCtx, cancelDefault := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelDefault()
	defaultDone := make(chan error, 1)
	go func() { defaultDone <- r.SendDefault(defaultCtx, cfg.Name) }()
	select {
	case err := <-defaultDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SendDefault before command completion = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SendDefault did not honor its caller deadline")
	}
	if defaultBeforeCommandApplied.Load() {
		t.Fatal("default reached the device before the older mutating request completed")
	}
	if got := defaultCalls.Load(); got != 1 {
		t.Fatalf("default calls before command completion = %d, want only startup default", got)
	}

	release()
	select {
	case err := <-commandDone:
		if !errors.Is(err, ErrCommandMayHaveRun) || !strings.Contains(err.Error(), context.Canceled.Error()) {
			t.Fatalf("command result = %v, want command-may-have-run plus canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("command did not return after its HTTP request completed")
	}
	select {
	case <-defaultAfterCommand:
	case <-time.After(time.Second):
		t.Fatal("accepted default did not run after the mutating request completed")
	}
	deadline := time.Now().Add(time.Second)
	for defaultCalls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := defaultCalls.Load(); got < 3 {
		t.Fatalf("default calls = %d, want startup, recovery, and requested default", got)
	}
	if defaultBeforeCommandApplied.Load() {
		t.Fatal("default reached the device before the older mutating request completed")
	}
	status, ok := r.ControlStatus(cfg.Name)
	if !ok || status.Blocked || !status.DefaultConfirmed || status.RecoveryPending {
		t.Fatalf("status after confirmed default = %+v, running=%v", status, ok)
	}
}

func TestSendDefaultCancelsHungLegacyPoll(t *testing.T) {
	var defaultCalls atomic.Int32
	var pollCompleted atomic.Bool
	pollEntered := make(chan struct{})
	releasePoll := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePoll) }) }
	t.Cleanup(release)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/default":
			defaultCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		case "/poll":
			enteredOnce.Do(func() { close(pollEntered) })
			select {
			case <-releasePoll:
			case <-req.Context().Done():
			}
			if req.Context().Err() == nil {
				pollCompleted.Store(true)
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	driverPath := writeTestDriver(t, `
function driver_init()
    host.set_poll_interval(50)
end
function driver_poll()
    host.http_get("`+srv.URL+`/poll")
    return 60000
end
function driver_default_mode()
    local _, err = host.http_patch("`+srv.URL+`/default", "{}")
    return err
end
`)
	r := NewRegistry(telemetry.NewStore())
	cfg := config.Driver{
		Name: "hung-poll",
		Lua:  driverPath,
		Capabilities: config.Capabilities{
			HTTP: &config.HTTPCapability{AllowWrite: true},
		},
	}
	if err := r.Add(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		release()
		r.ShutdownAll()
	})
	if got := defaultCalls.Load(); got != 1 {
		t.Fatalf("startup default calls = %d, want 1", got)
	}

	select {
	case <-pollEntered:
	case <-time.After(time.Second):
		t.Fatal("legacy poll did not enter host.http_get")
	}

	defaultCtx, cancelDefault := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDefault()
	started := time.Now()
	if err := r.SendDefault(defaultCtx, cfg.Name); err != nil {
		t.Fatalf("SendDefault = %v, want success while poll is hung", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("SendDefault returned after %s, want poll cancellation well under 1s", elapsed)
	}
	if pollCompleted.Load() {
		t.Fatal("hung poll completed instead of being cancelled")
	}
	if got := defaultCalls.Load(); got < 2 {
		t.Fatalf("default calls = %d, want startup and the requested default", got)
	}
	status, ok := r.ControlStatus(cfg.Name)
	if !ok || status.Blocked || !status.DefaultConfirmed || status.RecoveryPending {
		t.Fatalf("status after confirmed default = %+v, running=%v", status, ok)
	}
}

func TestLuaHostSleepHonorsCommandContext(t *testing.T) {
	driverPath := writeTestDriver(t, `
function driver_command(action, w, cmd)
    host.sleep(60000)
    return true
end
`)
	driver, err := NewLuaDriver(driverPath, NewHostEnv("blocked-sleep", telemetry.NewStore()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(driver.Cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = driver.Command(ctx, []byte(`{"action":"test"}`))
	if err == nil || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("Command = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("context-canceled host.sleep returned after %s", elapsed)
	}
}
