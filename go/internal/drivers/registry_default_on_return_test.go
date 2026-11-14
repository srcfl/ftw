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

// outageRuntime is a device that can go away. While down, polls and
// defaults fail. While refusing, defaults fail but polls succeed.
type outageRuntime struct {
	env *HostEnv

	mu       sync.Mutex
	down     bool
	refusing bool
	events   []string
}

func (r *outageRuntime) Init(context.Context, []byte) error { return nil }
func (r *outageRuntime) Poll(context.Context) (time.Duration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.down {
		r.events = append(r.events, "poll-fail")
		return 0, errors.New("modbus reconnect backoff active")
	}
	r.events = append(r.events, "poll-ok")
	return 0, nil
}
func (r *outageRuntime) Command(context.Context, []byte) error { return nil }
func (r *outageRuntime) DefaultMode(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.down || r.refusing {
		r.events = append(r.events, "default-fail")
		return errors.New("default write refused")
	}
	r.events = append(r.events, "default-ok")
	return nil
}
func (r *outageRuntime) Cleanup(context.Context) error { return nil }
func (r *outageRuntime) Discard()                      {}
func (r *outageRuntime) Env() *HostEnv                 { return r.env }

func (r *outageRuntime) set(down, refusing bool) {
	r.mu.Lock()
	r.down, r.refusing = down, refusing
	r.mu.Unlock()
}

func (r *outageRuntime) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *outageRuntime) count(event string) int {
	n := 0
	for _, e := range r.snapshot() {
		if e == event {
			n++
		}
	}
	return n
}

// waitFor polls until the runtime has recorded n events of a kind.
func (r *outageRuntime) waitFor(t *testing.T, event string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for r.count(event) < n {
		if time.Now().After(deadline) {
			t.Fatalf("waited for %d %q, events = %v", n, event, r.snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

// startOutageDriver runs a driver that polls every 10 ms and whose own
// default-recovery timer never fires within the test, so only the poll
// path can send a default.
func startOutageDriver(t *testing.T) (*Registry, *runningDriver, *outageRuntime) {
	t.Helper()
	tel := telemetry.NewStore()
	r := NewRegistry(tel)
	runtime := &outageRuntime{env: NewHostEnv("outage", tel)}
	runtime.env.PollIntervalMS = 10
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	rd := &runningDriver{
		driver:           runtime,
		env:              runtime.env,
		cfg:              config.Driver{Name: "outage"},
		generation:       1,
		defaultConfirmed: true,
		lifecycleCtx:     lifecycleCtx,
		lifecycleCancel:  lifecycleCancel,
		cmdCh:            make(chan driverCmd, 8),
		defaultCh:        make(chan driverCmd, 1),
		stop:             make(chan bool, 1),
		done:             make(chan struct{}),
		retryInitial:     time.Hour,
		retryMax:         time.Hour,
	}
	r.rec[rd.cfg.Name] = rd
	go r.runLoop(rd)
	t.Cleanup(func() { r.remove(rd.cfg.Name, true) })
	return r, rd, runtime
}

// failDefaultDuringOutage takes the device away and has the watchdog's
// default fail against it, leaving recovery pending.
func failDefaultDuringOutage(t *testing.T, r *Registry, runtime *outageRuntime) {
	t.Helper()
	runtime.set(true, false)
	runtime.waitFor(t, "poll-fail", runtime.count("poll-fail")+1)
	if err := r.SendDefault(context.Background(), "outage"); err == nil {
		t.Fatal("SendDefault during outage succeeded, want refusal")
	}
	status, _ := r.ControlStatus("outage")
	if !status.Blocked || !status.RecoveryPending || status.DefaultConfirmed {
		t.Fatalf("status during outage = %+v, want blocked with recovery pending", status)
	}
	runtime.waitFor(t, "poll-fail", runtime.count("poll-fail")+2)
}

// eventAfterFirstGoodPoll returns the event that followed the first
// poll-ok recorded after index from.
func eventAfterFirstGoodPoll(events []string, from int) string {
	for i := from; i < len(events)-1; i++ {
		if events[i] == "poll-ok" {
			return events[i+1]
		}
	}
	return ""
}

func TestDefaultSentOnFirstGoodPollAfterOutage(t *testing.T) {
	r, _, runtime := startOutageDriver(t)
	failDefaultDuringOutage(t, r, runtime)
	returnedAt := len(runtime.snapshot())

	runtime.set(false, false)
	runtime.waitFor(t, "default-ok", 1)
	if got := eventAfterFirstGoodPoll(runtime.snapshot(), returnedAt); got != "default-ok" {
		t.Fatalf("event after first good poll = %q, want default-ok; events = %v", got, runtime.snapshot())
	}
	okPolls := runtime.count("poll-ok")
	runtime.waitFor(t, "poll-ok", okPolls+5)
	if n := runtime.count("default-ok") + runtime.count("default-fail"); n != 2 {
		t.Fatalf("default calls = %d, want the failed one and one on return; events = %v", n, runtime.snapshot())
	}
	status, _ := r.ControlStatus("outage")
	if status.Blocked || !status.DefaultConfirmed || status.RecoveryPending {
		t.Fatalf("status after return = %+v, want default confirmed", status)
	}
}

func TestDefaultRetriedOncePerReturnNotPerPoll(t *testing.T) {
	r, _, runtime := startOutageDriver(t)
	failDefaultDuringOutage(t, r, runtime)

	// The device reads again but still refuses the default: one retry on
	// the return, then the backoff timer owns it.
	runtime.set(false, true)
	runtime.waitFor(t, "default-fail", 2)
	okPolls := runtime.count("poll-ok")
	runtime.waitFor(t, "poll-ok", okPolls+10)
	if n := runtime.count("default-fail"); n != 2 {
		t.Fatalf("failed defaults = %d, want one during the outage and one on return; events = %v", n, runtime.snapshot())
	}
	status, _ := r.ControlStatus("outage")
	if !status.Blocked || !status.RecoveryPending {
		t.Fatalf("status while refusing = %+v, want still blocked", status)
	}

	// A second outage is a second return, so the default goes out again.
	runtime.set(true, false)
	runtime.waitFor(t, "poll-fail", runtime.count("poll-fail")+2)
	returnedAt := len(runtime.snapshot())
	runtime.set(false, false)
	runtime.waitFor(t, "default-ok", 1)
	if got := eventAfterFirstGoodPoll(runtime.snapshot(), returnedAt); got != "default-ok" {
		t.Fatalf("event after second return = %q, want default-ok; events = %v", got, runtime.snapshot())
	}
	status, _ = r.ControlStatus("outage")
	if status.Blocked || !status.DefaultConfirmed || status.RecoveryPending {
		t.Fatalf("status after second return = %+v, want default confirmed", status)
	}
}

func TestGoodPollWithoutPendingRecoverySendsNoDefault(t *testing.T) {
	_, _, runtime := startOutageDriver(t)
	runtime.set(true, false)
	runtime.waitFor(t, "poll-fail", 2)
	runtime.set(false, false)
	runtime.waitFor(t, "poll-ok", runtime.count("poll-ok")+3)
	if n := runtime.count("default-ok") + runtime.count("default-fail"); n != 0 {
		t.Fatalf("default calls = %d, want none when no default was pending; events = %v", n, runtime.snapshot())
	}
}
