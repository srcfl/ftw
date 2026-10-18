package appuplink

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/protocol/appwire"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// capturedTun is the box's link to FTW Cloud, as the tunnel sees it.
type capturedTun struct {
	ch chan cloudwire.Tun
}

func (c *capturedTun) SendTun(t cloudwire.Tun, _ bool) error {
	select {
	case c.ch <- t:
		return nil
	default:
		return errTunGone
	}
}

type tunRig struct {
	enroll *appenroll.Identity
	tunnel *Tunnel
	slots  *slots
	out    *capturedTun
	modes  *countingModes
}

func newTunRig(t *testing.T) *tunRig {
	t.Helper()
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	r := &tunRig{enroll: enroll, slots: &slots{}, out: &capturedTun{ch: make(chan cloudwire.Tun, 64)},
		modes: &countingModes{}}
	r.tunnel = newTunnel(handlerOver(r.modes, appproto.NewCmdLog()), enroll, r.slots, nil, slog.New(slog.DiscardHandler))
	r.tunnel.Attach(r.out)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.tunnel.Run(ctx)
	return r
}

// pair enrols a phone directly, as a pairing over any carrier would.
func (r *tunRig) pair(t *testing.T) *appClient {
	t.Helper()
	code, _, err := r.enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newAppClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.enroll.Authorise(app.staticPublic(), code); err != nil {
		t.Fatal(err)
	}
	return app
}

// next is the next tun the box sent, failing after d.
func (r *tunRig) next(t *testing.T, d time.Duration) cloudwire.Tun {
	t.Helper()
	select {
	case m := <-r.out.ch:
		return m
	case <-time.After(d):
		t.Fatalf("the box sent no tun within %s", d)
		return cloudwire.Tun{}
	}
}

// silent fails if the box sends a tun within d.
func (r *tunRig) silent(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m := <-r.out.ch:
		t.Fatalf("the box sent %+v", m)
	case <-time.After(d):
	}
}

// open runs the handshake for app under dev and sid, through the tunnel.
func (r *tunRig) open(t *testing.T, app *appClient, dev string, sid uint32, code []byte) {
	t.Helper()
	m1, err := app.message1(r.enroll.StaticKey().Public(), code)
	if err != nil {
		t.Fatal(err)
	}
	r.tunnel.Deliver(cloudwire.Tun{Dev: dev, Sid: sid, D: m1})
	reply := r.next(t, 3*time.Second)
	if reply.Dev != dev || reply.Sid != sid || reply.Ended() {
		t.Fatalf("the handshake's answer is %+v", reply)
	}
	if err := app.readMessage2(reply.D); err != nil {
		t.Fatal(err)
	}
}

func (r *tunRig) send(t *testing.T, app *appClient, dev string, sid uint32, msgType string, body any) {
	t.Helper()
	raw, err := appproto.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := appwire.EncodeFrame(appwire.Frame{Lane: appwire.LaneControl, Bucket: 512,
		Envelope: appwire.Envelope{T: msgType, B: raw}})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := app.encrypt(frame)
	if err != nil {
		t.Fatal(err)
	}
	r.tunnel.Deliver(cloudwire.Tun{Dev: dev, Sid: sid, D: msg})
}

// receive is the next frame of type want for app, skipping lane 0 ticks.
func (r *tunRig) receive(t *testing.T, app *appClient, want string) appwire.Frame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m := r.next(t, time.Until(deadline))
		if m.Ended() {
			t.Fatalf("the box ended the session while waiting for %s", want)
		}
		plain, err := app.decrypt(m.D)
		if err != nil {
			t.Fatal(err)
		}
		f, err := appwire.DecodeFrame(plain)
		if err != nil {
			t.Fatal(err)
		}
		if f.Envelope.T == want {
			return f
		}
	}
	t.Fatalf("no %s", want)
	return appwire.Frame{}
}

func (r *tunRig) hello(t *testing.T, app *appClient, dev string, sid uint32) {
	t.Helper()
	r.send(t, app, dev, sid, appproto.MsgHello, appproto.Hello{
		Proto: appproto.ProtoRange{Min: 0, Max: appproto.ProtoMax},
		App:   appproto.AppInfo{Build: "test", UA: "ios"},
	})
	r.receive(t, app, appproto.MsgHelloOK)
}

