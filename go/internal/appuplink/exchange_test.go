package appuplink

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/appwire"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// handlerWith builds every session's handler over one box with a charger:
// a phone's stream, or with exchange set a watch's exchange.
func handlerWith(exchange bool, charger appproto.Loadpoints, cmds *appproto.CmdLog) HandlerBuilder {
	return func(s appproto.Sender, caller apiauth.Caller, grants appproto.GrantReader) (*appproto.Handler, error) {
		return appproto.New(appproto.Config{
			Clock:      appproto.SystemClock{StartedAt: time.Now().Add(-time.Minute), Source: "ntp"},
			Site:       stubSite{},
			Info:       stubInfo{},
			Modes:      stubModes{},
			Plans:      stubPlans{},
			Loadpoints: charger,
			Codec:      codec{},
			Sender:     s,
			Caller:     caller,
			Grants:     grants,
			Commands:   cmds,
			Exchange:   exchange,
			SrcGrid:    "meter",
			SrcPV:      "meter",
			SrcBattery: "meter",
			Logger:     slog.New(slog.DiscardHandler),
		})
	}
}

// xrig is one box with an owner phone, a charger and the exchange table, and
// a clock the test moves.
type xrig struct {
	enroll  *appenroll.Identity
	x       *exchanges
	charger *fakeCharger
	cmds    *appproto.CmdLog
	phone   appenroll.Grant

	mu  sync.Mutex
	now time.Time
}

func newXRig(t *testing.T) *xrig {
	t.Helper()
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	return newXRigOn(t, enroll)
}

// newXRigOn is the rig on a box that exists already.
func newXRigOn(t *testing.T, enroll *appenroll.Identity) *xrig {
	t.Helper()
	code, _, err := enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	phoneKey, err := appwire.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	phone, err := enroll.Authorise(phoneKey.Public(), code)
	if err != nil {
		t.Fatal(err)
	}
	r := &xrig{enroll: enroll, charger: &fakeCharger{}, cmds: appproto.NewCmdLog(), phone: phone, now: time.Now()}
	r.x = &exchanges{
		build:  handlerWith(true, r.charger, r.cmds),
		enroll: enroll,
		static: enroll.StaticKey(),
		log:    slog.New(slog.DiscardHandler),
		now:    r.clock,
	}
	return r
}

func (r *xrig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *xrig) advance(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.now = r.now.Add(d)
}

// addWatch adds a watch by its key, as its phone does, and opens its first
// session.
func (r *xrig) addWatch(t *testing.T) (*watchClient, appproto.ExchangeHelloOK) {
	t.Helper()
	w := r.newWatch(t)
	return w, r.open(t, w, nil)
}

// newWatch adds a watch by its key and opens nothing.
func (r *xrig) newWatch(t *testing.T) *watchClient {
	t.Helper()
	w, err := newWatchClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.enroll.AddWatch(r.phone.DeviceID, w.static.Public()); err != nil {
		t.Fatal(err)
	}
	return w
}

// open runs X1 for a watch and fails unless the box answered.
func (r *xrig) open(t *testing.T, w *watchClient, code []byte) appproto.ExchangeHelloOK {
	t.Helper()
	message1, err := w.open(r.enroll.StaticKey().Public(), code)
	if err != nil {
		t.Fatal(err)
	}
	if len(message1) != exchangeOpenBytes {
		t.Fatalf("message 1 is %d bytes, want %d", len(message1), exchangeOpenBytes)
	}
	message2 := r.x.answer(context.Background(), w.dev(), message1)
	if message2 == nil {
		t.Fatal("the box did not answer the watch's message 1")
	}
	ok, _, err := w.opened(message2)
	if err != nil {
		t.Fatalf("reading message 2: %v", err)
	}
	return ok
}

// ask sends one request and returns the answer's frames, or nil for silence.
func (r *xrig) ask(t *testing.T, w *watchClient, msgType string, body any) []appwire.Frame {
	t.Helper()
	request, err := w.request(msgType, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(request) != exchangeRequestBytes {
		t.Fatalf("a %s request is %d bytes, want %d", msgType, len(request), exchangeRequestBytes)
	}
	reply := r.x.answer(context.Background(), w.dev(), request)
	if reply == nil {
		return nil
	}
	frames, err := w.answer(reply)
	if err != nil {
		t.Fatalf("opening the answer: %v", err)
	}
	return frames
}

func types(frames []appwire.Frame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Envelope.T)
	}
	return out
}

func decodeAs[T any](t *testing.T, f appwire.Frame) T {
	t.Helper()
	var v T
	if err := appproto.Unmarshal(f.Envelope.B, &v); err != nil {
		t.Fatalf("decoding %s: %v", f.Envelope.T, err)
	}
	return v
}

// --------------------------------------------------------------------------
// What a watch does
// --------------------------------------------------------------------------

