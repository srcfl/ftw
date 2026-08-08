package drivers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// staleQueueRuntime models a legacy Lua driver_command that has crossed the
// driver boundary, may have changed hardware, and then blocks without
// watching its context. The registry must not let commands that timed out
// while waiting in cmdCh reach this runtime after recovery.
type staleQueueRuntime struct {
	env             *HostEnv
	entered         chan struct{}
	release         chan struct{}
	enteredOnce     sync.Once
	releaseOnce     sync.Once
	defaulted       chan struct{}
	defaultedOnce   sync.Once
	defaultFailures int32
	defaultAttempts atomic.Int32
	eventsMu        sync.Mutex
	events          []string
}

func (r *staleQueueRuntime) Init(ctx context.Context, configJSON []byte) error { return nil }
func (r *staleQueueRuntime) Poll(ctx context.Context) (time.Duration, error) {
	return time.Hour, nil
}
func (r *staleQueueRuntime) Command(ctx context.Context, cmdJSON []byte) error {
	payload := string(cmdJSON)
	r.eventsMu.Lock()
	r.events = append(r.events, "command:"+payload)
	r.eventsMu.Unlock()
	first := false
	r.enteredOnce.Do(func() {
		first = true
		close(r.entered)
	})
	if first {
		<-r.release
	}
	return nil
}
func (r *staleQueueRuntime) DefaultMode(ctx context.Context) error {
	r.eventsMu.Lock()
	r.events = append(r.events, "default")
	r.eventsMu.Unlock()
	attempt := r.defaultAttempts.Add(1)
	if attempt <= r.defaultFailures {
		return errors.New("simulated default failure")
	}
	r.defaultedOnce.Do(func() { close(r.defaulted) })
	return nil
}
func (r *staleQueueRuntime) Cleanup(ctx context.Context) error { return nil }
func (r *staleQueueRuntime) Env() *HostEnv                     { return r.env }

func (r *staleQueueRuntime) releaseCommand() {
	r.releaseOnce.Do(func() { close(r.release) })
}

func (r *staleQueueRuntime) eventSnapshot() []string {
	r.eventsMu.Lock()
	defer r.eventsMu.Unlock()
	return append([]string(nil), r.events...)
}

func TestRegistryDefaultBypassesStaleCommandQueue(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	rt := &staleQueueRuntime{
		env:             NewHostEnv("d1", tel),
		entered:         make(chan struct{}),
		release:         make(chan struct{}),
		defaulted:       make(chan struct{}),
		defaultFailures: 1,
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	rd := &runningDriver{
		driver:           rt,
		env:              rt.env,
		cfg:              config.Driver{Name: "d1"},
		generation:       1,
		lifecycleCtx:     lifecycleCtx,
		lifecycleCancel:  lifecycleCancel,
		defaultConfirmed: true,
		cmdCh:            make(chan driverCmd, 8),
		defaultCh:        make(chan driverCmd, 1),
		stop:             make(chan bool, 1),
		done:             make(chan struct{}),
	}
	r.mu.Lock()
	r.rec["d1"] = rd
	r.mu.Unlock()
	go r.runLoop(rd)
	t.Cleanup(func() {
		rt.releaseCommand()
		if _, ok := r.ControlStatus("d1"); ok {
			r.remove("d1", true)
		}
	})

	activeDone := make(chan error, 1)
	activeCtx, activeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer activeCancel()
	go func() {
		activeDone <- r.Send(activeCtx, "d1", []byte(`{"action":"active"}`))
	}()
	select {
	case <-rt.entered:
	case <-time.After(time.Second):
		t.Fatal("blocking command did not reach the driver")
	}

	// The blocked command leaves all eight normal queue slots available for
	// callers whose short contexts will expire before runLoop can dequeue.
	for i := 0; i < cap(rd.cmdCh); i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := r.Send(ctx, "d1", []byte(`{"action":"stale"}`))
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stale Send %d = %v, want deadline exceeded", i, err)
		}
	}
	if got := len(rd.cmdCh); got != cap(rd.cmdCh) {
		t.Fatalf("normal queue length = %d, want %d", got, cap(rd.cmdCh))
	}

	// This is the watchdog failure from the merged P1: the caller expires
	// while the active legacy command still owns runLoop. The request must be
	// accepted outside cmdCh and remain durable after this call returns.
	defaultCtx, defaultCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defaultErr := r.SendDefault(defaultCtx, "d1")
	defaultCancel()
	if !errors.Is(defaultErr, context.DeadlineExceeded) {
		t.Fatalf("SendDefault = %v, want caller deadline while command is blocked", defaultErr)
	}
	status, ok := r.ControlStatus("d1")
	if !ok || !status.Blocked || !status.RecoveryPending || status.DefaultConfirmed {
		t.Fatalf("status after accepted default = %+v, running=%v", status, ok)
	}

	rt.releaseCommand()
	select {
	case err := <-activeDone:
		if err != nil {
			t.Fatalf("active Send = %v, want success after release", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocking command did not finish after release")
	}
	select {
	case <-rt.defaulted:
	case <-time.After(2 * time.Second):
		t.Fatal("accepted default was not retried to success")
	}
	if attempts := rt.defaultAttempts.Load(); attempts < 2 {
		t.Fatalf("default attempts = %d, want failed attempt plus recovery retry", attempts)
	}

	status, ok = r.ControlStatus("d1")
	if !ok || status.Blocked || !status.DefaultConfirmed || status.RecoveryPending {
		t.Fatalf("status after default recovery = %+v, running=%v", status, ok)
	}
	for _, event := range rt.eventSnapshot() {
		if strings.HasPrefix(event, "command:") && !strings.HasSuffix(event, `{"action":"active"}`) {
			t.Fatalf("stale command crossed driver boundary: events=%v", rt.eventSnapshot())
		}
	}
}
