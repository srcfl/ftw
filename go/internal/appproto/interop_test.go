package appproto

import (
	"encoding/hex"
	"testing"

	"github.com/srcfl/ftw/go/internal/control"
)

// Bytes the app itself produced.
//
// Captured by encoding the shapes in src/lib/protocol/messages.ts of
// srcfl/ftw-webapp with that project's own CBOR library (cbor2), so these
// vectors are what a browser really puts on the wire — not what this package
// thinks a browser would put on the wire. A test built from our own encoder
// would only prove that this package agrees with itself.
//
// Regenerate by encoding the same objects in the app and pasting the hex.
var appVectors = map[string]string{
	// {t:'hello', b:{proto:{min:0,max:1}, app:{build:'test',ua:'pwa'}, locales:['sv']}}
	"hello": "a261746568656c6c6f6162a36570726f746fa2636d696e00636d61780163617070a2656275696c64647465737462756163707761676c6f63616c657381627376",
	// {t:'sub', b:{bucket:512, hz:1}}
	"sub": "a26174637375626162a2666275636b657419020062687a01",
	// {t:'plan.get', id:4}
	"plan.get": "a2617468706c616e2e67657462696404",
	// {t:'cmd', b:{cmdId:…, op:'site.mode.set', args:{mode:'charge'},
	//              notValidAfterMs:200000, expect:{rev:7, guards:[{fid:5,op:'lt',value:900}]}}}
	"cmd": "a2617463636d646162a565636d644964782430313932663261302d376331652d373030302d383030302d303132333435363738396162626f706d736974652e6d6f64652e7365746461726773a1646d6f6465666368617267656f6e6f7456616c696441667465724d731a00030d4066657870656374a263726576076667756172647381a36366696405626f70626c746576616c7565190384",
	// A newer app: extra keys the box has never heard of, at two levels.
	"hello_future": "a261746568656c6c6f6162a46570726f746fa3636d696e00636d617801697072656665727265640363617070a3656275696c646666757475726562756163707761676368616e6e656c6462657461676c6f63616c6573816273766974656c657061746879f5",
}

func appFrame(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(appVectors[name])
	if err != nil || len(raw) == 0 {
		t.Fatalf("vector %q: %v", name, err)
	}
	frame, err := testCodec{}.EncodeFrame(Frame{Lane: LaneControl, Bucket: 512, Envelope: mustEnvelope(t, raw)})
	if err != nil {
		t.Fatalf("frame %q: %v", name, err)
	}
	return frame
}

func mustEnvelope(t *testing.T, raw []byte) Envelope {
	t.Helper()
	env, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("the app's own bytes did not decode as an envelope: %v", err)
	}
	return env
}

func TestHandlesTheAppsOwnHello(t *testing.T) {
	h, _, rec, _ := newRig(t)
	if err := h.Handle(t.Context(), appFrame(t, "hello")); err != nil {
		t.Fatal(err)
	}
	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
	if ok.Proto != ProtoMax || ok.Mode != BoxModeFull {
		t.Fatalf("hello_ok = %+v", ok)
	}
}

func TestHandlesTheAppsOwnSub(t *testing.T) {
	h, _, rec, _ := newRig(t)
	if err := h.Handle(t.Context(), appFrame(t, "hello")); err != nil {
		t.Fatal(err)
	}
	rec.reset()
	if err := h.Handle(t.Context(), appFrame(t, "sub")); err != nil {
		t.Fatal(err)
	}
	if !h.Subscribed() {
		t.Fatal("the app's own sub did not start the stream")
	}
	snap := body[Snap](t, rec.only(t, MsgSnap))
	if len(snap.Fields) != 6 {
		t.Fatalf("snapshot has %d fields", len(snap.Fields))
	}
}

func TestHandlesTheAppsOwnPlanGet(t *testing.T) {
	h, box, rec, clock := newRig(t)
	box.plan = samplePlan(clock.now)
	if err := h.Handle(t.Context(), appFrame(t, "plan.get")); err != nil {
		t.Fatal(err)
	}
	f := rec.only(t, MsgPlan)
	if f.env.ID == nil || *f.env.ID != 4 {
		t.Fatalf("the app's request id did not come back: %v", f.env.ID)
	}
}

// The whole command shape, decoded from the app's bytes: idempotency key,
// mandatory expiry, expected rev and a guard on state of charge.
func TestHandlesTheAppsOwnCommand(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)

	if err := h.Handle(t.Context(), appFrame(t, "cmd")); err != nil {
		t.Fatal(err)
	}

	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdApplied {
		t.Fatalf("state = %q, want applied: %+v", res.State, res.Error)
	}
	if box.mode != control.ModeCharge {
		t.Fatalf("mode = %q", box.mode)
	}

	// The same bytes again. A retry must not act twice.
	if err := h.Handle(t.Context(), appFrame(t, "cmd")); err != nil {
		t.Fatal(err)
	}
	if box.setModeCalls != 1 {
		t.Fatalf("the app's retry ran the dispatcher %d times", box.setModeCalls)
	}
}

// The guard in the app's vector is battery_soc < 900 permille. Above it, the
// same bytes must be refused — the app did not change, the house did.
func TestTheAppsOwnCommandIsRefusedWhenItsGuardFails(t *testing.T) {
	h, box, rec, _ := newRig(t)
	subscribe(t, h, rec)
	box.snap.BatterySoC = 0.95

	if err := h.Handle(t.Context(), appFrame(t, "cmd")); err != nil {
		t.Fatal(err)
	}
	res := body[CmdResult](t, rec.only(t, MsgCmdResult))
	if res.State != CmdRejected || res.Error == nil || res.Error.Code != ErrPrecondition {
		t.Fatalf("result = %+v", res)
	}
}

// A bundle newer than this box sends keys it has never heard of. Ignoring them
// is what lets a service worker pin an old build without a white screen — in
// this direction as well as the other.
func TestHandlesAHelloFromANewerApp(t *testing.T) {
	h, _, rec, _ := newRig(t)
	if err := h.Handle(t.Context(), appFrame(t, "hello_future")); err != nil {
		t.Fatal(err)
	}
	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
	if ok.Proto != ProtoMax {
		t.Fatalf("proto = %d", ok.Proto)
	}
}

// Everything the box sends must land in the shapes the app declares. Anything
// the app reads as optional is checked for presence rather than absence, and
// anything it types as nullable must be null and not an empty string.
func TestOutboundBodiesMatchTheAppsDeclaredShapes(t *testing.T) {
	h, box, rec, clock := newRig(t)
	box.plan = samplePlan(clock.now)
	subscribe(t, h, rec)
	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 1})

	snap := body[Snap](t, rec.only(t, MsgSnap))
	// messages.ts: dict is Record<string, {name, unit: string|null, srcId: string|null}>
	if d := snap.Dict[fidKey(FidMode)]; d.Unit != nil || d.SrcID != nil {
		t.Fatal("a field with no unit or source sent empty strings where the app expects null")
	}
	if d := snap.Dict[fidKey(FidBatterySoc)]; d.Unit == nil || *d.Unit != "permille" {
		t.Fatalf("battery_soc unit = %v, want permille", d.Unit)
	}
	rec.reset()

	deliver(t, h, MsgPlanGet, ptrU32(1), nil)
	p := body[Plan](t, rec.only(t, MsgPlan))
	// messages.ts: ceilingW is number|null, priceMinor is number|null.
	if p.CeilingW != nil {
		t.Fatal("a site with no ceiling sent one")
	}
	if p.Slots[0].PriceMinor == nil {
		t.Fatal("a priced slot sent null")
	}
}
