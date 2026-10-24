package appuplink

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// A watch reaches the box through the exchange and never through the stream,
// so it can never take a phone's place. The roster gives it tun, because its
// exchange rides FTW Cloud, but its key in the tunnel gets the silence a
// stranger gets.
func TestAWatchKeyGetsNoStream(t *testing.T) {
	r := newTunRig(t)
	phone := r.pair(t)
	grant, ok := r.enroll.GrantFor(cloudwire.DeviceID(phone.staticPublic()))
	if !ok {
		t.Fatal("the phone did not pair")
	}
	watch, err := newAppClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.enroll.AddWatch(grant.DeviceID, watch.staticPublic()); err != nil {
		t.Fatalf("adding the watch: %v", err)
	}

	m1, err := watch.message1(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.tunnel.Deliver(cloudwire.Tun{Dev: cloudwire.DeviceID(watch.staticPublic()), Sid: 1, D: m1})
	r.silent(t, 750*time.Millisecond)
	if r.tunnel.count() != 0 {
		t.Fatal("the box opened a stream for a watch")
	}
}