func TestAWatchChargesThroughTheExchange(t *testing.T) {
	r := newXRig(t)
	w, hello := r.addWatch(t)

	if hello.Role != apiauth.RoleWatch || hello.Rev != 1 || hello.Mode != appproto.BoxModeFull {
		t.Fatalf("hello_ok = %+v", hello)
	}

	frames := r.ask(t, w, appproto.MsgCmd, chargeNow("watch-charge-1"))
	if got := types(frames); len(got) != 2 || got[0] != appproto.MsgCmdAck || got[1] != appproto.MsgCmdResult {
		t.Fatalf("the answer carries %v, want cmd.ack then cmd.result", got)
	}
	if res := decodeAs[appproto.CmdResult](t, frames[1]); res.State != appproto.CmdApplied {
		t.Fatalf("the charge ended %q (%+v)", res.State, res.Error)
	}
	if n := r.charger.count(); n != 1 {
		t.Fatalf("the charger saw %d holds", n)
	}
}

func TestAWatchReadsNowInOneFrame(t *testing.T) {
	r := newXRig(t)
	w, _ := r.addWatch(t)

	frames := r.ask(t, w, appproto.MsgNow, struct{}{})
	if got := types(frames); len(got) != 2 || got[0] != appproto.MsgNow || got[1] != appproto.MsgTick {
		t.Fatalf("the answer carries %v, want now then tick", got)
	}
	if frames[0].Lane != appwire.LaneControl || frames[0].Bucket != exchangeFrameBytes || frames[0].Truncated() {
		t.Fatalf("now is lane %d, %d B, trunc %v", frames[0].Lane, frames[0].Bucket, frames[0].Truncated())
	}
	now := decodeAs[appproto.Now](t, frames[0])
	if now.ControlRev != 1 || now.Fields["2"] != 1200 || now.Fields["3"] != -3000 {
		t.Fatalf("now = %+v", now)
	}
	if src, ok := now.Sources["7"]; !ok || src.AgeMs < 0 {
		t.Fatalf("the grid source is %+v", now.Sources)
	}
	if len(now.Loadpoints) != 1 {
		t.Fatalf("loadpoints = %+v", now.Loadpoints)
	}
	lp := now.Loadpoints[0]
	if lp.ID != "lp1" || !lp.Plugged || lp.SocPermille == nil || *lp.SocPermille != 620 || lp.PowerW != 7360 {
		t.Fatalf("the charger reads %+v", lp)
	}
}

// A watch's session takes cmd and now. The stream's other doors answer
// E_UNKNOWN_OP, and the site mode answers E_SCOPE_DENIED.
func TestTheExchangeTakesCmdAndNowAndNothingElse(t *testing.T) {
	r := newXRig(t)
	id := uint32(7)
	for _, c := range []struct {
		t    string
		body any
	}{
		{appproto.MsgSub, appproto.Sub{Bucket: 512, Hz: 1}},
		{appproto.MsgAPIReq, appproto.APIReq{Method: appproto.APIGet, Path: "/api/logs"}},
		{appproto.MsgHistQuery, appproto.HistQuery{}},
		{appproto.MsgPlanGet, struct{}{}},
		{appproto.MsgPriceGet, appproto.PriceGet{}},
	} {
		w, _ := r.addWatch(t)
		frame, err := lane0(c.t, &id, c.body)
		if err != nil {
			t.Fatal(err)
		}
		request, err := w.transport.Encrypt(frame)
		if err != nil {
			t.Fatal(err)
		}
		frames, err := w.answer(r.x.answer(context.Background(), w.dev(), request))
		if err != nil {
			t.Fatalf("%s: %v", c.t, err)
		}
		if len(frames) != 2 || frames[0].Envelope.T != appproto.MsgError {
			t.Fatalf("%s answered %v", c.t, types(frames))
		}
		if e := decodeAs[appproto.ErrorBody](t, frames[0]); e.Code != appproto.ErrUnknownOp {
			t.Fatalf("%s answered %s", c.t, e.Code)
		}
	}

	w, _ := r.addWatch(t)
	frames := r.ask(t, w, appproto.MsgCmd, appproto.Cmd{
		CmdID: "watch-mode", Op: appproto.OpSetMode, Args: map[string]any{"mode": "charge"},
		NotValidAfterMs: 10 * 60 * 1000, Expect: appproto.Expect{Rev: 1},
	})
	// The exchange takes the charger's hold alone (ADR 0009, decision B):
	// the mode is refused before the command runs.
	if e := decodeAs[appproto.ErrorBody](t, frames[0]); frames[0].Envelope.T != appproto.MsgError || e.Code != protocol.ErrOpNotGranted {
		t.Fatalf("site.mode.set from a watch answered %v %+v", types(frames), e)
	}
}

