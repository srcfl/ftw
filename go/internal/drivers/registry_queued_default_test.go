package drivers

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestQueuedCommandMustNotRunAfterSafetyDefault(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	runtime := &blockedRuntime{
		env:     NewHostEnv("queued-review", tel),
		entered: make(chan struct{}), release: make(chan struct{}), defaulted: make(chan struct{}),
	}
	close(runtime.release)
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	rd := &runningDriver{
		driver: runtime, env: runtime.env, cfg: config.Driver{Name: "queued-review"},
		generation: 1, defaultConfirmed: true,
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		cmdCh: make(chan driverCmd, 2), defaultCh: make(chan driverCmd, 1),
		stop: make(chan bool, 1), done: make(chan struct{}),
	}
	r.rec[rd.cfg.Name] = rd
	var startOnce sync.Once
	start := func() { startOnce.Do(func() { go r.runLoop(rd) }) }
	t.Cleanup(func() { start(); r.remove(rd.cfg.Name, true) })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// An operator command is already queued while the actor is busy.
	commandDone := make(chan error, 1)
	go func() { commandDone <- r.Send(ctx, rd.cfg.Name, []byte(`{"action":"set_offset","value":2}`)) }()
	for len(rd.cmdCh) == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("command was not queued")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	defaultDone := make(chan error, 1)
	go func() { defaultDone <- r.SendDefault(ctx, rd.cfg.Name) }()
	for len(rd.defaultCh) == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("default was not queued")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// The actor becomes available and must reject the older control request.
	start()
	if err := <-defaultDone; err != nil {
		t.Fatalf("default failed: %v", err)
	}
	err := <-commandDone
	if calls := runtime.commandCalls.Load(); calls != 0 || !errors.Is(err, ErrControlBlocked) {
		t.Fatalf("queued command survived safety default: command calls=%d, command error=%v, want zero calls and rejection", calls, err)
	}
	if err := r.Send(ctx, rd.cfg.Name, []byte(`{"action":"set_offset","value":1}`)); err != nil {
		t.Fatalf("new command after default: %v", err)
	}
	if calls := runtime.commandCalls.Load(); calls != 1 {
		t.Fatalf("new command calls=%d, want 1", calls)
	}
}
