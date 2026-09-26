package drivers

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type evCommandOwnerRuntime struct {
	env *HostEnv

	mu             sync.Mutex
	actions        []string
	blocked        chan struct{}
	once           sync.Once
	pauseEntered   chan struct{}
	pauseRelease   chan struct{}
	pauseOnce      sync.Once
	commandEntered chan struct{}
	commandRelease chan struct{}
	commandErr     error
	commandOnce    sync.Once
}

func (r *evCommandOwnerRuntime) Init(context.Context, []byte) error { return nil }
func (r *evCommandOwnerRuntime) Poll(context.Context) (time.Duration, error) {
	return time.Hour, nil
}
func (r *evCommandOwnerRuntime) Command(ctx context.Context, payload []byte) error {
	var command struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(payload, &command); err != nil {
		return err
	}
	r.mu.Lock()
	r.actions = append(r.actions, command.Action)
	r.mu.Unlock()
	if command.Action == "ev_pause" && r.pauseRelease != nil {
		r.pauseOnce.Do(func() { close(r.pauseEntered) })
		<-r.pauseRelease
	}
	if command.Action == "ev_set_current" && r.blocked != nil {
		r.once.Do(func() { close(r.blocked) })
		<-ctx.Done()
		return ctx.Err()
	}
	if command.Action == "ev_set_current" && r.commandRelease != nil {
		r.commandOnce.Do(func() { close(r.commandEntered) })
		<-r.commandRelease
		return r.commandErr
	}
	return nil
}
func (r *evCommandOwnerRuntime) DefaultMode(context.Context) error {
	r.mu.Lock()
	r.actions = append(r.actions, "default")
	r.mu.Unlock()
	return nil
}
func (r *evCommandOwnerRuntime) Cleanup(context.Context) error { return nil }
func (r *evCommandOwnerRuntime) Discard()                      {}
func (r *evCommandOwnerRuntime) Env() *HostEnv                 { return r.env }

func (r *evCommandOwnerRuntime) sent(action string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, got := range r.actions {
		if got == action {
			return true
		}
	}
	return false
}

