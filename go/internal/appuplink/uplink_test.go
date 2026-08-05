package appuplink

import (
	"context"
	"encoding/hex"
	"log/slog"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appwire"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
)

// The handle derivation must agree with the app byte for byte, or the box and
// the phone sit in two different rooms and nobody's house appears on their
// screen. These are the outputs of srcfl/ftw-webapp
// src/lib/carrier/rendezvous.ts for a secret of 0x00..0x1f.
func TestHandleMatchesTheApp(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i)
	}

	for _, want := range []struct {
		epoch  int64
		handle string
	}{
		{0, "02ce440bd74a87c38abe28f5fe74549b"},
		{1, "4e81fbf74ecfb2db9c5c8bd63dcb3a85"},
		{481234, "0f8ae5eb475acc0e97863f504ce04027"},
		{999999999, "1ec8efa34c1aeeb9fec676ef4a6bcb5e"},
	} {
		got, err := Handle(secret, want.epoch)
		if err != nil {
			t.Fatalf("Handle(%d): %v", want.epoch, err)
		}
		if got != want.handle {
			t.Fatalf("epoch %d: handle = %s, want %s", want.epoch, got, want.handle)
		}
	}
}

// Two epochs' handles must be unrelated strings, or the rotation buys nothing.
func TestHandlesOfNeighbouringEpochsShareNothing(t *testing.T) {
	secret := make([]byte, 32)
	a, err := Handle(secret, 100)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	b, err := Handle(secret, 101)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if a == b {
		t.Fatal("the handle did not change across an epoch")
	}
	if len(a) != HandleBytes*2 {
		t.Fatalf("handle is %d characters, want %d", len(a), HandleBytes*2)
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("handle is not hex: %v", err)
	}
}

func TestHandleRefusesAShortSecret(t *testing.T) {
	if _, err := Handle(make([]byte, 8), 1); err == nil {
		t.Fatal("an eight-byte secret was accepted as a secret")
	}
}

// A Pi with a dead RTC reads 1970 or earlier. Go divides toward zero and the
// app uses Math.floor, so without the floor here the two would disagree on
// exactly the boxes that need the correction most.
func TestCurrentEpochFloors(t *testing.T) {
	for _, c := range []struct{ ms, epoch int64 }{
		{0, 0},
		{EpochMs - 1, 0},
		{EpochMs, 1},
		{-1, -1},
		{-EpochMs, -1},
		{-EpochMs - 1, -2},
	} {
		if got := CurrentEpoch(c.ms); got != c.epoch {
			t.Fatalf("CurrentEpoch(%d) = %d, want %d", c.ms, got, c.epoch)
		}
	}
}

// --------------------------------------------------------------------------
// End to end, against a relay and a phone
// --------------------------------------------------------------------------

type rig struct {
	relay  *fakeRelay
	enroll *appenroll.Identity
	uplink *Uplink
	cancel context.CancelFunc
	done   chan error
}

func newRig(t *testing.T, epoch int64) *rig {
	t.Helper()

	relay := newFakeRelay(epoch)
	t.Cleanup(relay.close)

	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	now := time.UnixMilli(epoch * EpochMs)
	uplink, err := New(Options{
		Endpoint: relay.url(),
		Enroll:   enroll,
		Handler:  func(s appproto.Sender) (*appproto.Handler, error) { return newHandler(s) },
		Logger:   slog.New(slog.DiscardHandler),
		Now:      func() time.Time { return now },
		Random:   func() float64 { return 0 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- uplink.Run(ctx) }()

	return &rig{relay: relay, enroll: enroll, uplink: uplink, cancel: cancel, done: done}
}

// dialApp joins the room as a browser stream would.
func (r *rig) dialApp(t *testing.T, epoch int64) *websocket.Conn {
	t.Helper()

	handle, err := Handle(r.enroll.RendezvousSecret(), epoch)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	conn, _, err := websocket.DefaultDialer.Dial(
		r.relay.url()+"/r/"+strconv.FormatInt(epoch, 10)+"/"+handle+"/app", nil)
	if err != nil {
		t.Fatalf("dialling as an app: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Wait for the room to hold both of us. A frame written into an empty
	// room is dropped by the relay, which would make this test flaky for a
	// reason that has nothing to do with the box.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("waiting for the room: %v", err)
		}
		if kind == websocket.TextMessage && string(message) == CtrlReady {
			return conn
		}
	}
}

// waitForBinary reads until a binary message arrives, so the relay's control
// words do not have to be interleaved into every assertion.
func waitForBinary(t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("reading from the relay: %v", err)
		}
		if kind == websocket.BinaryMessage {
			return message
		}
	}
}

