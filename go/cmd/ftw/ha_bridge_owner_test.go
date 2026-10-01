package main

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/ha"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// ---- Real broker: the two ways a first start fails on a real site ----

// FTW boots before the Home Assistant broker after a power cut. The first
// start fails; the owner must connect once the broker is up, and HA commands
// must reach FTW without a restart or a settings save.
func TestHABridgeOwnerConnectsWhenBrokerComesUpLater(t *testing.T) {
	t.Parallel()
	port := freeTCPPort(t)
	cfg := &config.HomeAssistant{Enabled: true, Broker: "127.0.0.1", Port: port}
	modes := make(chan string, 4)
	owner := newRealHAOwner(modes)
	defer owner.Stop()

	owner.Apply(cfg) // broker down: fails and starts the retry
	if owner.Bridge() != nil {
		t.Fatal("no bridge may be handed out while the broker is down")
	}

	mb := startTestBroker(t, port, new(auth.AllowHook))
	waitConnected(t, owner)
	sendModeUntilSeen(t, mb, modes)

	owner.Stop()
	closeTestBroker(t, mb)
}

// The broker is up but refuses the first login (its user list is not loaded
// yet), then accepts the same login. Reported by Vortitron on #1448: nothing
// in FTW's settings changed, so FTW never tried again.
func TestHABridgeOwnerConnectsWhenBrokerLaterAcceptsLogin(t *testing.T) {
	t.Parallel()
	port := freeTCPPort(t)
	cfg := &config.HomeAssistant{Enabled: true, Broker: "127.0.0.1", Port: port, Username: "ha", Password: "pw"}
	login := &switchableAuth{}
	mb := startTestBroker(t, port, login)
	modes := make(chan string, 4)
	owner := newRealHAOwner(modes)
	defer owner.Stop()

	owner.Apply(cfg)
	if login.refused.Load() == 0 {
		t.Fatal("the broker never saw the refused login")
	}
	if owner.Bridge() != nil {
		t.Fatal("no bridge may be handed out while the broker refuses the login")
	}

	login.allow.Store(true)
	waitConnected(t, owner)
	sendModeUntilSeen(t, mb, modes)

	owner.Stop()
	closeTestBroker(t, mb)
}

func newRealHAOwner(modes chan<- string) *haBridgeOwner {
	tel := telemetry.NewStore()
	ctrl := control.NewState(0, 50, "")
	ctrlMu := &sync.Mutex{}
	cb := ha.CommandCallbacks{SetMode: func(m string) error {
		select {
		case modes <- m:
		default:
		}
		return nil
	}}
	o := newHABridgeOwner(func(c *config.HomeAssistant, names []string) (haBridgeHandle, error) {
		b, err := ha.Start(c, tel, ctrl, ctrlMu, names, cb, nil, nil)
		if err != nil {
			return nil, err
		}
		return b, nil
	}, func() []string { return nil })
	o.minDelay, o.maxDelay = 50*time.Millisecond, 200*time.Millisecond
	return o
}