func setModeCmd(id string) appproto.Cmd {
	return appproto.Cmd{
		CmdID: id, Op: appproto.OpSetMode,
		Args:            map[string]any{"mode": string(control.ModeSelfConsumption)},
		NotValidAfterMs: 10 * 60 * 1000, Expect: appproto.Expect{Rev: 1},
	}
}

// A paired phone's session runs through the tunnel as over the relay: the
// same handshake, the same hello and a command the box acts on and answers.
func TestATunnelSessionAnswersACommand(t *testing.T) {
	r := newTunRig(t)
	app := r.pair(t)
	dev := cloudwire.DeviceID(app.staticPublic())
	r.open(t, app, dev, 7, nil)
	r.hello(t, app, dev, 7)

	r.send(t, app, dev, 7, appproto.MsgCmd, setModeCmd("0192f2a0-7c1e-7000-8000-00000000a001"))
	r.receive(t, app, appproto.MsgCmdAck)
	var res appproto.CmdResult
	if err := appproto.Unmarshal(r.receive(t, app, appproto.MsgCmdResult).Envelope.B, &res); err != nil {
		t.Fatal(err)
	}
	if res.State != appproto.CmdApplied || r.modes.count() != 1 {
		t.Fatalf("result %q after %d mode changes", res.State, r.modes.count())
	}
}

// The service names the device, and the box checks it against the key the
// handshake proved. A session put under another device's id opens nothing
// and gets no answer.
func TestAForgedDevGetsNothing(t *testing.T) {
	r := newTunRig(t)
	app := r.pair(t)
	other := r.pair(t)
	m1, err := app.message1(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.tunnel.Deliver(cloudwire.Tun{Dev: cloudwire.DeviceID(other.staticPublic()), Sid: 1, D: m1})
	r.silent(t, 500*time.Millisecond)
	if r.tunnel.count() != 0 {
		t.Fatal("a session opened under another device's id")
	}
}

// A key the box never paired gets nothing, through the tunnel as over the
// relay, and neither does a wrong pairing code.
func TestAStrangerThroughTheTunnelGetsNothing(t *testing.T) {
	r := newTunRig(t)
	if _, _, err := r.enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL); err != nil {
		t.Fatal(err)
	}
	for _, code := range [][]byte{nil, make([]byte, appenroll.PairingCodeBytes)} {
		app, err := newAppClient()
		if err != nil {
			t.Fatal(err)
		}
		m1, err := app.message1(r.enroll.StaticKey().Public(), code)
		if err != nil {
			t.Fatal(err)
		}
		r.tunnel.Deliver(cloudwire.Tun{Dev: cloudwire.DeviceID(app.staticPublic()), Sid: 1, D: m1})
		r.silent(t, 400*time.Millisecond)
	}
	if r.tunnel.count() != 0 || r.enroll.AuthorisedCount() != 0 {
		t.Fatal("a stranger got a session or a pairing")
	}
}

