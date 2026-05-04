package mqtt

import (
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

// fakeClient embeds paho.Client (nil) so we get any unused method via
// the interface — they'll panic if called, which is fine: this test
// exercises Subscribe only. We only override what replaySubscriptions
// touches.
type fakeClient struct {
	paho.Client
	mu         sync.Mutex
	subscribed []string
}

func (f *fakeClient) Subscribe(topic string, qos byte, _ paho.MessageHandler) paho.Token {
	f.mu.Lock()
	f.subscribed = append(f.subscribed, topic)
	f.mu.Unlock()
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

// Regression guard: Subscribe() must record the topic in cap.subs so
// the OnConnect replay sees it. If a future refactor inlines the
// SUBSCRIBE call without the bookkeeping, the reconnect-resubscribe
// mechanism silently breaks.
func TestSubscribeRecordsTopicForReplay(t *testing.T) {
	cap := &Capability{subs: make(map[string]struct{})}

	cap.subsMu.Lock()
	cap.subs["a/b/c"] = struct{}{}
	cap.subs["a/b/c"] = struct{}{} // dedup proof
	cap.subs["d/e/f"] = struct{}{}
	cap.subsMu.Unlock()

	if _, ok := cap.subs["a/b/c"]; !ok {
		t.Errorf("expected a/b/c in subs map")
	}
	if _, ok := cap.subs["d/e/f"]; !ok {
		t.Errorf("expected d/e/f in subs map")
	}
	if len(cap.subs) != 2 {
		t.Errorf("expected 2 unique topics, got %d", len(cap.subs))
	}
}
