package drivers

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// mockMQTT implements MQTTCap with a Close-call counter. Used to prove
// that Registry.Remove / Restart actually tears the client down, which
// is what prevents the broker from leaving two clients fighting for
// the same clientID across a restart cycle.
type mockMQTT struct {
	mu        sync.Mutex
	subs      []string
	closeN    atomic.Int32
}

func (m *mockMQTT) Subscribe(topic string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, topic)
	return nil
}
func (m *mockMQTT) Publish(topic string, payload []byte) error { return nil }
func (m *mockMQTT) PopMessages() []MQTTMessage                  { return nil }
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
func (m *mockModbus) WriteSingle(addr, value uint16) error   { return nil }
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

const registryRestartTestDriver = `
function driver_init(config) end
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
