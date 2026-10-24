package appuplink

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"testing"

	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/protocol/appwire"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// The exchange's golden vectors (ADR 0009, "Vectors").
//
// testdata/exchange-vectors.json holds the bytes of every step of one watch
// session, from fixed keys: X1 for a watch its phone added by its key, a
// command answered by its ack and result, and a read answered by now and a
// tick, each also as the x that carries it on the uplink to FTW Cloud. The
// Swift client in srcfl/ftw-app replays the same file, so Go and Swift agree
// on every byte or one of them fails. Regenerate with
//
//	go test ./internal/appuplink -run TestExchangeVectors -update-exchange-vectors

var updateExchangeVectors = flag.Bool("update-exchange-vectors", false, "rewrite testdata/exchange-vectors.json")

type exchangeVectors struct {
	About        string `json:"about"`
	ProtocolName string `json:"protocolName"`
	Prologue     string `json:"prologue"`

	BoxStatic         string `json:"boxStatic"`
	BoxStaticPublic   string `json:"boxStaticPublic"`
	BoxEphemeral      string `json:"boxEphemeral"`
	WatchStatic       string `json:"watchStatic"`
	WatchStaticPublic string `json:"watchStaticPublic"`
	WatchEphemeral    string `json:"watchEphemeral"`
	HandshakeHash     string `json:"handshakeHash"`

	Steps []exchangeVectorStep `json:"steps"`
}

// exchangeVectorStep is one message of the exchange. Frames are the
// plaintext lane frames inside it; Body is the exchange message itself, the d
// of the watch's x {d} in its one-shot request or of the answer it receives;
// Uplink is the frame x {dev, id, d} as it crosses the box's uplink.
type exchangeVectorStep struct {
	Name   string   `json:"name"`
	From   string   `json:"from"`
	Frames []string `json:"frames"`
	Body   string   `json:"body"`
	Uplink string   `json:"uplink"`
}

// fixed is n bytes counting up from first: a key nobody would use, which is
// the point.
func fixed(first byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = first + byte(i)
	}
	return out
}