func newEVCommandOwnerRegistry(t *testing.T, blocked bool) (*Registry, *runningDriver, *evCommandOwnerRuntime) {
	t.Helper()
	tel := telemetry.NewStore()
	tel.RecordDriverSuccess("charger")
	r := NewRegistry(tel)
	runtime := &evCommandOwnerRuntime{env: NewHostEnv("charger", tel)}
	if blocked {
		runtime.blocked = make(chan struct{})
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	rd := &runningDriver{
		driver:          runtime,
		env:             runtime.env,
		cfg:             config.Driver{Name: "charger"},
		generation:      1,
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		cmdCh:           make(chan driverCmd, 8),
		defaultCh:       make(chan driverCmd, 1),
		stop:            make(chan bool, 1),
		done:            make(chan struct{}),
	}
	r.rec["charger"] = rd
	go r.runLoop(rd)
	t.Cleanup(func() { r.remove("charger", true) })
	return r, rd, runtime
}

// A health transition and its default are a single control boundary. A
// resume admitted by an earlier pause must not put the charger back under
// control after that default has completed.
func TestDefaultBoundaryInvalidatesEarlierEVPause(t *testing.T) {
	r, rd, runtime := newEVCommandOwnerRegistry(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	runtime.pauseEntered = make(chan struct{})
	runtime.pauseRelease = make(chan struct{})

	pauseDone := make(chan error, 1)
	go func() {
		pauseDone <- r.SendEVContinuation(ctx, "charger", []byte(`{"action":"ev_pause"}`), 1)
	}()
	select {
	case <-runtime.pauseEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("pause never entered the runtime")
	}
	r.tel.DriverHealthMut("charger").SetOffline()
	defaultDone := make(chan error, 1)
	go func() { defaultDone <- r.SendDefault(ctx, "charger") }()
	deadline := time.Now().Add(2 * time.Second)
	for len(rd.defaultCh) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(rd.defaultCh) == 0 {
		close(runtime.pauseRelease)
		t.Fatal("default was not queued behind the parked pause")
	}
	close(runtime.pauseRelease)
	select {
	case err := <-pauseDone:
		if !errors.Is(err, ErrCommandMayHaveRun) || !errors.Is(err, context.Canceled) {
			t.Fatalf("pause = %v, want command-may-have-run plus canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pause did not finish")
	}
	select {
	case err := <-defaultDone:
		if err != nil {
			t.Fatalf("default: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("default did not finish")
	}
	// Prove the actor's default revision, not the outer health check, owns
	// the rejection: telemetry may recover before this stale continuation.
	r.tel.RecordDriverSuccess("charger")
	_ = r.SendEVContinuation(ctx, "charger", []byte(`{"action":"ev_resume"}`), 1)

	if runtime.sent("ev_resume") {
		t.Fatal("ev_resume ran after health closed and default completed")
	}
}

// A failed periodic dispatch restores autonomous default inside the actor.
// That default cancels the earlier pause continuation before resume.
func TestFailedEVSetCurrentInvalidatesEarlierPause(t *testing.T) {
	r, _, runtime := newEVCommandOwnerRegistry(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	runtime.commandEntered = make(chan struct{})
	runtime.commandRelease = make(chan struct{})
	runtime.commandErr = errors.New("driver_command returned false")
	close(runtime.commandRelease)
	if err := r.SendEVContinuation(ctx, "charger", []byte(`{"action":"ev_pause"}`), 1); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := r.Send(ctx, "charger", []byte(`{"action":"ev_set_current","power_w":7000}`)); err == nil {
		t.Fatal("set current unexpectedly succeeded")
	}
	_ = r.SendEVContinuation(ctx, "charger", []byte(`{"action":"ev_resume"}`), 1)
	if runtime.sent("ev_resume") {
		t.Fatal("ev_resume crossed an intervening periodic dispatch")
	}
}

// The pause dwell spans more than one normal control interval. A successful
// setpoint during that dwell must stay ordered but must not cancel resume.
func TestSuccessfulEVSetCurrentKeepsPauseContinuation(t *testing.T) {
	r, _, runtime := newEVCommandOwnerRegistry(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.SendEVContinuation(ctx, "charger", []byte(`{"action":"ev_pause"}`), 1); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := r.Send(ctx, "charger", []byte(`{"action":"ev_set_current","power_w":7000}`)); err != nil {
		t.Fatalf("set current: %v", err)
	}
	if err := r.SendEVContinuation(ctx, "charger", []byte(`{"action":"ev_resume"}`), 1); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !runtime.sent("ev_resume") {
		t.Fatal("successful intervening setpoint canceled the wallbox resume")
	}
}

// Manual commands use the public payload alone. They must not depend on the
// private continuation token used by Core's automatic pause/resume cycle.
func TestManualEVResumeRemainsStandalone(t *testing.T) {
	r, _, runtime := newEVCommandOwnerRegistry(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.tel.DriverHealthMut("charger").SetOffline()
	if err := r.Send(ctx, "charger", []byte(`{"action":"ev_resume"}`)); err != nil {
		t.Fatalf("manual resume: %v", err)
	}
	if !runtime.sent("ev_resume") {
		t.Fatal("manual ev_resume did not reach the runtime")
	}
}

// A refused setpoint restores default inside the actor, then Core publishes
// command exclusion. Both must complete before the actor considers a queued
// wake command, or ev_pause can reclaim the charger in the callback gap.
func TestCommandOutcomeClosesHealthBeforeNextEVCommand(t *testing.T) {
	r, rd, runtime := newEVCommandOwnerRegistry(t, false)
	runtime.commandEntered = make(chan struct{})
	runtime.commandRelease = make(chan struct{})
	runtime.commandErr = errors.New("driver_command returned false")

	outcomeSeen := make(chan struct{})
	commandDone := make(chan error, 1)
	go func() {
		commandDone <- r.SendWithOutcome(
			context.Background(),
			"charger",
			[]byte(`{"action":"ev_set_current","power_w":7000}`),
			func(error) {
				r.tel.SetDriverCommandFault("charger", true, "refused")
				close(outcomeSeen)
			},
		)
	}()
	select {
	case <-runtime.commandEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("setpoint never entered the runtime")
	}

	pauseDone := make(chan error, 1)
	go func() {
		pauseDone <- r.SendEVContinuation(context.Background(), "charger", []byte(`{"action":"ev_pause"}`), 1)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(rd.cmdCh) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(rd.cmdCh) == 0 {
		close(runtime.commandRelease)
		t.Fatal("pause was not queued behind the parked setpoint")
	}
	close(runtime.commandRelease)

	for name, done := range map[string]<-chan error{
		"setpoint": commandDone,
		"pause":    pauseDone,
	} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not finish", name)
		}
	}
	select {
	case <-outcomeSeen:
	default:
		t.Fatal("command outcome had not run when the actor released its next command")
	}
	if runtime.sent("ev_pause") {
		t.Fatal("queued ev_pause reached the device after outcome closed health")
	}
}

// Remove cancels the driver lifecycle before it queues the stop request. Any
// resume already waiting behind an active command must observe that canceled
// lifecycle and leave without calling the device.
func TestCanceledLifecycleRejectsQueuedEVResume(t *testing.T) {
	r, rd, runtime := newEVCommandOwnerRegistry(t, true)
	if err := r.SendEVContinuation(context.Background(), "charger", []byte(`{"action":"ev_pause"}`), 1); err != nil {
		t.Fatalf("pause: %v", err)
	}

	commandDone := make(chan error, 1)
	go func() {
		commandDone <- r.Send(context.Background(), "charger", []byte(`{"action":"ev_set_current","power_w":7000}`))
	}()
	select {
	case <-runtime.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("blocking command never entered the runtime")
	}

	resumeDone := make(chan error, 1)
	go func() {
		resumeDone <- r.SendEVContinuation(context.Background(), "charger", []byte(`{"action":"ev_resume"}`), 1)
	}()
	// Match the safety-relevant first half of Registry.removeLocked while
	// leaving the stop channel until after the queued resume is observed.
	rd.cancelLifecycle()
	rd.cancelActiveCommand()

	for name, done := range map[string]<-chan error{
		"active command": commandDone,
		"queued resume":  resumeDone,
	} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s did not leave after lifecycle cancellation", name)
		}
	}
	if runtime.sent("ev_resume") {
		t.Fatal("queued ev_resume reached the device after remove canceled the lifecycle")
	}
}