// Shape: every request is one length and every answer another, whatever the
// op and the outcome, so FTW Cloud cannot tell a read from a command or a
// refusal from a success.
func TestEveryExchangeHasOneLength(t *testing.T) {
	r := newXRig(t)
	cases := []struct {
		name string
		t    string
		body any
	}{
		{"applied", appproto.MsgCmd, chargeNow("shape-1")},
		{"asked again", appproto.MsgCmd, chargeNow("shape-1")},
		{"not granted", appproto.MsgCmd, appproto.Cmd{CmdID: "shape-2", Op: appproto.OpSetMode,
			Args: map[string]any{"mode": "charge"}, NotValidAfterMs: 600_000, Expect: appproto.Expect{Rev: 1}}},
		{"expired", appproto.MsgCmd, appproto.Cmd{CmdID: "shape-3", Op: appproto.OpLoadpointHold,
			Args: map[string]any{"id": "lp1"}, NotValidAfterMs: 1, Expect: appproto.Expect{Rev: 1}}},
		{"conflict", appproto.MsgCmd, appproto.Cmd{CmdID: "shape-4", Op: appproto.OpLoadpointHold,
			Args: map[string]any{"id": "lp1", "power_w": 0}, NotValidAfterMs: 600_000, Expect: appproto.Expect{Rev: 9}}},
		{"now", appproto.MsgNow, struct{}{}},
		{"unknown", appproto.MsgPlanGet, struct{}{}},
	}
	want := exchangeAnswerFrames * exchangeRequestBytes
	for _, c := range cases {
		w, _ := r.addWatch(t)
		request, err := w.request(c.t, c.body)
		if err != nil {
			t.Fatal(err)
		}
		reply := r.x.answer(context.Background(), w.dev(), request)
		if len(request) != exchangeRequestBytes || len(reply) != want {
			t.Fatalf("%s: request %d B, answer %d B; want %d and %d",
				c.name, len(request), len(reply), exchangeRequestBytes, want)
		}
	}
}

// --------------------------------------------------------------------------
// Settling, replay and both paths
// --------------------------------------------------------------------------

// A result that has not settled when the answer goes leaves a tick in its
// place. Asking again with the same cmdId returns it, and the charger acts
// once.
func TestAnUnsettledResultIsAskedForAgain(t *testing.T) {
	r := newXRig(t)
	r.x.settle = 50 * time.Millisecond
	r.charger.slow = 300 * time.Millisecond
	w, _ := r.addWatch(t)

	frames := r.ask(t, w, appproto.MsgCmd, chargeNow("slow-1"))
	if got := types(frames); len(got) != 2 || got[0] != appproto.MsgCmdAck || got[1] != appproto.MsgTick {
		t.Fatalf("the first answer carries %v, want cmd.ack then tick", got)
	}

	// The watch asks again a moment later, as its receipt line says.
	time.Sleep(r.charger.slow)
	frames = r.ask(t, w, appproto.MsgCmd, chargeNow("slow-1"))
	if got := types(frames); len(got) != 2 || got[0] != appproto.MsgCmdAck || got[1] != appproto.MsgCmdResult {
		t.Fatalf("asking again carries %v, want the stored ack and result", got)
	}
	if n := r.charger.count(); n != 1 {
		t.Fatalf("the charger saw %d holds for one intent", n)
	}
}