func frameOf(t *testing.T, lane uint8, bucket int, msgType string, body any) []byte {
	t.Helper()
	raw, err := appproto.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := appwire.EncodeFrame(appwire.Frame{
		Lane: lane, Bucket: bucket, Envelope: appwire.Envelope{T: msgType, B: raw},
	})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func buildExchangeVectors(t *testing.T) exchangeVectors {
	t.Helper()
	keyPair := func(first byte) appwire.KeyPair {
		kp, err := appwire.KeyPairFromSecret(fixed(first, appwire.DHBytes))
		if err != nil {
			t.Fatal(err)
		}
		return kp
	}
	boxStatic, boxEphemeral := keyPair(0x01), keyPair(0x61)
	watchStatic, watchEphemeral := keyPair(0x21), keyPair(0x41)
	dev := cloudwire.DeviceID(watchStatic.Public())
	prologueBytes := exchangePrologue(boxStatic.Public())

	watch, err := appwire.NewInitiator(appwire.InitiatorOptions{
		StaticKey: watchStatic, RemoteStatic: boxStatic.Public(), Prologue: prologueBytes, Ephemeral: &watchEphemeral,
	})
	if err != nil {
		t.Fatal(err)
	}
	box, err := appwire.NewResponder(appwire.HandshakeOptions{
		StaticKey: boxStatic, Prologue: prologueBytes, Ephemeral: &boxEphemeral,
	})
	if err != nil {
		t.Fatal(err)
	}

	v := exchangeVectors{
		About: "ADR 0009 watch exchange: Noise IK with prologue ftw.exchange.v1: and the box's static key. " +
			"Every frame but hello_ok is lane 0 in 512 B; every answer after the handshake is two transport messages.",
		ProtocolName:      appwire.ProtocolName,
		Prologue:          hex.EncodeToString(prologueBytes),
		BoxStatic:         hex.EncodeToString(fixed(0x01, appwire.DHBytes)),
		BoxStaticPublic:   hex.EncodeToString(boxStatic.Public()),
		BoxEphemeral:      hex.EncodeToString(fixed(0x61, appwire.DHBytes)),
		WatchStatic:       hex.EncodeToString(fixed(0x21, appwire.DHBytes)),
		WatchStaticPublic: hex.EncodeToString(watchStatic.Public()),
		WatchEphemeral:    hex.EncodeToString(fixed(0x41, appwire.DHBytes)),
	}
	step := func(name, from string, frames [][]byte, body []byte) {
		// A request and its answer share the service's number.
		x, err := cloudwire.Marshal(cloudwire.X{Dev: dev, ID: uint32(len(v.Steps)/2 + 1), D: body})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := cloudwire.Envelope{T: cloudwire.MsgX, B: x}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		uplink, err := cloudwire.EncodeFrame(raw)
		if err != nil {
			t.Fatal(err)
		}
		s := exchangeVectorStep{Name: name, From: from, Body: hex.EncodeToString(body), Uplink: hex.EncodeToString(uplink)}
		for _, f := range frames {
			s.Frames = append(s.Frames, hex.EncodeToString(f))
		}
		v.Steps = append(v.Steps, s)
	}

	// X1: hello in message 1, hello_ok in message 2. The phone added the
	// watch by its key, so the hello carries no code.
	hello := frameOf(t, appwire.LaneControl, exchangeFrameBytes, appproto.MsgHello, appproto.Hello{
		Proto: appproto.ProtoRange{Min: appproto.ProtoMin, Max: appproto.ProtoMax},
		App:   appproto.AppInfo{Build: "vectors", UA: "watchos"},
	})
	message1, err := watch.WriteMessage(hello)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := box.ReadMessage(message1); err != nil || !bytes.Equal(got, hello) {
		t.Fatalf("the box read message 1 as %x, %v", got, err)
	}
	step("x1.request", "watch", [][]byte{hello}, message1)

	helloOK := frameOf(t, appwire.LaneBulk, 1024, appproto.MsgHelloOK, appproto.ExchangeHelloOK{
		Proto:  appproto.ProtoMax,
		Mode:   appproto.BoxModeFull,
		Clock:  appproto.Clock{Source: "ntp", SyncedAtMs: 1_760_000_000_000, UptimeMs: 60_000},
		Role:   "watch",
		Scopes: []string{"ftw.dispatch.write", "ftw.events.read", "ftw.live.read", "ftw.plan.read"},
		Rev:    7,
	})
	message2, err := box.WriteMessage(helloOK)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := watch.ReadMessage(message2); err != nil || !bytes.Equal(got, helloOK) {
		t.Fatalf("the watch read message 2 as %x, %v", got, err)
	}
	step("x1.answer", "box", [][]byte{helloOK}, message2)

	watchSession, err := watch.Split()
	if err != nil {
		t.Fatal(err)
	}
	boxSession, err := box.Split()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(watchSession.HandshakeHash, boxSession.HandshakeHash) {
		t.Fatal("the two ends disagree on the handshake hash")
	}
	v.HandshakeHash = hex.EncodeToString(boxSession.HandshakeHash)
	watchT, boxT := appwire.NewTransport(watchSession), appwire.NewTransport(boxSession)

	seal := func(tr *appwire.Transport, frames ...[]byte) []byte {
		var out []byte
		for _, f := range frames {
			sealed, err := tr.Encrypt(f)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, sealed...)
		}
		return out
	}
	open := func(tr *appwire.Transport, body []byte, frames ...[]byte) {
		for i, want := range frames {
			got, err := tr.Decrypt(body[i*exchangeRequestBytes : (i+1)*exchangeRequestBytes])
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("transport message %d opened as %x, %v", i, got, err)
			}
		}
	}

	// A command, answered by its ack and its result.
	cmd := frameOf(t, appwire.LaneControl, exchangeFrameBytes, appproto.MsgCmd, appproto.Cmd{
		CmdID: "0192f2a0-7c1e-7000-8000-0123456789ab", Op: appproto.OpLoadpointHold,
		Args:            map[string]any{"id": "lp1", "power_w": 7360, "hold_s": 0, "phase_mode": "3p"},
		NotValidAfterMs: 90_000, Expect: appproto.Expect{Rev: 7},
	})
	body := seal(watchT, cmd)
	open(boxT, body, cmd)
	step("cmd.request", "watch", [][]byte{cmd}, body)

	ack := frameOf(t, appwire.LaneControl, exchangeFrameBytes, appproto.MsgCmdAck, appproto.CmdAck{
		CmdID: "0192f2a0-7c1e-7000-8000-0123456789ab", LeaseID: "lease-vectors", ExpiresAtMs: 960_000,
	})
	result := frameOf(t, appwire.LaneControl, exchangeFrameBytes, appproto.MsgCmdResult, appproto.CmdResult{
		CmdID: "0192f2a0-7c1e-7000-8000-0123456789ab", State: appproto.CmdApplied,
		Observed: &appproto.Observed{Value: 7360, Src: appproto.ObservedSrcCore, UptimeMs: 61_000},
	})
	body = seal(boxT, ack, result)
	open(watchT, body, ack, result)
	step("cmd.answer", "box", [][]byte{ack, result}, body)

	// A read, answered by now and a tick.
	soc := int64(620)
	read := frameOf(t, appwire.LaneControl, exchangeFrameBytes, appproto.MsgNow, struct{}{})
	body = seal(watchT, read)
	open(boxT, body, read)
	step("now.request", "watch", [][]byte{read}, body)

	now := frameOf(t, appwire.LaneControl, exchangeFrameBytes, appproto.MsgNow, appproto.Now{
		UptimeMs: 62_000, ControlRev: 7,
		Fields: map[string]int64{"1": 2, "2": 1200, "3": -3400, "4": 900, "5": 620, "6": 3700, "10": 7360},
		Sources: map[string]appproto.NowSource{
			"7": {State: appproto.SourceLive, AgeMs: 400},
			"8": {State: appproto.SourceLive, AgeMs: 400},
			"9": {State: appproto.SourceLive, AgeMs: 400},
		},
		DispatchBlockedBy: []string{},
		Loadpoints:        []appproto.NowLoadpoint{{ID: "lp1", Plugged: true, SocPermille: &soc, PowerW: 7360}},
	})
	tick := frameOf(t, appwire.LaneControl, exchangeFrameBytes, appproto.MsgTick, appproto.Tick{UptimeMs: 62_000})
	body = seal(boxT, now, tick)
	open(watchT, body, now, tick)
	step("now.answer", "box", [][]byte{now, tick}, body)

	return v
}

