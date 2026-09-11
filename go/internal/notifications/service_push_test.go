package notifications

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/events"
)

// nothingPub is a transport with nowhere to deliver — web push before the
// first subscription is stored.
type nothingPub struct{ calls int }

func (n *nothingPub) Publish(context.Context, Message) error {
	n.calls++
	return ErrNothingToSend
}

func pushCfg(types ...config.NotificationRule) *config.Notifications {
	return &config.Notifications{Enabled: true, DefaultPriority: 3, Events: types}
}

func waitForMsgs(t *testing.T, pub *fakePub, n int) []Message {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(pub.Messages()) < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	msgs := pub.Messages()
	if len(msgs) < n {
		t.Fatalf("got %d messages, want %d", len(msgs), n)
	}
	return msgs
}

// settle waits for any in-flight handleCatalogued goroutines to finish
// delivering by watching the counters stop moving.
func settle(svc *Service) {
	for i := 0; i < 3; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	svc.mu.Lock()
	svc.mu.Unlock() //nolint:staticcheck // lock barrier, not a critical section
}

// A charging session completing reaches the phone with the catalogue's own
// words and the session's own kWh — and only once, because the emitter's
// latch already fired once and the engine adds nothing on top.
func TestSessionCompleteRendersFromCatalogue(t *testing.T) {
	pub := &fakePub{published: make(chan struct{}, 4)}
	svc, _ := newSvc(pushCfg(config.NotificationRule{
		Type: PushChargingSessionComplete, Enabled: true, Priority: 3,
	}), pub)
	bus := events.NewBus()
	svc.Subscribe(bus)

	bus.Publish(events.ChargingSessionComplete{LoadpointID: "garage", KWh: 7.42, At: time.Now()})
	msgs := waitForMsgs(t, pub, 1)
	if msgs[0].Title != "Car charged" {
		t.Fatalf("title = %q", msgs[0].Title)
	}
	if msgs[0].Body != "7.4 kWh delivered — ready to go." {
		t.Fatalf("body = %q", msgs[0].Body)
	}
}

// The interrupted rule's cooldown holds across repeats: the emitter's
// hysteresis makes each event real, and the engine still refuses to turn a
// charger failing all night into a feed.
func TestInterruptedHonorsCooldownPerLoadpoint(t *testing.T) {
	pub := &fakePub{published: make(chan struct{}, 4)}
	svc, clk := newSvc(pushCfg(config.NotificationRule{
		Type: PushChargingInterrupted, Enabled: true, Priority: 4, CooldownS: 3600,
	}), pub)
	bus := events.NewBus()
	svc.Subscribe(bus)

	bus.Publish(events.ChargingInterrupted{LoadpointID: "garage", At: clk.now()})
	waitForMsgs(t, pub, 1)
	if got := pub.Messages()[0]; got.Title != "Charging stopped early" ||
		got.Body != "The car stopped charging before it was done." {
		t.Fatalf("message = %+v", got)
	}

	// Same loadpoint inside the cooldown: silenced.
	clk.advance(10 * time.Minute)
	bus.Publish(events.ChargingInterrupted{LoadpointID: "garage", At: clk.now()})
	settle(svc)
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("cooldown did not hold: %d messages", n)
	}

	// A different loadpoint is a different fact.
	bus.Publish(events.ChargingInterrupted{LoadpointID: "street", At: clk.now()})
	waitForMsgs(t, pub, 2)

	// Past the cooldown the first one may speak again.
	clk.advance(time.Hour)
	bus.Publish(events.ChargingInterrupted{LoadpointID: "garage", At: clk.now()})
	waitForMsgs(t, pub, 3)
}

func TestUpdateInstalledRendersVersion(t *testing.T) {
	pub := &fakePub{published: make(chan struct{}, 4)}
	svc, _ := newSvc(pushCfg(config.NotificationRule{
		Type: PushUpdateInstalled, Enabled: true, Priority: 2,
	}), pub)
	bus := events.NewBus()
	svc.Subscribe(bus)

	bus.Publish(events.UpdateInstalled{Version: "v1.17.0", PreviousVersion: "v1.16.1", At: time.Now()})
	msgs := waitForMsgs(t, pub, 1)
	if !strings.Contains(msgs[0].Body, "v1.17.0") {
		t.Fatalf("body = %q", msgs[0].Body)
	}
	if msgs[0].Title != "Your box updated itself" {
		t.Fatalf("title = %q", msgs[0].Title)
	}
}

// A catalogue rule that is not enabled stays silent — the operator opts in
// per event, same as every rule before it.
func TestCataloguedEventNeedsItsRule(t *testing.T) {
	pub := &fakePub{}
	svc, _ := newSvc(pushCfg(config.NotificationRule{
		Type: PushChargingSessionComplete, Enabled: false,
	}), pub)
	bus := events.NewBus()
	svc.Subscribe(bus)
	bus.Publish(events.ChargingSessionComplete{LoadpointID: "garage", KWh: 5, At: time.Now()})
	settle(svc)
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("disabled rule dispatched %d messages", n)
	}
}