func waitConnected(t *testing.T, o *haBridgeOwner) {
	t.Helper()
	// One failed ha.Start waits out its 10 s connect timeout.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b := o.Bridge(); b != nil && b.IsConnected() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the HA bridge did not connect after the broker became available")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sendModeUntilSeen publishes a mode command until the bridge hands it to the
// callback. The bridge subscribes in its connect handler, so the first
// publishes can land before the subscription.
func sendModeUntilSeen(t *testing.T, mb *mqttserver.Server, modes <-chan string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := mb.Publish("forty-two-watts/cmd/mode", []byte("self_consumption"), false, 0); err != nil {
			t.Fatalf("publish: %v", err)
		}
		select {
		case m := <-modes:
			if m != "self_consumption" {
				t.Fatalf("mode = %q, want self_consumption", m)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("an HA mode command never reached FTW after the bridge connected")
}

type switchableAuth struct {
	mqttserver.HookBase
	allow   atomic.Bool
	refused atomic.Int32
}

func (h *switchableAuth) ID() string { return "switchable-auth" }

func (h *switchableAuth) Provides(b byte) bool {
	return b == mqttserver.OnConnectAuthenticate || b == mqttserver.OnACLCheck
}

func (h *switchableAuth) OnConnectAuthenticate(_ *mqttserver.Client, pk packets.Packet) bool {
	if h.allow.Load() && string(pk.Connect.Username) == "ha" && string(pk.Connect.Password) == "pw" {
		return true
	}
	h.refused.Add(1)
	return false
}

func (h *switchableAuth) OnACLCheck(*mqttserver.Client, string, bool) bool { return true }

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func startTestBroker(t *testing.T, port int, hook mqttserver.Hook) *mqttserver.Server {
	t.Helper()
	mb := mqttserver.New(&mqttserver.Options{InlineClient: true})
	if err := mb.AddHook(hook, nil); err != nil {
		t.Fatal(err)
	}
	tcp := listeners.NewTCP(listeners.Config{ID: "broker", Address: fmt.Sprintf("127.0.0.1:%d", port)})
	if err := mb.AddListener(tcp); err != nil {
		t.Fatal(err)
	}
	go func() { _ = mb.Serve() }()
	return mb
}

// closeTestBroker checks the stopped bridge left no client on the broker, then
// closes it. Waiting for the clients to leave also avoids the mochi-mqtt
// v2.7.9 Close deadlock described in go/test/e2e/stack_test.go.
func closeTestBroker(t *testing.T, mb *mqttserver.Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for mb.Clients.Len() > 1 && time.Now().Before(deadline) { // 1 = inline client
		time.Sleep(10 * time.Millisecond)
	}
	if n := mb.Clients.Len(); n > 1 {
		t.Errorf("broker still holds %d clients after Stop; the bridge left a client behind", n-1)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mb.Close()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("broker Close blocked for 5s")
	}
}

// ---- Fake start: lifecycle while a retry is pending ----

type fakeHABridge struct {
	cfg       *config.HomeAssistant
	reloadErr error
	mu        sync.Mutex
	stopped   bool
}

func (f *fakeHABridge) Reload(cfg *config.HomeAssistant, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = cfg
	return f.reloadErr
}

func (f *fakeHABridge) Stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
}

func (f *fakeHABridge) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// fakeHAStarter refuses every broker in down; others get a fake bridge.
type fakeHAStarter struct {
	mu      sync.Mutex
	down    map[string]bool
	calls   map[string]int
	bridges []*fakeHABridge
	gate    chan struct{} // when set, each start waits on it
}

func newFakeHAStarter(down ...string) *fakeHAStarter {
	f := &fakeHAStarter{down: map[string]bool{}, calls: map[string]int{}}
	for _, b := range down {
		f.down[b] = true
	}
	return f
}

func (f *fakeHAStarter) start(cfg *config.HomeAssistant, _ []string) (haBridgeHandle, error) {
	f.mu.Lock()
	gate := f.gate
	f.calls[cfg.Broker]++
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[cfg.Broker] {
		return nil, errors.New("connection refused")
	}
	b := &fakeHABridge{cfg: cfg}
	f.bridges = append(f.bridges, b)
	return b, nil
}

func (f *fakeHAStarter) setDown(broker string, down bool) {
	f.mu.Lock()
	f.down[broker] = down
	f.mu.Unlock()
}

func (f *fakeHAStarter) callCount(broker string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[broker]
}

func newFakeHAOwner(f *fakeHAStarter) *haBridgeOwner {
	o := newHABridgeOwner(f.start, func() []string { return nil })
	o.minDelay, o.maxDelay = 5*time.Millisecond, 20*time.Millisecond
	return o
}

func haCfg(broker string) *config.HomeAssistant {
	return &config.HomeAssistant{Enabled: true, Broker: broker, Port: 1883}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// pendingRetryDone returns the done channel of the running retry.
func pendingRetryDone(o *haBridgeOwner) chan struct{} {
	o.lifecycleMu.Lock()
	defer o.lifecycleMu.Unlock()
	return o.retryDone
}

func assertClosed(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Fatal("the retry goroutine is still running")
	}
}

func TestHABridgeOwnerStopDuringRetryEndsIt(t *testing.T) {
	f := newFakeHAStarter("a")
	o := newFakeHAOwner(f)
	o.Apply(haCfg("a"))
	waitFor(t, "the owner never retried", func() bool { return f.callCount("a") >= 3 })
	done := pendingRetryDone(o)

	o.Stop()
	assertClosed(t, done)

	// The broker comes up after Stop: nothing may start against it.
	f.setDown("a", false)
	calls := f.callCount("a")
	time.Sleep(50 * time.Millisecond)
	if got := f.callCount("a"); got != calls {
		t.Fatalf("start ran %d more times after Stop", got-calls)
	}
	if o.Bridge() != nil || len(f.bridges) != 0 {
		t.Fatal("a bridge started after Stop")
	}
	o.Apply(haCfg("a"))
	if f.callCount("a") != calls {
		t.Fatal("Apply after Stop started a bridge")
	}
}

func TestHABridgeOwnerConfigChangeDuringRetryUsesNewConfig(t *testing.T) {
	f := newFakeHAStarter("old")
	o := newFakeHAOwner(f)
	defer o.Stop()
	o.Apply(haCfg("old"))
	waitFor(t, "the owner never retried", func() bool { return f.callCount("old") >= 2 })
	done := pendingRetryDone(o)

	o.Apply(haCfg("new"))
	assertClosed(t, done)
	if len(f.bridges) != 1 || f.bridges[0].cfg.Broker != "new" {
		t.Fatalf("bridges = %+v, want one bridge on the new broker", f.bridges)
	}
	calls := f.callCount("old")
	f.setDown("old", false)
	time.Sleep(50 * time.Millisecond)
	if f.callCount("old") != calls {
		t.Fatal("the cancelled retry kept trying the old broker")
	}
}

func TestHABridgeOwnerDisableDuringRetryEndsIt(t *testing.T) {
	f := newFakeHAStarter("a")
	o := newFakeHAOwner(f)
	defer o.Stop()
	o.Apply(haCfg("a"))
	waitFor(t, "the owner never retried", func() bool { return f.callCount("a") >= 2 })
	done := pendingRetryDone(o)

	o.Apply(&config.HomeAssistant{Enabled: false, Broker: "a"})
	assertClosed(t, done)
	f.setDown("a", false)
	time.Sleep(50 * time.Millisecond)
	if o.Bridge() != nil || len(f.bridges) != 0 {
		t.Fatal("a disabled bridge still started")
	}
}

// A start already in flight when Stop runs may still succeed. Its bridge must
// be stopped, not handed over or left connected.
func TestHABridgeOwnerStopsBridgeFromStartInFlight(t *testing.T) {
	f := newFakeHAStarter("a")
	o := newFakeHAOwner(f)
	o.Apply(haCfg("a"))

	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.down["a"] = false
	f.mu.Unlock()
	calls := f.callCount("a")
	waitFor(t, "no retry reached the broker", func() bool { return f.callCount("a") > calls })

	stopped := make(chan struct{})
	go func() {
		o.Stop()
		close(stopped)
	}()
	time.Sleep(20 * time.Millisecond)
	close(gate)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return after the start in flight finished")
	}
	if len(f.bridges) != 1 || !f.bridges[0].isStopped() {
		t.Fatal("the bridge from the start in flight was not stopped")
	}
	if o.Bridge() != nil {
		t.Fatal("the owner kept a bridge after Stop")
	}
}

// A connected bridge from a retry is owned like one started at once: a
// config change reloads it and Stop stops it.
func TestHABridgeOwnerRetriedBridgeGetsReloadAndStop(t *testing.T) {
	f := newFakeHAStarter("a")
	o := newFakeHAOwner(f)
	o.Apply(haCfg("a"))
	done := pendingRetryDone(o)
	f.setDown("a", false)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the retry never finished after the broker came up")
	}
	if o.Bridge() != nil {
		t.Fatal("Bridge() must only return a real *ha.Bridge")
	}
	o.mu.Lock()
	handed := o.bridge != nil
	o.mu.Unlock()
	if !handed {
		t.Fatal("the retry finished without handing over its bridge")
	}

	o.Apply(haCfg("b"))
	b := f.bridges[0]
	if b.cfg.Broker != "b" {
		t.Fatalf("reload went to %q, want b", b.cfg.Broker)
	}
	o.Stop()
	if !b.isStopped() {
		t.Fatal("Stop left the retried bridge running")
	}
}

