package mqtt

import (
	"fmt"
	"testing"

	"github.com/srcfl/ftw/go/internal/drivers"
)

// A driver that stops draining must not be able to grow the inbound queue
// without limit. paho's handler goroutine outlives any single poll — it
// keeps delivering across auto-reconnects whether or not the driver is
// still calling host.mqtt_messages() — so on a box with 1 GB of RAM an
// undrained subscription to a busy broker is an out-of-memory kill of the
// whole controller, not just of one driver.
func TestEnqueueBoundsUndrainedQueue(t *testing.T) {
	c := &Capability{clientID: "test"}

	const sent = 50_000
	for i := 0; i < sent; i++ {
		c.enqueue(drivers.MQTTMessage{Topic: "extapi/data/ehub", Payload: fmt.Sprintf("%d", i)})
	}

	got := c.PopMessages()
	if len(got) > maxIncoming {
		t.Fatalf("queue held %d messages after %d undrained publishes, bound is %d",
			len(got), sent, maxIncoming)
	}
	if len(got) == 0 {
		t.Fatal("queue held nothing; the bound must drop old messages, not stop accepting new ones")
	}
	// Overflow drops the oldest: the most recent publish is what the
	// driver's next poll acts on, so it must survive.
	if want := fmt.Sprintf("%d", sent-1); got[len(got)-1].Payload != want {
		t.Fatalf("newest queued payload = %q, want %q", got[len(got)-1].Payload, want)
	}
	if c.dropped == 0 {
		t.Fatal("dropped counter stayed at zero after overflow")
	}
}

// Normal operation — a driver polling faster than the broker publishes —
// must be untouched by the bound.
func TestEnqueueKeepsEveryMessageBelowTheBound(t *testing.T) {
	c := &Capability{clientID: "test"}

	for i := 0; i < maxIncoming-1; i++ {
		c.enqueue(drivers.MQTTMessage{Topic: "extapi/data/eso", Payload: fmt.Sprintf("%d", i)})
	}
	got := c.PopMessages()
	if len(got) != maxIncoming-1 {
		t.Fatalf("queue held %d of %d messages below the bound", len(got), maxIncoming-1)
	}
	if c.dropped != 0 {
		t.Fatalf("dropped %d messages below the bound", c.dropped)
	}
	if got[0].Payload != "0" {
		t.Fatalf("oldest queued payload = %q, want %q", got[0].Payload, "0")
	}
}

// PopMessages clears the queue, so a driver that resumes draining gets the
// full bound back.
func TestEnqueueRecoversHeadroomAfterDrain(t *testing.T) {
	c := &Capability{clientID: "test"}

	for i := 0; i < maxIncoming*3; i++ {
		c.enqueue(drivers.MQTTMessage{Topic: "t", Payload: "x"})
	}
	c.PopMessages()
	for i := 0; i < maxIncoming-1; i++ {
		c.enqueue(drivers.MQTTMessage{Topic: "t", Payload: fmt.Sprintf("%d", i)})
	}
	if got := c.PopMessages(); len(got) != maxIncoming-1 {
		t.Fatalf("after draining, queue held %d of %d messages", len(got), maxIncoming-1)
	}
}
