package drivers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// fakeWS is a WSCap stub for unit tests. Tests Push() inbound frames
// (as if Tibber sent them); the driver drains them through host.ws_messages.
// Tests Sent() to assert what the driver wrote (connection_init, subscribe,
// ping responses, etc.). No real network involved.
type fakeWS struct {
	mu     sync.Mutex
	open   bool
	queue  []string
	sent   []string
	dials  []string
}

func (f *fakeWS) Open(url string, headers map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open = true
	f.dials = append(f.dials, url)
	return nil
}

func (f *fakeWS) Send(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.open {
		return errClosed
	}
	f.sent = append(f.sent, text)
	return nil
}

func (f *fakeWS) PopMessages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.queue
	f.queue = nil
	return out
}

func (f *fakeWS) IsOpen() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open
}

func (f *fakeWS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open = false
	return nil
}

// Push enqueues an inbound frame as if Tibber sent it.
func (f *fakeWS) Push(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, text)
}

// Sent returns a copy of all frames the driver has sent so far.
func (f *fakeWS) Sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	copy(out, f.sent)
	return out
}

// errClosed avoids importing a real ws lib for the fake — the only
// caller is fakeWS.Send and it just needs a non-nil error.
var errClosed = &simpleErr{"ws not open"}

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

// newTestTibberDriver loads the real drivers/tibber.lua against a
// fake WS + the in-process telemetry store. Returns the driver plus
// the WS stub so tests can inject inbound frames.
func newTestTibberDriver(t *testing.T, apiKey, homeID string) (*LuaDriver, *fakeWS, *telemetry.Store) {
	t.Helper()
	tel := telemetry.NewStore()
	ws := &fakeWS{}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	luaPath := filepath.Join(wd, "..", "..", "..", "drivers", "tibber.lua")
	if _, err := os.Stat(luaPath); err != nil {
		t.Fatalf("tibber.lua not found at %s: %v", luaPath, err)
	}
	env := NewHostEnv("tibber", tel).WithWS(ws)
	// HTTP capability isn't strictly needed when home_id is passed in
	// config, but turning it on lets us cover the resolve_home_id path
	// in other tests if we wire a fake HTTP client.
	env.WithHTTP()

	d, err := NewLuaDriver(luaPath, env)
	if err != nil {
		t.Fatalf("load tibber.lua: %v", err)
	}
	cfg := map[string]any{"api_key": apiKey}
	if homeID != "" {
		cfg["home_id"] = homeID
	}
	if err := d.Init(context.Background(), cfg); err != nil {
		t.Fatalf("init: %v", err)
	}
	t.Cleanup(d.Cleanup)
	return d, ws, tel
}

// First poll with a configured home_id opens the WS and sends
// connection_init with the API key in the payload (NOT in HTTP headers —
// Tibber wants auth in the GraphQL init message).
func TestTibberDriverOpensWSAndSendsConnectionInit(t *testing.T) {
	d, ws, _ := newTestTibberDriver(t, "secret-token", "home-uuid-1")

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	if !ws.IsOpen() {
		t.Fatal("WS should be open after first poll with valid config")
	}
	if len(ws.dials) != 1 || !strings.HasPrefix(ws.dials[0], "wss://websocket-api.tibber.com/") {
		t.Errorf("dials = %v, want wss://websocket-api.tibber.com/...", ws.dials)
	}

	sent := ws.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d frames, want exactly 1 (connection_init)", len(sent))
	}
	if !strings.Contains(sent[0], `"type":"connection_init"`) {
		t.Errorf("first frame = %s, want connection_init", sent[0])
	}
	if !strings.Contains(sent[0], `"token":"secret-token"`) {
		t.Errorf("first frame must contain api_key in payload, got: %s", sent[0])
	}
}

// After connection_ack the driver must send a `subscribe` frame
// targeting the configured home_id and asking for liveMeasurement.
func TestTibberDriverSubscribesAfterConnectionAck(t *testing.T) {
	d, ws, _ := newTestTibberDriver(t, "secret", "home-xyz")

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	// First poll sent connection_init. Now Tibber's ack:
	ws.Push(`{"type":"connection_ack"}`)
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-2: %v", err)
	}

	sent := ws.Sent()
	if len(sent) < 2 {
		t.Fatalf("sent %d frames, want at least 2 (init + subscribe)", len(sent))
	}
	sub := sent[1]
	if !strings.Contains(sub, `"type":"subscribe"`) {
		t.Errorf("second frame = %s, want subscribe", sub)
	}
	// home_id flows through GraphQL variables, not the query body, so
	// a hostile config value can't break parse / inject fragments.
	if !strings.Contains(sub, `liveMeasurement(homeId: $homeId)`) {
		t.Errorf("subscribe must reference homeId via $homeId variable; got %s", sub)
	}
	if !strings.Contains(sub, `"variables":{"homeId":"home-xyz"}`) &&
		!strings.Contains(sub, `"variables":{"homeId":\"home-xyz\"}`) {
		t.Errorf("subscribe must pass home_id home-xyz via variables; got %s", sub)
	}
	if !strings.Contains(sub, "power") || !strings.Contains(sub, "currentL1") {
		t.Errorf("subscribe query missing key fields; got %s", sub)
	}
}