// Reload tears the old client down before it connects again. When the new
// connect fails, the owner drops the dead bridge and retries.
func TestHABridgeOwnerRetriesAfterFailedReload(t *testing.T) {
	f := newFakeHAStarter()
	o := newFakeHAOwner(f)
	defer o.Stop()
	o.Apply(haCfg("a"))
	first := f.bridges[0]
	first.reloadErr = errors.New("connect timeout")
	f.setDown("b", true)

	o.Apply(haCfg("b"))
	if !first.isStopped() {
		t.Fatal("the bridge whose reload failed was not stopped")
	}
	waitFor(t, "the owner never retried after a failed reload", func() bool { return f.callCount("b") >= 2 })
	f.setDown("b", false)
	waitFor(t, "the bridge never came back", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.bridge != nil
	})
}

func TestHABridgeOwnerBackoffIsCapped(t *testing.T) {
	f := newFakeHAStarter("a")
	o := newFakeHAOwner(f)
	o.minDelay, o.maxDelay = time.Millisecond, 4*time.Millisecond
	defer o.Stop()
	o.Apply(haCfg("a"))
	// Uncapped doubling from 1 ms would need about 16 s for 15 attempts.
	waitFor(t, "the backoff grew past its cap", func() bool { return f.callCount("a") >= 15 })
}
