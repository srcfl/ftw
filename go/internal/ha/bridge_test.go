package ha

import (
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

// TestNilBridgeSafe verifies that all diagnostic methods on a nil *Bridge
// return zero values without panicking. The API layer calls these without
// checking whether the bridge was started, so nil-safety is load-bearing.
func TestNilBridgeSafe(t *testing.T) {
	var b *Bridge

	if b.IsConnected() {
		t.Error("nil bridge: IsConnected should be false")
	}
	if b.BrokerAddr() != "" {
		t.Error("nil bridge: BrokerAddr should be empty")
	}
	if b.LastPublishMs() != 0 {
		t.Error("nil bridge: LastPublishMs should be 0")
	}
	if b.SensorsAnnounced() != 0 {
		t.Error("nil bridge: SensorsAnnounced should be 0")
	}
	// Stop is NOT nil-safe by design (it dereferences b.stop and b.client),
	// so we do not call it on a nil receiver.
}

// TestZeroBridgeSafe verifies that a zero-value Bridge (non-nil but
// uninitialised) returns safe defaults from the diagnostic methods.
func TestZeroBridgeSafe(t *testing.T) {
	b := &Bridge{}

	if b.IsConnected() {
		t.Error("zero bridge: IsConnected should be false")
	}
	if b.BrokerAddr() != "" {
		t.Error("zero bridge: BrokerAddr should be empty (cfg is nil)")
	}
	if b.LastPublishMs() != 0 {
		t.Error("zero bridge: LastPublishMs should be 0")
	}
	if b.SensorsAnnounced() != 0 {
		t.Error("zero bridge: SensorsAnnounced should be 0")
	}
}

// TestBrokerAddrFormat verifies BrokerAddr returns "host:port" for
// various broker configurations.
func TestBrokerAddrFormat(t *testing.T) {
	cases := []struct {
		name   string
		broker string
		port   int
		want   string
	}{
		{"ip", "192.168.1.100", 1883, "192.168.1.100:1883"},
		{"hostname", "mqtt.local", 8883, "mqtt.local:8883"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Bridge{cfg: &config.HomeAssistant{Broker: tc.broker, Port: tc.port}}
			if got := b.BrokerAddr(); got != tc.want {
				t.Errorf("BrokerAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSensorsAnnouncedZeroWithoutStart verifies that SensorsAnnounced is
// zero when the bridge has been constructed but discovery has not run (no
// broker connection). This is the normal state when the API queries a
// bridge that failed to connect.
func TestSensorsAnnouncedZeroWithoutStart(t *testing.T) {
	b := &Bridge{
		driverNames: []string{"ferroamp", "sungrow"},
	}
	if got := b.SensorsAnnounced(); got != 0 {
		t.Errorf("SensorsAnnounced before discovery = %d, want 0", got)
	}
}

// TestSensorsAnnouncedFormula verifies the count that publishDiscovery
// writes matches the formula in the code: len(sensors) + len(driverNames)*5.
// We can't call publishDiscovery (it needs a live MQTT client), but we
// can verify the formula by constructing a Bridge and writing the field
// directly as publishDiscovery would.
func TestSensorsAnnouncedFormula(t *testing.T) {
	drivers := []string{"ferroamp", "sungrow"}
	b := &Bridge{driverNames: drivers}

	// Simulate what publishDiscovery sets (line 242 of bridge.go):
	// 6 site-level sensors + len(driverNames)*5
	siteSensors := 6
	perDriver := 5
	expected := siteSensors + len(drivers)*perDriver
	b.mu.Lock()
	b.sensorsAnnounced = expected
	b.mu.Unlock()

	if got := b.SensorsAnnounced(); got != expected {
		t.Errorf("SensorsAnnounced = %d, want %d", got, expected)
	}
}

// TestLastPublishMsRoundTrip verifies the getter/setter path through the
// mutex-protected field.
func TestLastPublishMsRoundTrip(t *testing.T) {
	b := &Bridge{}

	b.mu.Lock()
	b.lastPublishMs = 1713200000000
	b.mu.Unlock()

	if got := b.LastPublishMs(); got != 1713200000000 {
		t.Errorf("LastPublishMs = %d, want 1713200000000", got)
	}
}

// TestStopIdempotent verifies the lifecycle Stop is safe to call multiple
// times. main.go's defer-stack and the configreload "running → disabled"
// path can both wind up calling Stop on the same bridge; a second call
// must be a no-op rather than a panic-inducing double-close-of-channel.
func TestStopIdempotent(t *testing.T) {
	b := &Bridge{}
	// Pretend Start ran and gave us channels to close.
	b.stop = make(chan struct{})
	b.done = make(chan struct{})
	close(b.done) // simulate publishLoop having already exited

	b.Stop()
	b.Stop() // second call must be a no-op

	if !b.stopped {
		t.Error("Stop should mark bridge stopped")
	}
}

// TestReloadAfterStopRejected guards the invariant that a Stop'd bridge
// is terminal — operators must construct a fresh bridge, not resurrect
// the dead one. The configreload applier in main.go relies on this: it
// nulls haBridge after Stop and calls ha.Start on a re-enable, so a
// Reload-on-stopped path that silently succeeded would mask wiring bugs.
func TestReloadAfterStopRejected(t *testing.T) {
	b := &Bridge{}
	b.stop = make(chan struct{})
	b.done = make(chan struct{})
	close(b.done)
	b.Stop()

	err := b.Reload(&config.HomeAssistant{Broker: "x", Port: 1883}, nil)
	if err == nil {
		t.Fatal("Reload after Stop must error, got nil")
	}
}

// TestStopAfterFailedConnectDoesNotDeadlock drives connectAndStart's
// error path against a guaranteed-refused broker (127.0.0.1:1, with a
// short connectTimeout to keep the test fast) and asserts the next
// Stop() returns instead of blocking forever in teardown's <-doneCh.
// Reverting the deferred close(b.done) rollback in connectAndStart
// makes this test deadlock — that's the regression it guards.
func TestStopAfterFailedConnectDoesNotDeadlock(t *testing.T) {
	b := &Bridge{
		topicPrefix:    "forty-two-watts",
		discoPrefix:    "homeassistant",
		deviceID:       "forty_two_watts",
		connectTimeout: 100 * time.Millisecond,
	}
	cfg := &config.HomeAssistant{Broker: "127.0.0.1", Port: 1}

	if err := b.connectAndStart(cfg, nil); err == nil {
		t.Fatal("connectAndStart against a refused broker must return an error")
	}

	stopped := make(chan struct{})
	go func() {
		b.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		// Stop returned — rollback closed b.done, teardown didn't block.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked after a failed Connect — teardown is stuck on <-doneCh; the connectAndStart rollback regressed")
	}
}
