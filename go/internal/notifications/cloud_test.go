package notifications

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/events"
)

// FTW Cloud on or off reaches the owners even on a box whose stored rules
// predate the two kinds, and stays silent when an owner turned it off.
func TestCloudGrantNoticesAreOnUnlessTurnedOff(t *testing.T) {
	pub := &fakePub{}
	svc, _ := newSvc(pushCfg(config.NotificationRule{Type: PushChargingSessionComplete, Enabled: true}), pub)
	svc.NotifyCloudGrant(PushCloudGranted)
	settle(svc)
	msgs := pub.Messages()
	if len(msgs) != 1 || msgs[0].Kind != PushCloudGranted || msgs[0].Title != "FTW Cloud is on" {
		t.Fatalf("messages %+v", msgs)
	}

	pub = &fakePub{}
	svc, _ = newSvc(pushCfg(config.NotificationRule{Type: PushCloudRevoked, Enabled: false}), pub)
	svc.NotifyCloudGrant(PushCloudRevoked)
	svc.NotifyCloudGrant("charging.connected")
	settle(svc)
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("an owner's off switch was ignored, or another kind went through: %d", n)
	}
}

// A security notice goes whether or not the owner turned notifications on,
// and whatever its rule says.
func TestSecurityNoticesPassTheMasterSwitch(t *testing.T) {
	pub := &fakePub{}
	cfg := pushCfg(config.NotificationRule{Type: PushAgentAdded, Enabled: false})
	cfg.Enabled = false
	svc, _ := newSvc(cfg, pub)
	svc.NotifyAgents(PushAgentAdded, false)
	svc.NotifyAgents(PushOwnerAdded, false)
	svc.NotifyAgents(PushChargingConnected, false)
	settle(svc)
	msgs := pub.Messages()
	if len(msgs) != 2 || msgs[0].Kind != PushAgentAdded || msgs[1].Kind != PushOwnerAdded {
		t.Fatalf("messages %+v", msgs)
	}
}

// FTW Cloud takes a security notice for the owners' phones before the box's
// own transports hear of it. A phone paired as the first owner finds web
// push with no subscription yet and FTW Cloud's event lane closed to notice
// kinds, yet FTW Cloud carried the notice: the history says sent (#1785).
// Without FTW Cloud nobody was told, and the failure says so plainly.
func TestASecurityNoticeFTWCloudTookCountsAsSent(t *testing.T) {
	svc, _ := newSvc(pushCfg(), nil)
	svc.AddPublisher(&nothingPub{})
	svc.AddPublisher(CloudPublisher{Queue: func(Message) bool { return false }})
	bus := events.NewBus()
	svc.Subscribe(bus)
	dispatched := watchDispatched(bus)

	svc.NotifyAgents(PushOwnerAdded, true)
	if ev := waitForDispatched(t, dispatched); ev.Status != "sent" || ev.Error != "" {
		t.Fatalf("carried by FTW Cloud: status %q, error %q", ev.Status, ev.Error)
	}
	if st := svc.Status(); st.Sent != 1 || st.Failed != 0 {
		t.Fatalf("sent=%d failed=%d, want 1/0", st.Sent, st.Failed)
	}

	svc.NotifyAgents(PushOwnerAdded, false)
	if ev := waitForDispatched(t, dispatched); ev.Status != "failed" || ev.Error != "no phone or service to send to" {
		t.Fatalf("told nobody: status %q, error %q", ev.Status, ev.Error)
	}
	if st := svc.Status(); st.Sent != 1 || st.Failed != 1 {
		t.Fatalf("sent=%d failed=%d, want 1/1", st.Sent, st.Failed)
	}
}