// A paired phone gets a session, a hello_ok and a telemetry stream. This is
// the whole product in one test.
func TestAPairedAppGetsASessionAndTelemetry(t *testing.T) {
	r := newRig(t, 481234)

	code, _, err := r.enroll.MintPairingCode()
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}

	conn := r.dialApp(t, 481234)
	app, err := newAppClient()
	if err != nil {
		t.Fatalf("newAppClient: %v", err)
	}

	message1, err := app.message1(r.enroll.StaticKey().Public(), code)
	if err != nil {
		t.Fatalf("message1: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, message1); err != nil {
		t.Fatalf("sending message 1: %v", err)
	}
	if err := app.readMessage2(waitForBinary(t, conn)); err != nil {
		t.Fatalf("readMessage2: %v", err)
	}

	// hello, on the wire the box actually speaks.
	send(t, conn, app, appproto.MsgHello, 512, appproto.Hello{
		Proto: appproto.ProtoRange{Min: 0, Max: appproto.ProtoMax},
		App:   appproto.AppInfo{Build: "test", UA: "pwa"},
	})
	if got := receive(t, conn, app); got.Envelope.T != appproto.MsgHelloOK {
		t.Fatalf("first reply is %q, want %q", got.Envelope.T, appproto.MsgHelloOK)
	}

	send(t, conn, app, appproto.MsgSub, 512, appproto.Sub{Bucket: 512})
	snapFrame := receive(t, conn, app)
	if snapFrame.Envelope.T != appproto.MsgSnap {
		t.Fatalf("subscription reply is %q, want %q", snapFrame.Envelope.T, appproto.MsgSnap)
	}

	// The stream itself. Lane 0 is one bucket for the life of the session,
	// because a frame length that follows what happened in the house leaks
	// the household's load pattern through perfect encryption.
	for i := 0; i < 2; i++ {
		frame := receive(t, conn, app)
		if frame.Lane != appwire.LaneControl {
			t.Fatalf("telemetry frame %d is on lane %d, want %d", i, frame.Lane, appwire.LaneControl)
		}
		if frame.Bucket != 512 {
			t.Fatalf("telemetry frame %d is %d bytes, want 512", i, frame.Bucket)
		}
	}
}