// A new phone pairs through the tunnel with the code from the QR, inside its
// handshake, exactly as over the relay.
func TestPairingThroughTheTunnel(t *testing.T) {
	r := newTunRig(t)
	code, _, err := r.enroll.MintPairingCode(apiauth.RoleViewer, appenroll.PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newAppClient()
	if err != nil {
		t.Fatal(err)
	}
	dev := cloudwire.DeviceID(app.staticPublic())
	r.open(t, app, dev, 3, code)
	r.hello(t, app, dev, 3)
	if g, ok := r.enroll.GrantFor(dev); !ok || g.Role != apiauth.RoleOwner {
		t.Fatalf("the first phone through the tunnel holds %+v", g)
	}
}

// One slot per device: a new session from the same device ends its older one
// and tells the phone, and a session of another device stays.
func TestOneSessionPerDevice(t *testing.T) {
	r := newTunRig(t)
	app := r.pair(t)
	other := r.pair(t)
	dev, otherDev := cloudwire.DeviceID(app.staticPublic()), cloudwire.DeviceID(other.staticPublic())
	r.open(t, other, otherDev, 1, nil)
	r.hello(t, other, otherDev, 1)
	r.open(t, app, dev, 1, nil)
	r.hello(t, app, dev, 1)

	again := &appClient{static: app.static}
	m1, err := again.message1(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.tunnel.Deliver(cloudwire.Tun{Dev: dev, Sid: 2, D: m1})
	reply := r.next(t, 3*time.Second)
	if reply.Sid != 2 || reply.Ended() {
		t.Fatalf("got %+v", reply)
	}
	if err := again.readMessage2(reply.D); err != nil {
		t.Fatal(err)
	}
	// The new session proves itself with its first message, and only then
	// takes the device's place.
	r.send(t, again, dev, 2, appproto.MsgHello, appproto.Hello{Proto: appproto.ProtoRange{Max: appproto.ProtoMax},
		App: appproto.AppInfo{Build: "test", UA: "ios"}})
	sawEnd := false
	for !sawEnd {
		m := r.next(t, 3*time.Second)
		sawEnd = m.Dev == dev && m.Sid == 1 && m.Ended()
	}
	if r.tunnel.count() != 2 {
		t.Fatalf("%d sessions, want the new one and the other device's", r.tunnel.count())
	}
}

// A message for a session the box does not hold gets an end, so the phone
// starts again at once instead of waiting.
func TestAnUnknownSessionGetsAnEnd(t *testing.T) {
	r := newTunRig(t)
	app := r.pair(t)
	dev := cloudwire.DeviceID(app.staticPublic())
	r.tunnel.Deliver(cloudwire.Tun{Dev: dev, Sid: 4, D: bytes.Repeat([]byte{1}, 600)})
	if m := r.next(t, 3*time.Second); m.Dev != dev || m.Sid != 4 || !m.Ended() {
		t.Fatalf("answer %+v", m)
	}
}

// The uplink ending ends every session, and a message queued from it opens
// nothing afterwards.
func TestDropEndsEverySession(t *testing.T) {
	r := newTunRig(t)
	app := r.pair(t)
	dev := cloudwire.DeviceID(app.staticPublic())
	r.open(t, app, dev, 1, nil)
	r.tunnel.Drop()
	if r.tunnel.count() != 0 {
		t.Fatal("a session outlived its uplink")
	}
	r.silent(t, 300*time.Millisecond)
}

// The phone ending its session, with an empty d, ends it at the box.
func TestAnEmptyDEndsTheSession(t *testing.T) {
	r := newTunRig(t)
	app := r.pair(t)
	dev := cloudwire.DeviceID(app.staticPublic())
	r.open(t, app, dev, 1, nil)
	r.tunnel.Deliver(cloudwire.Tun{Dev: dev, Sid: 1})
	deadline := time.Now().Add(3 * time.Second)
	for r.tunnel.count() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the phone's end left the session open")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Revoking a phone ends its tunnel session too.
func TestARevokeEndsATunnelSession(t *testing.T) {
	r := newTunRig(t)
	enrolOwner(t, r.enroll)
	app := r.pair(t)
	dev := cloudwire.DeviceID(app.staticPublic())
	r.open(t, app, dev, 1, nil)
	r.hello(t, app, dev, 1)
	removed, err := r.enroll.Revoke(dev, appenroll.AtTheBox)
	if err != nil {
		t.Fatal(err)
	}
	if n := r.tunnel.dropByAppKey(removed[0].Key); n != 1 || r.tunnel.count() != 0 {
		t.Fatalf("dropped %d, %d left", n, r.tunnel.count())
	}
}

// One device cannot spend the handshakes every other phone needs.
func TestOneDeviceCannotStarveTheOthersOfHandshakes(t *testing.T) {
	r := newTunRig(t)
	busy := r.pair(t)
	busyDev := cloudwire.DeviceID(busy.staticPublic())
	for i := range tunOpensPerDevice + 5 {
		again := &appClient{static: busy.static}
		m1, _ := again.message1(r.enroll.StaticKey().Public(), nil)
		r.tunnel.Deliver(cloudwire.Tun{Dev: busyDev, Sid: uint32(100 + i), D: m1})
	}
	// Drain what the busy device got: its answers, and the ends of the
	// sessions each new one replaced. Each answer costs X25519 work, which a
	// loaded CI runner does slowly, so wait for the budget to be spent and
	// then a while longer for any answer past it.
	answers := 0
	quiet := 500 * time.Millisecond
	deadline := time.After(10 * time.Second)
drain:
	for {
		wait := time.After(quiet)
		if answers < tunOpensPerDevice {
			wait = nil
		}
		select {
		case m := <-r.out.ch:
			if !m.Ended() {
				answers++
			}
		case <-wait:
			break drain
		case <-deadline:
			break drain
		}
	}
	// At most its budget. Handshakes run side by side, and a device holds at
	// most tunPendingPerDevice unproved sessions, so one evicted before it
	// answered is fair; what must never happen is an answer past the budget.
	if answers > tunOpensPerDevice || answers < tunOpensPerDevice-tunPendingPerDevice {
		t.Fatalf("the busy device got %d handshakes answered, want at most %d", answers, tunOpensPerDevice)
	}
	other := r.pair(t)
	r.open(t, other, cloudwire.DeviceID(other.staticPublic()), 1, nil)
}

// A message 1 replayed under a new sid, as the service could, gets an
// answer and nothing else: the live session goes on, and no stamp or code
// moves.
func TestAReplayedMessage1LeavesTheLiveSession(t *testing.T) {
	r := newTunRig(t)
	app := r.pair(t)
	dev := cloudwire.DeviceID(app.staticPublic())
	m1, err := app.message1(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.tunnel.Deliver(cloudwire.Tun{Dev: dev, Sid: 1, D: m1})
	reply := r.next(t, 3*time.Second)
	if err := app.readMessage2(reply.D); err != nil {
		t.Fatal(err)
	}
	r.hello(t, app, dev, 1)
	seen := r.enroll.Devices()[0].LastSeenMs

	// The replay, under a sid of the replayer's choosing.
	r.tunnel.Deliver(cloudwire.Tun{Dev: dev, Sid: 99, D: m1})
	if m := r.next(t, 3*time.Second); m.Sid != 99 || m.Ended() {
		t.Fatalf("the replay got %+v", m)
	}
	r.silent(t, 300*time.Millisecond)
	if r.tunnel.count() != 1 || r.enroll.Devices()[0].LastSeenMs != seen {
		t.Fatal("a replayed message 1 changed the live session or the stamp")
	}
	r.send(t, app, dev, 1, appproto.MsgCmd, setModeCmd("0192f2a0-7c1e-7000-8000-00000000d001"))
	r.receive(t, app, appproto.MsgCmdAck)
}

// A dropped tun does not end the session: the next command still gets its
// ack and its result.
func TestADroppedTunDoesNotEndTheSession(t *testing.T) {
	r := newTunRig(t)
	app := r.pair(t)
	dev := cloudwire.DeviceID(app.staticPublic())
	r.open(t, app, dev, 1, nil)
	r.hello(t, app, dev, 1)
	drops := &dropOnce{inner: r.out}
	r.tunnel.Attach(drops)
	r.send(t, app, dev, 1, appproto.MsgCmd, setModeCmd("0192f2a0-7c1e-7000-8000-00000000d101"))
	// The ack was dropped; the result comes.
	r.receive(t, app, appproto.MsgCmdResult)
	if r.tunnel.count() != 1 {
		t.Fatal("a dropped frame ended the session")
	}
	r.send(t, app, dev, 1, appproto.MsgCmd, setModeCmd("0192f2a0-7c1e-7000-8000-00000000d102"))
	r.receive(t, app, appproto.MsgCmdAck)
	r.receive(t, app, appproto.MsgCmdResult)
}

// dropOnce loses the first message, as a full queue would, and counts it.
type dropOnce struct {
	inner *capturedTun
	done  bool
}

type countedDrop struct{}

func (countedDrop) Error() string { return "dropped" }
func (countedDrop) Dropped() bool { return true }

func (d *dropOnce) SendTun(t cloudwire.Tun, bulk bool) error {
	if !d.done {
		d.done = true
		return countedDrop{}
	}
	return d.inner.SendTun(t, bulk)
}

// Revoking a phone through the uplink reaches its tunnel session.
func TestDropSessionsByAppKeyReachesTheTunnel(t *testing.T) {
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := New(Options{Enroll: enroll, Handler: newHandler, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	out := &capturedTun{ch: make(chan cloudwire.Tun, 16)}
	u.Tunnel().Attach(out)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.Tunnel().Run(ctx)
	enrolOwner(t, enroll)
	r := &tunRig{enroll: enroll, tunnel: u.Tunnel(), out: out}
	app := r.pair(t)
	dev := cloudwire.DeviceID(app.staticPublic())
	r.open(t, app, dev, 1, nil)
	r.hello(t, app, dev, 1)
	removed, err := enroll.Revoke(dev, appenroll.AtTheBox)
	if err != nil {
		t.Fatal(err)
	}
	if n := u.DropSessionsByAppKey(removed[0].Key); n != 1 {
		t.Fatalf("dropped %d", n)
	}
}

type heldAPI struct {
	started   chan struct{}
	cancelled chan struct{}
	once      sync.Once
}

func (h *heldAPI) Route(*http.Request) apiauth.RouteFacts {
	return apiauth.RouteFacts{Tier: apiauth.TierRead}
}

func (h *heldAPI) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	h.once.Do(func() { close(h.started) })
	<-req.Context().Done()
	close(h.cancelled)
	w.WriteHeader(http.StatusOK)
}

// Revoking a phone cancels its API request already running in Core.
func TestARevokedShareStopsACallAlreadyInFlight(t *testing.T) {
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	enrolOwner(t, enroll)
	gateway := &heldAPI{started: make(chan struct{}), cancelled: make(chan struct{})}
	build := func(sender appproto.Sender, caller apiauth.Caller, grants appproto.GrantReader) (*appproto.Handler, error) {
		return appproto.New(appproto.Config{
			Clock: appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
			Site:  stubSite{}, Info: stubInfo{}, Modes: stubModes{}, Plans: stubPlans{}, Codec: codec{},
			Sender: sender, Caller: caller, Grants: grants, API: gateway,
			Commands: appproto.NewCmdLog(), Caps: append(appproto.DefaultCaps(), appproto.CapApiPassthrough),
			SrcGrid: "meter", SrcPV: "meter", SrcBattery: "meter", Logger: slog.New(slog.DiscardHandler),
		})
	}
	u, err := New(Options{Enroll: enroll, Handler: build, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	out := &capturedTun{ch: make(chan cloudwire.Tun, 16)}
	u.Tunnel().Attach(out)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = u.Run(ctx) }()
	r := &tunRig{enroll: enroll, tunnel: u.Tunnel(), out: out}
	app := r.pair(t)
	dev := cloudwire.DeviceID(app.staticPublic())
	r.open(t, app, dev, 1, nil)
	r.hello(t, app, dev, 1)
	id := uint32(1)
	body, err := appproto.Marshal(appproto.APIReq{Method: appproto.APIGet, Path: "/api/logs"})
	if err != nil {
		t.Fatal(err)
	}
	frame, err := appwire.EncodeFrame(appwire.Frame{Lane: appwire.LaneBulk, Bucket: 16384,
		Envelope: appwire.Envelope{T: appproto.MsgAPIReq, ID: &id, B: body}})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := app.encrypt(frame)
	if err != nil {
		t.Fatal(err)
	}
	r.tunnel.Deliver(cloudwire.Tun{Dev: dev, Sid: 1, D: msg})
	select {
	case <-gateway.started:
	case <-time.After(3 * time.Second):
		t.Fatal("API request did not start")
	}
	removed, err := enroll.Revoke(dev, appenroll.AtTheBox)
	if err != nil {
		t.Fatal(err)
	}
	if n := u.DropSessionsByAppKey(removed[0].Key); n != 1 {
		t.Fatalf("dropped %d sessions", n)
	}
	select {
	case <-gateway.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("revoked session left its API request running")
	}
}
