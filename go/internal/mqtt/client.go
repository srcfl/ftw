// Package mqtt provides an MQTT capability wrapper for per-driver binding.
package mqtt

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/frahlg/forty-two-watts/go/internal/drivers"
)

// Capability wraps a paho client to match drivers.MQTTCap.
type Capability struct {
	client paho.Client

	mu       sync.Mutex
	incoming []drivers.MQTTMessage

	// subsMu guards the subscription set. Released before any
	// network call (SUBSCRIBE) — never held across I/O.
	subsMu sync.Mutex
	subs   map[string]struct{}
}

// Dial connects to an MQTT broker and returns a Capability.
//
// On reconnect (paho's auto-reconnect), the broker has no record of the
// previous session's subscriptions because we use the default
// CleanSession=true. Without intervention, paho does NOT automatically
// re-issue SUBSCRIBE on reconnect — the TCP link comes back but the
// driver never receives another message, the poll loop reads an empty
// queue forever, and the watchdog flips the driver offline. Restarting
// the driver re-runs driver_init → host.mqtt_subscribe and traffic
// resumes (real incident: Pixii MQTT going silent at dusk after a broker
// blip, recovered only by 42W-side driver restart).
//
// Fix: every Subscribe() records the topic in cap.subs and issues
// SUBSCRIBE directly (so the initial flow from driver_init is
// unchanged). The OnConnectHandler replays the recorded set on every
// (re)connect — on first connect cap.subs is still empty (driver_init
// hasn't run yet), which is fine: the replay is a no-op and Subscribe
// will set up the bindings in a moment. From the second connect onward
// the replay is what restores the subscription set the broker just
// dropped.
func Dial(host string, port int, username, password, clientID string) (*Capability, error) {
	cap := &Capability{
		subs: make(map[string]struct{}),
	}
	opts := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%d", host, port)).
		SetClientID(clientID).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(c paho.Client) {
			cap.replaySubscriptions(c, clientID)
		}).
		SetDefaultPublishHandler(func(_ paho.Client, m paho.Message) {
			cap.mu.Lock()
			cap.incoming = append(cap.incoming, drivers.MQTTMessage{
				Topic:   m.Topic(),
				Payload: string(m.Payload()),
			})
			cap.mu.Unlock()
		})
	if username != "" {
		opts.SetUsername(username)
	}
	if password != "" {
		opts.SetPassword(password)
	}
	cap.client = paho.NewClient(opts)
	if tok := cap.client.Connect(); tok.WaitTimeout(10*time.Second) && tok.Error() != nil {
		return nil, tok.Error()
	}
	return cap, nil
}

// Close disconnects the client. Returns error so the signature matches
// drivers.MQTTCap — that lets the registry call Close() uniformly at
// driver teardown. Without it a stale paho client stays connected
// under the same clientID; the broker kicks the newer one on the
// next Dial and subscribe ACKs to the new client race with the old
// disconnect, which is what caused ferroamp to go silent after a
// POST /api/drivers/ferroamp/restart on 2026-04-17.
func (c *Capability) Close() error {
	c.client.Disconnect(250)
	return nil
}

// replaySubscriptions issues a SUBSCRIBE for every topic recorded in
// cap.subs against the given paho client. Called from OnConnectHandler
// on every (re)connect; on the very first connect cap.subs is empty
// because driver_init hasn't run yet — the loop becomes a no-op and
// Subscribe handles initial bindings directly. From the second connect
// onward this is what restores the dropped subscription set.
//
// Default CleanSession=true means the broker drops the session on
// disconnect; without this an evening network blip silently leaves the
// driver subscribed to nothing while the TCP link looks healthy.
//
// Failure handling: a SUBSCRIBE token can either complete with an error
// (broker rejected) or time out without completing (broker hung,
// network slow). Both leave the topic unbound; both must surface so
// operators see them during incident response. The summary line at the
// end reports total + failure counts and downgrades from "restored" to
// "partially restored" when any subscription failed.
func (c *Capability) replaySubscriptions(client paho.Client, clientID string) {
	c.subsMu.Lock()
	topics := make([]string, 0, len(c.subs))
	for t := range c.subs {
		topics = append(topics, t)
	}
	c.subsMu.Unlock()
	if len(topics) == 0 {
		return
	}
	failed := 0
	for _, t := range topics {
		tok := client.Subscribe(t, 0, nil)
		if !tok.WaitTimeout(5 * time.Second) {
			// SUBSCRIBE didn't complete within timeout — topic is
			// unbound until the next reconnect. The driver's poll
			// loop will see no messages, watchdog will flip it
			// offline.
			slog.Warn("mqtt: resubscribe timed out", "topic", t, "client_id", clientID)
			failed++
			continue
		}
		if err := tok.Error(); err != nil {
			slog.Warn("mqtt: resubscribe failed", "topic", t, "client_id", clientID, "err", err)
			failed++
		}
	}
	if failed > 0 {
		slog.Warn("mqtt: (re)connected, partially restored subscriptions",
			"client_id", clientID, "count", len(topics), "failed", failed)
	} else {
		slog.Info("mqtt: (re)connected, restored subscriptions",
			"client_id", clientID, "count", len(topics))
	}
}

// Subscribe — implements drivers.MQTTCap.
//
// Records the topic in cap.subs so OnConnect can re-issue it after a
// reconnect. Subscribe() is typically called once per topic from
// driver_init, but is idempotent — repeated calls are deduped by the
// map.
func (c *Capability) Subscribe(topic string) error {
	c.subsMu.Lock()
	c.subs[topic] = struct{}{}
	c.subsMu.Unlock()
	tok := c.client.Subscribe(topic, 0, nil)
	if !tok.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("mqtt: subscribe timed out: topic=%q", topic)
	}
	return tok.Error()
}

// Publish — implements drivers.MQTTCap.
func (c *Capability) Publish(topic string, payload []byte) error {
	tok := c.client.Publish(topic, 0, false, payload)
	if !tok.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("mqtt: publish timed out: topic=%q", topic)
	}
	return tok.Error()
}

// PopMessages — implements drivers.MQTTCap.
func (c *Capability) PopMessages() []drivers.MQTTMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.incoming
	c.incoming = nil
	return out
}
