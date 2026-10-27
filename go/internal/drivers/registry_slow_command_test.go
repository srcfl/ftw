package drivers

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// slowOKRuntime accepts every command, but only after delay: a device that
// is slower than the caller's wait yet answers successfully.
type slowOKRuntime struct {
	env      *HostEnv
	delay    time.Duration
	commands atomic.Int32
	defaults atomic.Int32
}

func (r *slowOKRuntime) Init(ctx context.Context, configJSON []byte) error { return nil }
func (r *slowOKRuntime) Poll(ctx context.Context) (time.Duration, error) {
	return time.Hour, nil
}
func (r *slowOKRuntime) Command(ctx context.Context, cmdJSON []byte) error {
	r.commands.Add(1)
	time.Sleep(r.delay) // device I/O that ignores ctx, like a Lua socket write
	return nil
}
func (r *slowOKRuntime) DefaultMode(ctx context.Context) error {
	r.defaults.Add(1)
	return nil
}
func (r *slowOKRuntime) Cleanup(ctx context.Context) error { return nil }
func (r *slowOKRuntime) Discard()                          {}
func (r *slowOKRuntime) Env() *HostEnv                     { return r.env }

func newSlowOKRegistry(delay time.Duration) (*Registry, *slowOKRuntime) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	rt := &slowOKRuntime{env: NewHostEnv("d1", tel), delay: delay}
	rd := &runningDriver{
		driver:    rt,
		env:       rt.env,
		cfg:       config.Driver{Name: "d1"},
		cmdCh:     make(chan driverCmd, 1),
		defaultCh: make(chan driverCmd, 1),
		stop:      make(chan bool, 1),
		done:      make(chan struct{}),
	}
	r.rec["d1"] = rd
	go r.runLoop(rd)
	return r, rt
}

func TestSlowSuccessfulCommandAfterCallerDeadlineIsSuccess(t *testing.T) {
	r, rt := newSlowOKRegistry(60 * time.Millisecond)
	defer r.remove("d1", true)

	var outcome atomic.Value
	done := make(chan struct{})
	// Caller waits 30 ms (the tick's interval/2 deadline, scaled down); the
	// device accepts the command at 60 ms.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	sendErr := r.SendWithOutcome(ctx, "d1", []byte(`{"action":"battery","power_w":-2000}`), func(err error) {
		if err != nil {
			outcome.Store(err)
		}
		close(done)
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("actor never completed the command")
	}
	t.Logf("send err = %v", sendErr)
	if v := outcome.Load(); v != nil {
		t.Logf("actor outcome = %v", v.(error))
	}
	if n := rt.defaults.Load(); n != 0 || outcome.Load() != nil {
		t.Fatalf("device accepted the command, yet outcome = %v and driver_default_mode ran %d time(s)", outcome.Load(), n)
	}
}

// Repeats the tick: each round the device accepts the setpoint and is then
// returned to its autonomous default.
func TestSlowSuccessfulCommandKeepsControlEveryTick(t *testing.T) {
	r, rt := newSlowOKRegistry(40 * time.Millisecond)
	defer r.remove("d1", true)

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := r.Send(ctx, "d1", []byte(`{"action":"battery","power_w":-2000}`))
		cancel()
		t.Logf("tick %d send err = %v (may-have-run=%v)", i, err, errors.Is(err, ErrCommandMayHaveRun))
		time.Sleep(80 * time.Millisecond) // rest of the tick
	}
	t.Logf("commands=%d defaults=%d", rt.commands.Load(), rt.defaults.Load())
	if rt.defaults.Load() != 0 {
		t.Fatalf("every accepted setpoint was followed by driver_default_mode: commands=%d defaults=%d", rt.commands.Load(), rt.defaults.Load())
	}
}