func TestExchangeVectors(t *testing.T) {
	got, err := json.MarshalIndent(buildExchangeVectors(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "testdata/exchange-vectors.json"
	if *updateExchangeVectors {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; regenerate with -update-exchange-vectors", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the exchange no longer produces the bytes in " + path +
			"; if the change is meant, regenerate it and update srcfl/ftw-app's copy in the same pair of PRs")
	}

	// The sizes the docs promise, each inside one x on the uplink.
	var v exchangeVectors
	if err := json.Unmarshal(want, &v); err != nil {
		t.Fatal(err)
	}
	sizes := map[string]int{
		"x1.request": exchangeOpenBytes, "x1.answer": appwire.Message2Overhead + 1024,
		"cmd.request": exchangeRequestBytes, "cmd.answer": 2 * exchangeRequestBytes,
		"now.request": exchangeRequestBytes, "now.answer": 2 * exchangeRequestBytes,
	}
	for _, s := range v.Steps {
		if n := len(s.Body) / 2; n != sizes[s.Name] {
			t.Fatalf("%s is %d bytes, want %d", s.Name, n, sizes[s.Name])
		}
		if n := len(s.Uplink) / 2; n > cloudwire.MaxXFrame {
			t.Fatalf("%s's x is %d bytes, over %d", s.Name, n, cloudwire.MaxXFrame)
		}
	}
}

// The box's own code path reads the vectors' message 1 and answers with their
// message 2, given the same fixed ephemeral key: the vectors are what the box
// does, not a second implementation that happens to agree with itself.
func TestTheBoxAnswersTheVectorsHandshake(t *testing.T) {
	v := buildExchangeVectors(t)
	boxStatic, err := appwire.KeyPairFromSecret(fixed(0x01, appwire.DHBytes))
	if err != nil {
		t.Fatal(err)
	}
	boxEphemeral, err := appwire.KeyPairFromSecret(fixed(0x61, appwire.DHBytes))
	if err != nil {
		t.Fatal(err)
	}
	box, err := appwire.NewResponder(appwire.HandshakeOptions{
		StaticKey: boxStatic, Prologue: exchangePrologue(boxStatic.Public()), Ephemeral: &boxEphemeral,
	})
	if err != nil {
		t.Fatal(err)
	}
	message1, _ := hex.DecodeString(v.Steps[0].Body)
	payload, err := box.ReadMessage(message1)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(payload) != v.Steps[0].Frames[0] {
		t.Fatal("the box read a different hello out of the vectors' message 1")
	}
	helloOK, _ := hex.DecodeString(v.Steps[1].Frames[0])
	message2, err := box.WriteMessage(helloOK)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(message2) != v.Steps[1].Body {
		t.Fatal("the box's message 2 differs from the vectors'")
	}
	if hex.EncodeToString(box.RemoteStatic()) != v.WatchStaticPublic {
		t.Fatal("the box authenticated a different watch key")
	}
}