// Both transports hear one dispatch: the config-selected provider and the
// engine-owned one. The household that had ntfy keeps ntfy.
func TestDispatchReachesEveryInstalledPublisher(t *testing.T) {
	ntfy := &fakePub{published: make(chan struct{}, 4)}
	push := &fakePub{published: make(chan struct{}, 4)}
	svc, _ := newSvc(pushCfg(config.NotificationRule{
		Type: PushChargingSessionComplete, Enabled: true,
	}), ntfy)
	svc.AddPublisher(push)
	bus := events.NewBus()
	svc.Subscribe(bus)

	bus.Publish(events.ChargingSessionComplete{LoadpointID: "garage", KWh: 3.0, At: time.Now()})
	waitForMsgs(t, ntfy, 1)
	waitForMsgs(t, push, 1)
	if svc.Status().Sent != 1 {
		t.Fatalf("sent = %d, want 1 (one dispatch, two transports)", svc.Status().Sent)
	}
}

// A transport with no subscribers is absence, not failure: the dispatch
// still counts as sent through the provider that delivered, and the failed
// counter stays put.
func TestNothingToSendIsNotAFailure(t *testing.T) {
	ntfy := &fakePub{published: make(chan struct{}, 4)}
	empty := &nothingPub{}
	svc, _ := newSvc(pushCfg(config.NotificationRule{
		Type: PushChargingSessionComplete, Enabled: true,
	}), ntfy)
	svc.AddPublisher(empty)
	bus := events.NewBus()
	svc.Subscribe(bus)

	bus.Publish(events.ChargingSessionComplete{LoadpointID: "garage", KWh: 3.0, At: time.Now()})
	waitForMsgs(t, ntfy, 1)
	st := svc.Status()
	if st.Sent != 1 || st.Failed != 0 {
		t.Fatalf("sent=%d failed=%d, want 1/0", st.Sent, st.Failed)
	}

	// Alone, an empty transport means nobody was told — and that is the
	// truthful "no publisher" failure, not a quiet success.
	svc2, _ := newSvc(pushCfg(config.NotificationRule{
		Type: PushChargingSessionComplete, Enabled: true,
	}), nil)
	svc2.AddPublisher(&nothingPub{})
	bus2 := events.NewBus()
	svc2.Subscribe(bus2)
	bus2.Publish(events.ChargingSessionComplete{LoadpointID: "garage", KWh: 3.0, At: time.Now()})
	settle(svc2)
	deadline := time.Now().Add(time.Second)
	for svc2.Status().Failed < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st := svc2.Status(); st.Failed != 1 || st.Sent != 0 {
		t.Fatalf("sent=%d failed=%d, want 0/1", st.Sent, st.Failed)
	}
}

// Reload must not shake off the engine-owned transport: it survives every
// config change the way subscriptions survive a settings toggle.
func TestEngineOwnedPublisherSurvivesReload(t *testing.T) {
	push := &fakePub{published: make(chan struct{}, 4)}
	cfg := pushCfg(config.NotificationRule{Type: PushChargingSessionComplete, Enabled: true})
	svc, _ := newSvc(cfg, nil)
	svc.AddPublisher(push)
	bus := events.NewBus()
	svc.Subscribe(bus)

	svc.Reload(pushCfg(config.NotificationRule{Type: PushChargingSessionComplete, Enabled: true}))
	svc.SetPublisher(nil)

	bus.Publish(events.ChargingSessionComplete{LoadpointID: "garage", KWh: 1.0, At: time.Now()})
	waitForMsgs(t, push, 1)
}

func TestConnectedPushNeedsOptInAndKeepsChargerDestination(t *testing.T) {
	pub := &fakePub{published: make(chan struct{}, 4)}
	svc, clk := newSvc(pushCfg(config.NotificationRule{Type: PushChargingConnected, Enabled: false, CooldownS: 60}), pub)
	bus := events.NewBus()
	svc.Subscribe(bus)
	bus.Publish(events.ChargingConnected{LoadpointID: "garage", At: clk.now()})
	settle(svc)
	if len(pub.Messages()) != 0 {
		t.Fatal("sent without opt-in")
	}
	svc.Reload(pushCfg(config.NotificationRule{Type: PushChargingConnected, Enabled: true, CooldownS: 60}))
	bus.Publish(events.ChargingConnected{LoadpointID: "garage", At: clk.now()})
	msgs := waitForMsgs(t, pub, 1)
	if msgs[0].Kind != PushChargingConnected || msgs[0].LoadpointID != "garage" || msgs[0].Title != "Car plugged in" {
		t.Fatalf("wrong message: %+v", msgs[0])
	}
	bus.Publish(events.ChargingConnected{LoadpointID: "garage", At: clk.now()})
	settle(svc)
	if len(pub.Messages()) != 1 {
		t.Fatal("repeated inside cooldown")
	}
	bus.Publish(events.ChargingConnected{LoadpointID: "street", At: clk.now()})
	waitForMsgs(t, pub, 2)
}