// A liveMeasurement payload turns into a meter emit using site convention
// (positive = importing, negative = exporting). Per-phase voltage/current
// land both in the meter Data and as TS metrics; signalStrength does too.
func TestTibberDriverEmitsMeterFromLiveMeasurement(t *testing.T) {
	d, ws, tel := newTestTibberDriver(t, "secret", "home-xyz")

	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	ws.Push(`{"type":"connection_ack"}`)
	// Importing 1200 W, no production. Per-phase numbers across L1-L3.
	ws.Push(`{"id":"lm","type":"next","payload":{"data":{"liveMeasurement":{` +
		`"power":1200,"powerProduction":0,` +
		`"accumulatedConsumption":12.5,"accumulatedProduction":0.7,` +
		`"voltagePhase1":231.1,"voltagePhase2":230.5,"voltagePhase3":229.9,` +
		`"currentL1":2.5,"currentL2":1.8,"currentL3":1.4,` +
		`"signalStrength":-68}}}}`)
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-2: %v", err)
	}

	meter := tel.Get("tibber", telemetry.DerMeter)
	if meter == nil {
		t.Fatal("expected meter reading")
	}
	if meter.RawW != 1200 {
		t.Errorf("meter RawW = %v, want 1200 (import)", meter.RawW)
	}
	for _, key := range []string{
		`"l1_v":231.1`, `"l2_v":230.5`, `"l3_v":229.9`,
		`"l1_a":2.5`, `"l2_a":1.8`, `"l3_a":1.4`,
		`"import_wh":12500`, `"export_wh":700`,
	} {
		if !strings.Contains(string(meter.Data), key) {
			t.Errorf("meter Data missing %s; got: %s", key, string(meter.Data))
		}
	}
	if gotSig, _, ok := tel.LatestMetric("tibber", "tibber_signal_dbm"); !ok || gotSig != -68 {
		t.Errorf("tibber_signal_dbm = %v ok=%v, want -68", gotSig, ok)
	}
}

// When the home is exporting (production > consumption), the meter w
// must be negative — that's the contract every site-meter driver shares.
func TestTibberDriverNetsExportNegative(t *testing.T) {
	d, ws, tel := newTestTibberDriver(t, "secret", "home-xyz")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	ws.Push(`{"type":"connection_ack"}`)
	// Exporting 800 W net (0 import, 800 production).
	ws.Push(`{"type":"next","payload":{"data":{"liveMeasurement":{` +
		`"power":0,"powerProduction":800}}}}`)
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-2: %v", err)
	}
	meter := tel.Get("tibber", telemetry.DerMeter)
	if meter == nil || meter.RawW != -800 {
		t.Fatalf("meter RawW = %v, want -800 (export)", meter.RawW)
	}
}

// Tibber server pings must be answered with pong, per the
// graphql-transport-ws spec. Without this the server eventually drops
// the connection as "client unresponsive".
func TestTibberDriverRespondsToServerPing(t *testing.T) {
	d, ws, _ := newTestTibberDriver(t, "secret", "home-xyz")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	beforeSent := len(ws.Sent())
	ws.Push(`{"type":"ping"}`)
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-2: %v", err)
	}
	sent := ws.Sent()
	if len(sent) <= beforeSent {
		t.Fatal("expected a new frame after server ping")
	}
	last := sent[len(sent)-1]
	if !strings.Contains(last, `"type":"pong"`) {
		t.Errorf("after server ping last frame = %s, want pong", last)
	}
}

// EOF sentinel ("" entry) in the inbound queue means the read pump
// exited. The driver must close + schedule a reconnect; it must NOT
// keep trying to send on the dead socket.
func TestTibberDriverHandlesEOFAndScheduledReconnect(t *testing.T) {
	d, ws, _ := newTestTibberDriver(t, "secret", "home-xyz")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !ws.IsOpen() {
		t.Fatal("WS should be open after first poll")
	}
	// Simulate the read pump exiting:
	ws.Push("")
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-2: %v", err)
	}
	if ws.IsOpen() {
		t.Error("WS must be closed after EOF sentinel")
	}
	// Reconnect is cooldown-gated so the next immediate poll should NOT
	// re-open the socket (would be a hot reconnect loop on auth fail).
	dialsBefore := len(ws.dials)
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll-3: %v", err)
	}
	if len(ws.dials) != dialsBefore {
		t.Errorf("reconnect happened immediately (cooldown bypassed): dials=%v", ws.dials)
	}
}
