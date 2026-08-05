package drivers

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// wedgedCommandRuntime models the driver the dispatch deadline exists
// for: driver_command enters, blocks on device I/O and ignores the
// context entirely — a Lua driver sitting in a socket read does not
// watch Go cancellation. Send must still return to its caller, because
// the caller is the control tick for every other driver on the site.
type wedgedCommandRuntime struct {
	env       *HostEnv
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
	inCommand atomic.Bool
}

func (r *wedgedCommandRuntime) Init(ctx context.Context, configJSON []byte) error { return nil }
func (r *wedgedCommandRuntime) Poll(ctx context.Context) (time.Duration, error) {
	return time.Hour, nil
}
func (r *wedgedCommandRuntime) Command(ctx context.Context, cmdJSON []byte) error {
	r.inCommand.Store(true)
	defer r.inCommand.Store(false)
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return nil
}
func (r *wedgedCommandRuntime) DefaultMode(ctx context.Context) error { return nil }
func (r *wedgedCommandRuntime) Cleanup(ctx context.Context) error     { return nil }
func (r *wedgedCommandRuntime) Env() *HostEnv                         { return r.env }

// Twin of TestSendDefaultPassesCallerContextToRuntime for the dispatch
// path: a bounded context releases the caller at its deadline while the
// driver is still inside driver_command.
func TestSendReturnsAtDeadlineWhileDriverIsWedged(t *testing.T) {
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	rt := &wedgedCommandRuntime{
		env:     NewHostEnv("d1", tel),
		entered: make(chan struct{}),
		release: make(chan struct{}),
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
	start := time.Now()
	err := r.Send(ctx, "d1", []byte(`{"action":"battery","power_w":-2000}`))
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send err = %v, want deadline exceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Send returned after %s, want release at the 20ms deadline", elapsed)
	}

	select {
	case <-rt.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("runtime Command was never called")
	}
	if !rt.inCommand.Load() {
		t.Fatal("driver left Command on its own; the test proves nothing about wedged drivers")
	}

	close(rt.release)
	r.remove("d1", true)
}
