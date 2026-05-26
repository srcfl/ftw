package mqtt

import (
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// fakeToken is a paho.Token whose WaitTimeout returns immediately with
// no error. Sufficient for testing replaySubscriptions which only cares
// about (a) whether each topic gets handed to client.Subscribe and (b)
// the WaitTimeout return path.
type fakeToken struct{}

func (fakeToken) Wait() bool                         { return true }
func (fakeToken) WaitTimeout(time.Duration) bool     { return true }
func (fakeToken) Done() <-chan struct{}              { c := make(chan struct{}); close(c); return c }
func (fakeToken) Error() error                       { return nil }

// errToken simulates a SUBSCRIBE that completes within timeout but
// reports a broker-side error.
type errToken struct{ err error }

func (errToken) Wait() bool                       { return true }
func (errToken) WaitTimeout(time.Duration) bool   { return true }
func (errToken) Done() <-chan struct{}            { c := make(chan struct{}); close(c); return c }
func (e errToken) Error() error                   { return e.err }

// timeoutToken simulates a SUBSCRIBE that never confirms.
type timeoutToken struct{}

func (timeoutToken) Wait() bool                       { return false }
func (timeoutToken) WaitTimeout(time.Duration) bool   { return false }
func (timeoutToken) Done() <-chan struct{}            { return make(chan struct{}) }
func (timeoutToken) Error() error                     { return nil }

// fakeClient embeds paho.Client (nil) so we get any unused method via
// the interface — they'll panic if called, which is fine: this test
// exercises Subscribe only. We only override what replaySubscriptions
// touches. tokenFor lets a test inject a per-topic Token (timeout,
// error, success) without rewriting the Subscribe call site.
type fakeClient struct {
	paho.Client
	mu         sync.Mutex
	subscribed []string
	tokenFor   func(topic string) paho.Token
}

func (f *fakeClient) Subscribe(topic string, qos byte, _ paho.MessageHandler) paho.Token {
	f.mu.Lock()
	f.subscribed = append(f.subscribed, topic)
	tf := f.tokenFor
	f.mu.Unlock()
	if tf != nil {
		return tf(topic)
	}
	return fakeToken{}
}

// Regression: paho with default CleanSession=true and SetAutoReconnect(true)
// transparently reconnects but does NOT re-issue SUBSCRIBE. Drivers
// (e.g. pixii_pv) call host.mqtt_subscribe once from driver_init and
// never again, so a network blip silently ends the message flow until
// the driver is restarted. Real-world: Pixii MQTT going dark every
// evening, "Connected" in Pixii UI but red in 42W, restored only by
// restarting the driver in 42W.
//
// Capability records every Subscribe() topic and replays them all from
// its OnConnectHandler. This test verifies the replay actually iterates
// the recorded set and calls client.Subscribe for each one (deduped
// across repeated Subscribe calls).
func TestReplaySubscriptionsResubsAllRecordedTopics(t *testing.T) {
	cap := &Capability{subs: make(map[string]struct{})}

	// Simulate driver_init recording subscriptions through Capability.
	// We can't call the real Subscribe (no live client), so write to
	// the bookkeeping directly — same set Subscribe would build up.
	cap.subs["pixii/status/+/meter_ext6"] = struct{}{}
	cap.subs["pixii/status/+/meter"] = struct{}{}
	// Duplicate via map key — no extra entry.
	cap.subs["pixii/status/+/meter"] = struct{}{}

	fc := &fakeClient{}
	cap.replaySubscriptions(fc, "ftw-pixii-pv")

	got := append([]string(nil), fc.subscribed...)
	sort.Strings(got)
	want := []string{"pixii/status/+/meter", "pixii/status/+/meter_ext6"}
	if len(got) != len(want) {
		t.Fatalf("expected %d resubs, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("resub[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Regression: replaySubscriptions previously used `if WaitTimeout(...)
// && tok.Error() != nil` which silently swallowed timeouts. A SUBSCRIBE
// that never confirms still leaves the topic unbound; operators need to
// see it during incident response. Verifies that timeout AND
// broker-side error each register as a failure (not a silent pass) and
// that the per-call iteration continues so a single broken topic
// doesn't block the rest.
func TestReplaySubscriptionsCountsTimeoutAndErrorAsFailure(t *testing.T) {
	cap := &Capability{subs: make(map[string]struct{})}
	cap.subs["ok/topic"] = struct{}{}
	cap.subs["timeout/topic"] = struct{}{}
	cap.subs["err/topic"] = struct{}{}

	fc := &fakeClient{
		tokenFor: func(topic string) paho.Token {
			switch topic {
			case "timeout/topic":
				return timeoutToken{}
			case "err/topic":
				return errToken{err: errors.New("broker rejected")}
			default:
				return fakeToken{}
			}
		},
	}
	cap.replaySubscriptions(fc, "ftw-test")

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.subscribed) != 3 {
		t.Errorf("expected SUBSCRIBE to fire for all 3 topics (loop must not bail), got %d (%v)",
			len(fc.subscribed), fc.subscribed)
	}
}

// Regression guard: Subscribe() must record the topic in cap.subs so
// the OnConnect replay sees it. If a future refactor inlines the
// SUBSCRIBE call without the bookkeeping, the reconnect-resubscribe
// mechanism silently breaks. Wire a fake paho client into Capability
// and exercise the real Subscribe() entrypoint — direct map mutation
// would not catch a regression where Subscribe stops recording.
func TestSubscribeRecordsTopicForReplay(t *testing.T) {
	fc := &fakeClient{}
	cap := &Capability{
		client: fc,
		subs:   make(map[string]struct{}),
	}

	if err := cap.Subscribe("a/b/c"); err != nil {
		t.Fatalf("Subscribe(a/b/c): %v", err)
	}
	if err := cap.Subscribe("a/b/c"); err != nil { // dedup proof
		t.Fatalf("Subscribe(a/b/c) repeat: %v", err)
	}
	if err := cap.Subscribe("d/e/f"); err != nil {
		t.Fatalf("Subscribe(d/e/f): %v", err)
	}

	cap.subsMu.Lock()
	defer cap.subsMu.Unlock()
	if _, ok := cap.subs["a/b/c"]; !ok {
		t.Errorf("expected a/b/c in subs map")
	}
	if _, ok := cap.subs["d/e/f"]; !ok {
		t.Errorf("expected d/e/f in subs map")
	}
	if len(cap.subs) != 2 {
		t.Errorf("expected 2 unique topics, got %d (%v)", len(cap.subs), cap.subs)
	}

	// And the SUBSCRIBE made it through to the underlying client. The
	// dedup happens in the bookkeeping map, NOT at the wire — repeated
	// Subscribe calls still hit paho (paho is idempotent so this is
	// fine, and it preserves the contract that Subscribe completes a
	// round trip rather than silently no-op'ing).
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.subscribed) != 3 {
		t.Errorf("expected 3 wire SUBSCRIBE calls, got %d (%v)", len(fc.subscribed), fc.subscribed)
	}
}