// A recorded message 1 draws a fresh message 2, under a new box ephemeral key
// that only the real watch can use. A recorded command fails in its own
// session, and the same command in a fresh session returns the stored
// outcome.
func TestReplayGetsNothingNew(t *testing.T) {
	r := newXRig(t)
	w := r.newWatch(t)
	message1, err := w.open(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	first := r.x.answer(context.Background(), w.dev(), message1)
	if _, _, err := w.opened(first); err != nil {
		t.Fatal(err)
	}

	again := r.x.answer(context.Background(), w.dev(), message1)
	if again == nil || bytes.Equal(first[:appwire.DHBytes], again[:appwire.DHBytes]) {
		t.Fatal("a replayed message 1 drew the same box ephemeral key")
	}
	// An eavesdropper holds message 1 and the new message 2 but neither of
	// the watch's private keys. Without them nothing opens: a reader that
	// tries with another ephemeral fails.
	thief, err := newWatchClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := thief.open(r.enroll.StaticKey().Public(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := thief.opened(again); err == nil {
		t.Fatal("someone without the watch's keys read the replayed message 2")
	}

	request, err := w.request(appproto.MsgCmd, chargeNow("replay-1"))
	if err != nil {
		t.Fatal(err)
	}
	firstAnswer, err := w.answer(r.x.answer(context.Background(), w.dev(), request))
	if err != nil {
		t.Fatal(err)
	}
	if reply := r.x.answer(context.Background(), w.dev(), request); reply != nil {
		t.Fatal("a recorded command was answered in its own session")
	}

	r.open(t, w, nil)
	frames := r.ask(t, w, appproto.MsgCmd, chargeNow("replay-1"))
	if decodeAs[appproto.CmdAck](t, frames[0]).LeaseID != decodeAs[appproto.CmdAck](t, firstAnswer[0]).LeaseID {
		t.Fatal("the same command in a fresh session did not return the stored outcome")
	}
	if n := r.charger.count(); n != 1 {
		t.Fatalf("the charger saw %d holds", n)
	}
}

// Both paths: one cmdId sent by the watch and by its phone's stream acts once.
func TestOneIntentByBothPathsActsOnce(t *testing.T) {
	r := newXRig(t)
	w, _ := r.addWatch(t)
	frames := r.ask(t, w, appproto.MsgCmd, chargeNow("both-paths"))
	lease := decodeAs[appproto.CmdAck](t, frames[0]).LeaseID

	sent := &capturedFrames{}
	phone, err := handlerWith(false, r.charger, r.cmds)(sent, apiauth.Caller{
		Subject: subjectOf(r.phone.DeviceID),
		Kind:    apiauth.KindApp,
		Role:    r.phone.Role,
		Scopes:  appproto.ScopesForGrant(r.phone.Role, ""),
		Epoch:   r.phone.Epoch,
	}, liveGrant{enroll: r.enroll, deviceID: r.phone.DeviceID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(phone.Close)
	frame, err := lane0(appproto.MsgCmd, nil, chargeNow("both-paths"))
	if err != nil {
		t.Fatal(err)
	}
	if err := phone.Handle(context.Background(), frame); err != nil {
		t.Fatal(err)
	}

	if n := r.charger.count(); n != 1 {
		t.Fatalf("the charger saw %d holds for one intent sent twice", n)
	}
	if got := sent.ack(t).LeaseID; got != lease {
		t.Fatalf("the phone got lease %q, the watch %q", got, lease)
	}
}

// capturedFrames is a stream session's sender, kept for assertions.
type capturedFrames struct {
	mu     sync.Mutex
	frames []appwire.Frame
}

func (c *capturedFrames) Send(raw []byte) error {
	f, err := appwire.DecodeFrame(raw)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, f)
	return nil
}

func (c *capturedFrames) ack(t *testing.T) appproto.CmdAck {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.frames {
		if f.Envelope.T == appproto.MsgCmdAck {
			return decodeAs[appproto.CmdAck](t, f)
		}
	}
	t.Fatal("the phone got no ack")
	return appproto.CmdAck{}
}

// --------------------------------------------------------------------------
// Who gets an answer, and for how long
// --------------------------------------------------------------------------

// A stranger, a phone and a removed watch get silence, as on the uplink.
func TestOnlyAPairedWatchIsAnswered(t *testing.T) {
	r := newXRig(t)

	stranger, err := newWatchClient()
	if err != nil {
		t.Fatal(err)
	}
	message1, err := stranger.open(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.x.answer(context.Background(), stranger.dev(), message1) != nil {
		t.Fatal("a stranger was answered")
	}

	// A message 1 sealed under the stream's prologue is not an exchange
	// handshake, even from a paired watch: one can never stand in for the
	// other.
	w, _ := r.addWatch(t)
	box := r.enroll.StaticKey().Public()
	streamHandshake, err := appwire.NewInitiator(appwire.InitiatorOptions{
		StaticKey: w.static, RemoteStatic: box, Prologue: prologue(box),
	})
	if err != nil {
		t.Fatal(err)
	}
	hello, err := lane0(appproto.MsgHello, nil, appproto.Hello{Proto: appproto.ProtoRange{Max: appproto.ProtoMax}})
	if err != nil {
		t.Fatal(err)
	}
	streamOpen, err := streamHandshake.WriteMessage(hello)
	if err != nil {
		t.Fatal(err)
	}
	if r.x.answer(context.Background(), w.dev(), streamOpen) != nil {
		t.Fatal("a stream handshake opened an exchange session")
	}

	// And a phone's key on the watch's door.
	phone, err := newWatchClient()
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := r.enroll.MintPairingCode(apiauth.RoleViewer, appenroll.InviteTTL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.enroll.Authorise(phone.static.Public(), code); err != nil {
		t.Fatal(err)
	}
	message1, err = phone.open(box, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.x.answer(context.Background(), phone.dev(), message1) != nil {
		t.Fatal("a phone opened an exchange session")
	}
}

// Removing the watch, or its phone, ends its sessions and silences its next
// message 1.
func TestRemovalEndsAWatchsSessions(t *testing.T) {
	for _, removePhone := range []bool{false, true} {
		r := newXRig(t)
		w, _ := r.addWatch(t)
		target := w.dev()
		if removePhone {
			target = r.phone.DeviceID
		}

		removed, err := r.enroll.Revoke(target, appenroll.AtTheBox)
		if err != nil {
			t.Fatal(err)
		}
		dropped := 0
		for _, gone := range removed {
			dropped += r.x.dropByAppKey(gone.Key)
		}
		if dropped != 1 {
			t.Fatalf("removing %s dropped %d watch sessions", target, dropped)
		}
		if frames := r.ask(t, w, appproto.MsgNow, struct{}{}); frames != nil {
			t.Fatal("a removed watch's session still answered")
		}
		message1, err := w.open(r.enroll.StaticKey().Public(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if r.x.answer(context.Background(), w.dev(), message1) != nil {
			t.Fatal("a removed watch's message 1 was answered")
		}
	}
}

// A session ends after four exchanges, a minute without one, or three minutes.
func TestASessionIsShort(t *testing.T) {
	r := newXRig(t)
	w, _ := r.addWatch(t)
	for i := 0; i < exchangeUses; i++ {
		if r.ask(t, w, appproto.MsgNow, struct{}{}) == nil {
			t.Fatalf("exchange %d was not answered", i+1)
		}
	}
	if r.ask(t, w, appproto.MsgNow, struct{}{}) != nil {
		t.Fatal("a fifth exchange was answered")
	}

	r.open(t, w, nil)
	r.advance(exchangeIdle + time.Second)
	if r.ask(t, w, appproto.MsgNow, struct{}{}) != nil {
		t.Fatal("a session idle past a minute was answered")
	}

	r.open(t, w, nil)
	for i := 0; i < 3; i++ {
		r.advance(50 * time.Second)
		if r.ask(t, w, appproto.MsgNow, struct{}{}) == nil {
			t.Fatalf("exchange %d inside the lifetime was not answered", i+1)
		}
	}
	r.advance(40 * time.Second)
	if r.ask(t, w, appproto.MsgNow, struct{}{}) != nil {
		t.Fatal("a session past three minutes was answered")
	}
}

// At most twelve handshakes a minute from one watch and sixty in all; the
// rest are dropped unanswered. A device FTW Cloud names that is no watch of
// this box spends nothing, not even a Diffie-Hellman, and one watch's opens
// never spend another's.
func TestHandshakesAreRationed(t *testing.T) {
	r := newXRig(t)
	junk := make([]byte, exchangeOpenBytes)
	for i := 0; i < 2*exchangeOpensPerMinute; i++ {
		junk[0] = byte(i)
		r.x.answer(context.Background(), "nobody00", junk)
	}

	w := r.newWatch(t)
	message1, err := w.open(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < exchangeOpensPerWatch; i++ {
		if r.x.answer(context.Background(), w.dev(), message1) == nil {
			t.Fatalf("open %d of a watch's twelve was not answered after a stranger's junk", i+1)
		}
	}
	if r.x.answer(context.Background(), w.dev(), message1) != nil {
		t.Fatal("a watch's thirteenth handshake in a minute was answered")
	}
	// Another watch has a share of its own.
	r.addWatch(t)

	// Sixty in all: four more watches spend their twelve each.
	for k := 0; k < 3; k++ {
		other := r.newWatch(t)
		m1, err := other.open(r.enroll.StaticKey().Public(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < exchangeOpensPerWatch; i++ {
			r.x.answer(context.Background(), other.dev(), m1)
		}
	}
	late := r.newWatch(t)
	m1, err := late.open(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < exchangeOpensPerWatch; i++ {
		r.x.answer(context.Background(), late.dev(), m1)
	}
	if r.x.answer(context.Background(), late.dev(), m1) != nil {
		t.Fatal("a handshake past sixty in a minute was answered")
	}
	r.advance(time.Minute)
	if r.x.answer(context.Background(), w.dev(), message1) == nil {
		t.Fatal("a handshake a minute later was not answered")
	}
}

// A replayed message 1 opens a session that never proves itself: it stamps
// no visit, and replays end only one another, never the watch's working
// session and never another watch's.
func TestReplaysEndNoWorkingSession(t *testing.T) {
	r := newXRig(t)
	other, _ := r.addWatch(t)
	if r.ask(t, other, appproto.MsgNow, struct{}{}) == nil {
		t.Fatal("the other watch was not answered")
	}

	w := r.newWatch(t)
	message1, err := w.open(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	message2 := r.x.answer(context.Background(), w.dev(), message1)
	if _, _, err := w.opened(message2); err != nil {
		t.Fatal(err)
	}
	if r.ask(t, w, appproto.MsgNow, struct{}{}) == nil {
		t.Fatal("the watch's own session was not answered")
	}
	seen := func(dev string) int64 {
		for _, d := range r.enroll.Devices() {
			if d.ID == dev {
				return d.LastSeenMs
			}
		}
		return -1
	}
	stamped := seen(w.dev())

	r.advance(time.Second)
	for i := 0; i < MaxExchangeSessions; i++ {
		if r.x.answer(context.Background(), w.dev(), message1) == nil {
			t.Fatalf("replay %d drew no answer", i+1)
		}
	}
	if got := seen(w.dev()); got != stamped {
		t.Fatalf("replays stamped the watch's visit: %d, was %d", got, stamped)
	}
	if n := count(r.x.live, func(s *exchangeSession) bool { return s.dev == w.dev() }); n != MaxExchangeSessionsPerWatch {
		t.Fatalf("the watch holds %d sessions, want %d", n, MaxExchangeSessionsPerWatch)
	}
	if r.ask(t, w, appproto.MsgNow, struct{}{}) == nil {
		t.Fatal("replays ended the watch's working session")
	}
	if r.ask(t, other, appproto.MsgNow, struct{}{}) == nil {
		t.Fatal("one watch's replays ended another watch's session")
	}
}

// Removed mid-session, a watch's next command is refused and reaches no
// charger, even before anything ends its session.
func TestARevokeMidSessionRefusesTheNextCommand(t *testing.T) {
	r := newXRig(t)
	w, _ := r.addWatch(t)
	if _, err := r.enroll.Revoke(w.dev(), appenroll.AtTheBox); err != nil {
		t.Fatal(err)
	}
	frames := r.ask(t, w, appproto.MsgCmd, chargeNow("after-revoke"))
	if frames != nil {
		for _, f := range frames {
			if f.Envelope.T == appproto.MsgCmdResult && decodeAs[appproto.CmdResult](t, f).State == appproto.CmdApplied {
				t.Fatal("a removed watch's command was applied")
			}
		}
	}
	if r.charger.count() != 0 {
		t.Fatalf("a removed watch's command reached the charger %d times", r.charger.count())
	}
}

// A box that restarted has forgotten the commands it answered (protocol-cloud
// .md, "Exchange"). Its hello_ok says so: the uptime starts over, so the watch
// can tell a retry would reach a box that would act again.
func TestARestartedBoxSaysSoInItsClock(t *testing.T) {
	r := newXRig(t)
	_, before := r.addWatch(t)
	restarted := newXRigOn(t, r.enroll)
	restarted.x.build = func(sn appproto.Sender, caller apiauth.Caller, grants appproto.GrantReader) (*appproto.Handler, error) {
		return appproto.New(appproto.Config{
			Clock: appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"}, Site: stubSite{}, Info: stubInfo{},
			Modes: stubModes{}, Plans: stubPlans{}, Loadpoints: restarted.charger, Codec: codec{}, Sender: sn,
			Caller: caller, Grants: grants, Commands: appproto.NewCmdLog(), Exchange: true,
			SrcGrid: "meter", SrcPV: "meter", SrcBattery: "meter", Logger: slog.New(slog.DiscardHandler),
		})
	}
	_, after := restarted.addWatch(t)
	if after.Clock.UptimeMs >= before.Clock.UptimeMs {
		t.Fatalf("a restarted box's hello_ok says uptime %d, the first said %d", after.Clock.UptimeMs, before.Clock.UptimeMs)
	}
}

// Isolation: eight watch sessions leave the phones' sessions running, and a
// ninth watch ends the oldest watch's session, never a phone's.
func TestWatchesNeverTakeAPhonesPlace(t *testing.T) {
	const phones = 4
	tr := newTunRig(t)
	r := newXRigOn(t, tr.enroll)
	for i := 0; i < phones; i++ {
		app := tr.pair(t)
		dev := cloudwire.DeviceID(app.staticPublic())
		tr.open(t, app, dev, uint32(i+1), nil)
		tr.hello(t, app, dev, uint32(i+1))
	}
	if tr.tunnel.count() != phones {
		t.Fatalf("%d phone sessions, want %d", tr.tunnel.count(), phones)
	}

	first, _ := r.addWatch(t)
	for i := 1; i < MaxExchangeSessions; i++ {
		r.addWatch(t)
	}
	if tr.tunnel.count() != phones || r.x.count() != MaxExchangeSessions {
		t.Fatalf("%d phone and %d watch sessions, want %d and %d",
			tr.tunnel.count(), r.x.count(), phones, MaxExchangeSessions)
	}

	r.addWatch(t)
	if tr.tunnel.count() != phones {
		t.Fatalf("a ninth watch left %d phone sessions", tr.tunnel.count())
	}
	if r.ask(t, first, appproto.MsgNow, struct{}{}) != nil {
		t.Fatal("the oldest watch session outlived a ninth")
	}
}

// A request FTW Cloud names under another device gets silence, whether it
// opens a session or rides one: a service cannot spend one watch's requests
// on another.
func TestAnotherDevicesNameGetsSilence(t *testing.T) {
	r := newXRig(t)
	w, _ := r.addWatch(t)
	other, _ := r.addWatch(t)

	message1, err := w.open(r.enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.x.answer(context.Background(), other.dev(), message1) != nil {
		t.Fatal("a handshake was answered under another watch's id")
	}

	r.open(t, w, nil)
	request, err := w.request(appproto.MsgNow, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if r.x.answer(context.Background(), other.dev(), request) != nil {
		t.Fatal("a request was answered under another watch's id")
	}
	if frames := r.ask(t, w, appproto.MsgNow, struct{}{}); frames == nil {
		t.Fatal("the watch's own session died of a request under another id")
	}
}

// --------------------------------------------------------------------------
// The carrier, as FTW Cloud drives it
// --------------------------------------------------------------------------

// capturedX is the box's link to FTW Cloud, as the exchange sees it.
// closed says the link's share for x is spent.
type capturedX struct {
	ch     chan cloudwire.X
	closed bool
}

func (c *capturedX) XAllowed() bool { return !c.closed }

func (c *capturedX) SendX(m cloudwire.X) error {
	select {
	case c.ch <- m:
		return nil
	default:
		return errTunGone
	}
}

func (c *capturedX) next(t *testing.T) cloudwire.X {
	t.Helper()
	select {
	case m := <-c.ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("the box sent no x")
		return cloudwire.X{}
	}
}

// A watch its phone added by its key charges through the uplink's exchange:
// each request comes as x {dev, id, d} and each answer goes back as x with
// the same dev and id.
func TestAWatchChargesThroughFTWCloud(t *testing.T) {
	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	charger := &fakeCharger{}
	cmds := appproto.NewCmdLog()
	uplink, err := New(Options{
		Enroll:          enroll,
		Handler:         handlerWith(false, charger, cmds),
		ExchangeHandler: handlerWith(true, charger, cmds),
		Logger:          slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if uplink.exchange.table.cmdFloor != ExchangeCmdFloor {
		t.Fatalf("the uplink holds commands for %s, want %s", uplink.exchange.table.cmdFloor, ExchangeCmdFloor)
	}
	out := &capturedX{ch: make(chan cloudwire.X, 16)}
	uplink.Exchange().Attach(out)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = uplink.Run(ctx) }()

	code, _, err := enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	phoneKey, err := appwire.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	phone, err := enroll.Authorise(phoneKey.Public(), code)
	if err != nil {
		t.Fatal(err)
	}
	w, err := newWatchClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enroll.AddWatch(phone.DeviceID, w.static.Public()); err != nil {
		t.Fatal(err)
	}

	message1, err := w.open(enroll.StaticKey().Public(), nil)
	if err != nil {
		t.Fatal(err)
	}
	uplink.Exchange().Deliver(cloudwire.X{Dev: w.dev(), ID: 7, D: message1})
	got := out.next(t)
	if got.Dev != w.dev() || got.ID != 7 || len(got.D) == 0 {
		t.Fatalf("the handshake's answer is %+v", got)
	}
	if _, _, err := w.opened(got.D); err != nil {
		t.Fatal(err)
	}

	request, err := w.request(appproto.MsgCmd, chargeNow("cloud-1"))
	if err != nil {
		t.Fatal(err)
	}
	uplink.Exchange().Deliver(cloudwire.X{Dev: w.dev(), ID: 8, D: request})
	got = out.next(t)
	if got.ID != 8 {
		t.Fatalf("the command's answer came as %+v", got)
	}
	frames, err := w.answer(got.D)
	if err != nil {
		t.Fatal(err)
	}
	if kinds := types(frames); len(kinds) != 2 || kinds[0] != appproto.MsgCmdAck || kinds[1] != appproto.MsgCmdResult {
		t.Fatalf("the answer carries %v", kinds)
	}
	if charger.count() != 1 {
		t.Fatalf("the charger saw %d holds", charger.count())
	}

	// Silence is an empty d under the request's own number, so the service
	// answers the watch at once.
	again, err := w.request(appproto.MsgNow, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	uplink.Exchange().Deliver(cloudwire.X{Dev: "someone", ID: 9, D: again})
	if got = out.next(t); got.ID != 9 || got.Dev != "someone" || len(got.D) != 0 {
		t.Fatalf("a request under another id got %+v", got)
	}

	// Removing the watch through the uplink ends its exchange session too.
	if dropped := uplink.DropSessionsByAppKey(w.static.Public()); dropped != 1 {
		t.Fatalf("DropSessionsByAppKey dropped %d sessions, want the watch's one", dropped)
	}
}

// A box with more requests than it answers at once answers the rest with
// silence at once, rather than late.
func TestABusyExchangeAnswersSilenceAtOnce(t *testing.T) {
	r := newXRig(t)
	e := newExchange(r.x, slog.New(slog.DiscardHandler))
	out := &capturedX{ch: make(chan cloudwire.X, 2*exchangeInbox)}
	e.Attach(out)
	// Nothing runs the inbox, so it fills, and the one past it is answered.
	for i := 0; i < exchangeInbox; i++ {
		e.Deliver(cloudwire.X{Dev: "w", ID: uint32(i), D: []byte{1}})
	}
	e.Deliver(cloudwire.X{Dev: "w", ID: 99, D: []byte{1}})
	if got := out.next(t); got.ID != 99 || len(got.D) != 0 {
		t.Fatalf("the request past the inbox got %+v", got)
	}
}

// --------------------------------------------------------------------------
// The floor (decision F)
// --------------------------------------------------------------------------

// timed sends one request and returns how long the box took to answer it.
func (r *xrig) timed(t *testing.T, w *watchClient, msgType string, body any) ([]appwire.Frame, time.Duration) {
	t.Helper()
	request, err := w.request(msgType, body)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	reply := r.x.answer(context.Background(), w.dev(), request)
	took := time.Since(started)
	frames, err := w.answer(reply)
	if err != nil {
		t.Fatal(err)
	}
	return frames, took
}

// A command answers no sooner than the floor, applied or refused, so the
// time to answer does not say which. One that takes longer answers when it
// is done and waits no more. A read is not a command and is not held.
func TestACommandAnswersNoSoonerThanTheFloor(t *testing.T) {
	r := newXRig(t)
	r.x.cmdFloor = ExchangeCmdFloor
	slack := 300 * time.Millisecond

	w, _ := r.addWatch(t)
	frames, applied := r.timed(t, w, appproto.MsgCmd, chargeNow("floor-applied"))
	if decodeAs[appproto.CmdResult](t, frames[1]).State != appproto.CmdApplied {
		t.Fatalf("the charge answered %v", types(frames))
	}
	_, refused := r.timed(t, w, appproto.MsgCmd, appproto.Cmd{CmdID: "floor-refused", Op: appproto.OpSetMode,
		Args: map[string]any{"mode": "charge"}, NotValidAfterMs: 600_000, Expect: appproto.Expect{Rev: 1}})
	_, read := r.timed(t, w, appproto.MsgNow, struct{}{})

	for name, took := range map[string]time.Duration{"applied": applied, "refused": refused} {
		if took < ExchangeCmdFloor || took > ExchangeCmdFloor+slack {
			t.Fatalf("an %s command answered after %s, want %s", name, took, ExchangeCmdFloor)
		}
	}
	if read >= ExchangeCmdFloor {
		t.Fatalf("a read was held %s", read)
	}

	// An EV save longer than the floor sets the time alone.
	r.charger.mu.Lock()
	r.charger.save = 600 * time.Millisecond
	r.charger.mu.Unlock()
	w, _ = r.addWatch(t)
	_, slow := r.timed(t, w, appproto.MsgCmd, chargeNow("floor-slow"))
	if slow < 600*time.Millisecond || slow > 600*time.Millisecond+slack {
		t.Fatalf("a charge with a 600 ms save answered after %s; the floor must not add to it", slow)
	}
}

// With the link's share for x spent, a request runs nothing: no answer
// could tell the watch what happened, so the command does not happen.
func TestNoAnswerMeansNoCommand(t *testing.T) {
	r := newXRig(t)
	w, _ := r.addWatch(t)
	e := newExchange(r.x, slog.New(slog.DiscardHandler))
	out := &capturedX{ch: make(chan cloudwire.X, 4), closed: true}
	e.Attach(out)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.Run(ctx)

	request, err := w.request(appproto.MsgCmd, chargeNow("budget-spent"))
	if err != nil {
		t.Fatal(err)
	}
	e.Deliver(cloudwire.X{Dev: w.dev(), ID: 1, D: request})
	select {
	case got := <-out.ch:
		t.Fatalf("the box answered %+v with its share spent", got)
	case <-time.After(1500 * time.Millisecond):
	}
	if r.charger.count() != 0 {
		t.Fatal("a command ran whose answer could not leave")
	}
}

// Past the minute's requests the box drops the rest unanswered and runs
// none of them.
func TestTheExchangeTakesAMinutesRequestsAndNoMore(t *testing.T) {
	r := newXRig(t)
	e := newExchange(r.x, slog.New(slog.DiscardHandler))
	e.Attach(&capturedX{ch: make(chan cloudwire.X, 1)})
	for i := 0; i < exchangeRequestsPerMinute; i++ {
		if !e.take() {
			t.Fatalf("request %d of the minute was refused", i+1)
		}
	}
	if e.take() {
		t.Fatal("a request past the minute's was taken")
	}
	r.advance(time.Minute)
	if !e.take() {
		t.Fatal("a request a minute later was refused")
	}
}