// The pairing code is what the box checks. Without it a stranger who can reach
// the relay would get a session on somebody else's house.
func TestAnUnpairedAppGetsNoReply(t *testing.T) {
	r := newRig(t, 481234)

	conn := r.dialApp(t, 481234)
	app, err := newAppClient()
	if err != nil {
		t.Fatalf("newAppClient: %v", err)
	}
	message1, err := app.message1(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatalf("message1: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, message1); err != nil {
		t.Fatalf("sending message 1: %v", err)
	}

	// Silence, not an error frame. Replying at all would confirm that a box
	// is on this handle and hand a stranger a fresh ephemeral key.
	_ = conn.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
	for {
		kind, _, err := conn.ReadMessage()
		if err != nil {
			return // the deadline, which is the pass
		}
		if kind == websocket.BinaryMessage {
			t.Fatal("the box replied to a handshake with no pairing code")
		}
	}
}

func TestAWrongPairingCodeIsRefused(t *testing.T) {
	r := newRig(t, 481234)
	if _, _, err := r.enroll.MintPairingCode(); err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}

	conn := r.dialApp(t, 481234)
	app, err := newAppClient()
	if err != nil {
		t.Fatalf("newAppClient: %v", err)
	}
	wrong := make([]byte, appenroll.PairingCodeBytes)
	message1, err := app.message1(r.enroll.StaticKey().Public(), wrong)
	if err != nil {
		t.Fatalf("message1: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, message1); err != nil {
		t.Fatalf("sending message 1: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(750 * time.Millisecond))
	for {
		kind, _, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind == websocket.BinaryMessage {
			t.Fatal("the box replied to a handshake with the wrong pairing code")
		}
	}
}

// The box joins under the handle its own secret derives, not under anything
// the relay chose. If this ever became a constant the relay could correlate a
// household across years.
func TestTheBoxJoinsUnderTheDerivedHandle(t *testing.T) {
	r := newRig(t, 481234)

	select {
	case <-r.relay.joined:
	case <-time.After(5 * time.Second):
		t.Fatal("the box never joined")
	}

	want, err := Handle(r.enroll.RendezvousSecret(), 481234)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	handles := r.relay.seenHandles()
	if len(handles) == 0 || handles[0] != want {
		t.Fatalf("joined as %v, want %s", handles, want)
	}
}

// The relay is allowed to correct our clock by an hour, and not to steer us
// onto handles of its choosing.
func TestAnImplausibleEpochCorrectionIsIgnored(t *testing.T) {
	u := &Uplink{
		log:    slog.New(slog.DiscardHandler),
		now:    func() time.Time { return time.UnixMilli(100 * EpochMs) },
		random: func() float64 { return 0 },
	}

	u.adoptEpoch("101")
	if u.epochOffset != 1 {
		t.Fatalf("offset = %d, want 1", u.epochOffset)
	}

	u.adoptEpoch("50000")
	if u.epochOffset != 1 {
		t.Fatalf("a far-away epoch moved us to %d", u.epochOffset)
	}

	// The empty string parses as zero in more languages than it should. A
	// close with no reason must leave us where we are.
	u.adoptEpoch("")
	if u.epochOffset != 1 {
		t.Fatalf("an empty reason moved us to %d", u.epochOffset)
	}
	u.adoptEpoch(" 99 x")
	if u.epochOffset != 1 {
		t.Fatalf("a malformed reason moved us to %d", u.epochOffset)
	}
}

// A box closed at a rotation must not come straight back. The relay would
// otherwise watch one handle go quiet and another appear in the same
// millisecond, and link the two epochs by timing alone.
func TestRotationIsNotFollowedInstantly(t *testing.T) {
	relay := newFakeRelay(481234)
	defer relay.close()

	next := int64(481235)
	relay.rotateTo = &next

	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	now := time.UnixMilli(481234 * EpochMs)
	u, err := New(Options{
		Endpoint: relay.url(),
		Enroll:   enroll,
		Handler:  func(s appproto.Sender) (*appproto.Handler, error) { return newHandler(s) },
		Logger:   slog.New(slog.DiscardHandler),
		Now:      func() time.Time { return now },
		// The largest draw the jitter can make, so the assertion is about
		// the window rather than about luck.
		Random: func() float64 { return 1 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	delay, err := u.once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}
	if delay <= 0 {
		t.Fatal("the box would rejoin the instant the relay rotated it")
	}
	if delay > rotateJitter {
		t.Fatalf("delay = %v, longer than the whole window %v", delay, rotateJitter)
	}
	if u.epochOffset != 1 {
		t.Fatalf("offset = %d; the announced epoch was not adopted", u.epochOffset)
	}
}

// A box whose RTC reads 1970 must be right after one round trip, not after a
// backoff.
func TestAnEpochCorrectionRetriesImmediately(t *testing.T) {
	relay := newFakeRelay(481234)
	defer relay.close()

	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	// The box guesses one epoch behind; the relay corrects it.
	now := time.UnixMilli(481233 * EpochMs)
	u, err := New(Options{
		Endpoint: relay.url(),
		Enroll:   enroll,
		Handler:  func(s appproto.Sender) (*appproto.Handler, error) { return newHandler(s) },
		Logger:   slog.New(slog.DiscardHandler),
		Now:      func() time.Time { return now },
		Random:   func() float64 { return 1 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	delay, err := u.once(context.Background())
	if err != nil {
		t.Fatalf("once: %v", err)
	}
	if delay != 0 {
		t.Fatalf("delay = %v, want an immediate retry", delay)
	}
	if u.epochOffset != 1 {
		t.Fatalf("offset = %d, want 1", u.epochOffset)
	}
}

func TestARelayOriginWithAPathIsRefused(t *testing.T) {
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	handler := func(s appproto.Sender) (*appproto.Handler, error) { return newHandler(s) }

	for _, endpoint := range []string{
		"wss://relay.ftw.energy/r/1/abc/box",
		"https://relay.ftw.energy",
		"relay.ftw.energy",
	} {
		if _, err := New(Options{Endpoint: endpoint, Enroll: enroll, Handler: handler}); err == nil {
			t.Fatalf("%q was accepted as a relay origin", endpoint)
		}
	}
}

// --------------------------------------------------------------------------
// A box with nothing attached, so the tests above are about the uplink
// --------------------------------------------------------------------------

type stubSite struct{}

func (stubSite) Snapshot() appproto.Snapshot {
	return appproto.Snapshot{
		Mode:       control.ModeSelfConsumption,
		ControlRev: 1,
		GridW:      1200,
		// Negative while generating. FTW's convention is that positive watts
		// flow into the site, so PV is never positive.
		PVW:             -3000,
		BatteryW:        800,
		LoadW:           1000,
		BatterySoC:      0.55,
		BatterySoCKnown: true,
		Sources: []appproto.Source{
			{ID: "meter", Kind: "meter", Name: "P1", LastOkUptimeMs: 1000, StaleAfterMs: 5000},
		},
	}
}

type stubInfo struct{}

func (stubInfo) Identity() appproto.Identity {
	return appproto.Identity{ID: "box", Build: "test", TZ: "Europe/Stockholm"}
}
func (stubInfo) Boot() *appproto.BootProgress { return nil }

type stubModes struct{}

func (stubModes) SetMode(context.Context, control.Mode) error { return nil }
func (stubModes) ObservedMode() (control.Mode, bool)          { return control.ModeSelfConsumption, true }

type stubPlans struct{}

func (stubPlans) Latest() *mpc.Plan { return nil }
func (stubPlans) Rev() uint64       { return 0 }
func (stubPlans) CeilingW() *int64  { return nil }

func newHandler(s appproto.Sender) (*appproto.Handler, error) {
	return appproto.New(appproto.Config{
		Clock:      appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
		Site:       stubSite{},
		Info:       stubInfo{},
		Modes:      stubModes{},
		Plans:      stubPlans{},
		Codec:      codec{},
		Sender:     s,
		SrcGrid:    "meter",
		SrcPV:      "meter",
		SrcBattery: "meter",
		Logger:     slog.New(slog.DiscardHandler),
	})
}

func send(t *testing.T, conn *websocket.Conn, app *appClient, msgType string, bucket int, body any) {
	t.Helper()

	raw, err := appproto.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling %s: %v", msgType, err)
	}
	frame, err := appwire.EncodeFrame(appwire.Frame{
		Lane:     appwire.LaneControl,
		Bucket:   bucket,
		Envelope: appwire.Envelope{T: msgType, B: raw},
	})
	if err != nil {
		t.Fatalf("encoding %s: %v", msgType, err)
	}
	message, err := app.encrypt(frame)
	if err != nil {
		t.Fatalf("encrypting %s: %v", msgType, err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, message); err != nil {
		t.Fatalf("sending %s: %v", msgType, err)
	}
}

func receive(t *testing.T, conn *websocket.Conn, app *appClient) appwire.Frame {
	t.Helper()

	plaintext, err := app.decrypt(waitForBinary(t, conn))
	if err != nil {
		t.Fatalf("decrypting: %v", err)
	}
	frame, err := appwire.DecodeFrame(plaintext)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return frame
}
