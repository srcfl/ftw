package cloudwire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/srcfl/ftw/go/protocol/appwire"
)

// testdata/cloud-once-vectors.json freezes one one-shot read byte for byte
// (ADR 0009, milestone 6): fixed static and ephemeral keys on both ends, a
// read whose have already covers two of the newest envelopes, message 2 with
// the snapshot of what is left, one envelope that did not fit after it, and
// the whole answer body as the service writes it. srcfl/ftw-cloud answers it
// as the service; the watch replays it as the initiator.

const onceVectorsPath = "testdata/cloud-once-vectors.json"

type onceVectorFile struct {
	ServiceStatic    string            `json:"serviceStatic"`
	ServicePublic    string            `json:"servicePublic"`
	ServiceEphemeral string            `json:"serviceEphemeral"`
	WatchStatic      string            `json:"watchStatic"`
	WatchPublic      string            `json:"watchPublic"`
	WatchEphemeral   string            `json:"watchEphemeral"`
	Home             string            `json:"home"`
	Path             string            `json:"path"`
	Prologue         string            `json:"prologue"`
	Msg1Payload      string            `json:"msg1Payload"`
	Request          string            `json:"request"`
	Held             []string          `json:"held"`
	Msg2Payload      string            `json:"msg2Payload"`
	Msg2             string            `json:"msg2"`
	Following        []appVectorFrame  `json:"following"`
	Answer           string            `json:"answer"`
	HandshakeHash    string            `json:"handshakeHash"`
	Notes            map[string]string `json:"notes"`
}

func TestOnceReadVectors(t *testing.T) {
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var signed vectorFile
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	envelope := func(name string) []byte {
		for _, m := range signed.Messages {
			if m.Name == name {
				b, _ := hex.DecodeString(m.Envelope)
				return b
			}
		}
		t.Fatalf("no signed vector %q", name)
		return nil
	}

	labels := map[string]string{
		"serviceStatic":    "ftw.cloud once vector service static",
		"serviceEphemeral": "ftw.cloud once vector service ephemeral",
		"watchStatic":      "ftw.cloud once vector watch static",
		"watchEphemeral":   "ftw.cloud once vector watch ephemeral",
	}
	serviceStatic := fixedX25519(t, labels["serviceStatic"])
	serviceEph := fixedX25519(t, labels["serviceEphemeral"])
	watchStatic := fixedX25519(t, labels["watchStatic"])
	watchEph := fixedX25519(t, labels["watchEphemeral"])
	home := signed.BoxKey.Home
	prologue := OncePrologue(home)

	// The watch holds the full live (seq 3) and the lp (seq 5) from an
	// earlier read, so the service leaves both out.
	have := Have{Boot: bytes.Repeat([]byte{0xb0}, BootBytes), Seq: map[string]uint64{MsgLive: 3, MsgLP: 5}}
	msg1Payload, err := Unsigned(MsgRead, Read{Have: have, Hz: HzLooking})
	if err != nil {
		t.Fatal(err)
	}

	newest := []string{"live full", "live delta", "lp", "prices", "hb"}
	var held, send []string
	for _, n := range newest {
		if have.Covers(envelope(n)) {
			held = append(held, n)
		} else {
			send = append(send, n)
		}
	}
	if len(held) != 2 || held[0] != "live full" || held[1] != "lp" {
		t.Fatalf("have covers %v", held)
	}
	// Message 2 carries the first two; the last follows it, as an envelope
	// that did not fit would.
	inMsg2, after := send[:2], send[2:]
	var envs [][]byte
	for _, n := range inMsg2 {
		envs = append(envs, envelope(n))
	}
	msg2Payload, err := SnapshotPayload(envs, 1_791_820_869_000, true, len(after))
	if err != nil {
		t.Fatal(err)
	}

	watch, err := appwire.NewInitiator(appwire.InitiatorOptions{
		StaticKey: watchStatic, RemoteStatic: serviceStatic.Public(), Prologue: prologue, Ephemeral: &watchEph,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := appwire.NewResponder(appwire.HandshakeOptions{StaticKey: serviceStatic, Prologue: prologue, Ephemeral: &serviceEph})
	if err != nil {
		t.Fatal(err)
	}
	msg1, err := watch.WriteMessage(msg1Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg1) > OnceMessage1MaxBytes {
		t.Fatalf("message 1 is %d bytes", len(msg1))
	}
	got1, err := service.ReadMessage(msg1)
	if err != nil || !bytes.Equal(got1, msg1Payload) {
		t.Fatalf("message 1: %v", err)
	}
	msg2, err := service.WriteMessage(msg2Payload)
	if err != nil {
		t.Fatal(err)
	}
	ss, _ := service.Split()
	serviceT := appwire.NewTransport(ss)

	out := onceVectorFile{
		ServiceStatic: secretHex(labels["serviceStatic"]), ServicePublic: hex.EncodeToString(serviceStatic.Public()),
		ServiceEphemeral: secretHex(labels["serviceEphemeral"]),
		WatchStatic:      secretHex(labels["watchStatic"]), WatchPublic: hex.EncodeToString(watchStatic.Public()),
		WatchEphemeral: secretHex(labels["watchEphemeral"]),
		Home:           home, Path: OncePath + "?" + AppHomeParam + "=" + home,
		Prologue:    hex.EncodeToString(prologue),
		Msg1Payload: hex.EncodeToString(msg1Payload), Request: hex.EncodeToString(msg1),
		Held:        held,
		Msg2Payload: hex.EncodeToString(msg2Payload), Msg2: hex.EncodeToString(msg2),
		HandshakeHash: hex.EncodeToString(ss.HandshakeHash),
		Notes: map[string]string{
			"keys":    "Secrets are SHA-256 of the labels in once_vectors_test.go. X25519 clamps them.",
			"request": "The POST body is message 1 and nothing else.",
			"held":    "The cloud-vectors.json envelopes the read's have covers, which the service leaves out.",
			"answer":  "Message 2, then each following transport message, every one after its length as u16 big endian.",
		},
	}
	messages := [][]byte{msg2}
	for _, n := range after {
		frame, err := EncodeFrame(envelope(n))
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := serviceT.Encrypt(frame)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, sealed)
		out.Following = append(out.Following, appVectorFrame{From: "service", Name: n,
			Frame: hex.EncodeToString(frame), Message: hex.EncodeToString(sealed)})
	}
	answer, err := OnceAnswer(messages...)
	if err != nil {
		t.Fatal(err)
	}
	out.Answer = hex.EncodeToString(answer)

	// The watch's side: split the body, read message 2, open what follows.
	parts, err := SplitOnceAnswer(answer)
	if err != nil || len(parts) != 1+len(after) {
		t.Fatalf("split %d parts: %v", len(parts), err)
	}
	got2, err := watch.ReadMessage(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	snap, err := ParseSnapshot(got2)
	if err != nil || len(snap.Envelopes) != len(inMsg2) || snap.More != len(after) {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}
	ws, _ := watch.Split()
	if !bytes.Equal(ws.HandshakeHash, ss.HandshakeHash) {
		t.Fatal("the two ends disagree on the handshake hash")
	}
	watchT := appwire.NewTransport(ws)
	for i, n := range after {
		frame, err := watchT.Decrypt(parts[1+i])
		if err != nil {
			t.Fatal(err)
		}
		env, err := DecodeFrame(frame)
		if err != nil || !bytes.Equal(env, envelope(n)) {
			t.Fatalf("%s did not come through as the box's bytes: %v", n, err)
		}
	}

	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
		if err := os.WriteFile(onceVectorsPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(onceVectorsPath)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the bytes no longer match %s. If the wire change is intended, "+
			"regenerate with -update and pair the change with srcfl/ftw-cloud and the watch", onceVectorsPath)
	}
}

// A once handshake never opens an app session, nor the reverse: the prologues
// differ, so each binds its own door.
func TestOnceAndAppHandshakesStayApart(t *testing.T) {
	const home = "fjzyr67buzpyncqmjliwgezvde"
	if bytes.Equal(OncePrologue(home), AppPrologue(home)) {
		t.Fatal("the once and app prologues are the same")
	}
	service := fixedX25519(t, "once apart service")
	watchKey := fixedX25519(t, "once apart watch")
	watch, err := appwire.NewInitiator(appwire.InitiatorOptions{
		StaticKey: watchKey, RemoteStatic: service.Public(), Prologue: OncePrologue(home),
	})
	if err != nil {
		t.Fatal(err)
	}
	read, _ := Unsigned(MsgRead, Read{Hz: HzAway})
	msg1, err := watch.WriteMessage(read)
	if err != nil {
		t.Fatal(err)
	}
	app, err := appwire.NewResponder(appwire.HandshakeOptions{StaticKey: service, Prologue: AppPrologue(home)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.ReadMessage(msg1); err == nil {
		t.Fatal("an app session read a once handshake")
	}
}

func TestHaveCoversOnlyWhatTheWatchHolds(t *testing.T) {
	boot := bytes.Repeat([]byte{0xb0}, BootBytes)
	env := func(t2 string, seq uint64, b []byte) []byte {
		raw, err := Marshal(Header{Seq: seq, Boot: b})
		if err != nil {
			t.Fatal(err)
		}
		e, err := Envelope{T: t2, B: raw, Sig: []byte{1}}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	have := Have{Boot: boot, Seq: map[string]uint64{MsgLive: 10}}
	for _, c := range []struct {
		name string
		env  []byte
		want bool
	}{
		{"older live", env(MsgLive, 9, boot), true},
		{"the same live", env(MsgLive, 10, boot), true},
		{"newer live", env(MsgLive, 11, boot), false},
		{"a class it holds none of", env(MsgHB, 1, boot), false},
		{"another boot", env(MsgLive, 1, bytes.Repeat([]byte{0xb1}, BootBytes)), false},
		{"not an envelope", []byte{0xa0}, false},
	} {
		if got := have.Covers(c.env); got != c.want {
			t.Fatalf("%s: covered = %v", c.name, got)
		}
	}
	if (Have{}).Covers(env(MsgLive, 1, boot)) {
		t.Fatal("an empty have covered an envelope")
	}
}

func TestOnceAnswerFraming(t *testing.T) {
	body, err := OnceAnswer([]byte{1, 2, 3}, []byte{}, bytes.Repeat([]byte{9}, 300))
	if err != nil {
		t.Fatal(err)
	}
	parts, err := SplitOnceAnswer(body)
	if err != nil || len(parts) != 3 || len(parts[0]) != 3 || len(parts[1]) != 0 || len(parts[2]) != 300 {
		t.Fatalf("split %d parts: %v", len(parts), err)
	}
	for _, bad := range [][]byte{nil, {0}, {0, 5, 1, 2}} {
		if _, err := SplitOnceAnswer(bad); err == nil {
			t.Fatalf("%x split", bad)
		}
	}
	if _, err := OnceAnswer(); err == nil {
		t.Fatal("an answer with no message 2 framed")
	}
}

// push.register says which app a token belongs to; a phone that leaves it out
// is the iPhone app, as before.
func TestPushRegisterNamesTheApp(t *testing.T) {
	old, _ := Marshal(PushRegister{APNsToken: []byte{1}, Environment: PushProduction, Kind: PushKindAlert})
	var back PushRegister
	if err := Unmarshal(old, &back); err != nil || back.App != "" {
		t.Fatalf("an old registration read app %q: %v", back.App, err)
	}
	watch, _ := Marshal(PushRegister{APNsToken: []byte{1}, Environment: PushProduction, Kind: PushKindAlert, App: PushAppWatch})
	if bytes.Equal(old, watch) {
		t.Fatal("app did not reach the wire")
	}
	if err := Unmarshal(watch, &back); err != nil || back.App != PushAppWatch {
		t.Fatalf("a watch registration read app %q: %v", back.App, err)
	}
}

// testdata/cloud-once-x-vectors.json freezes one exchange request through
// the service byte for byte: the watch's x {d} in message 1, the x {dev, id,
// d} the service sends the box and the one the box sends back, and message 2
// with the box's d as the service writes it. d is filler of the exchange's
// real lengths; the exchange's own bytes are in srcfl/ftw
// go/internal/appuplink/testdata/exchange-vectors.json.
const onceXVectorsPath = "testdata/cloud-once-x-vectors.json"

type onceXVectorFile struct {
	ServiceStatic    string            `json:"serviceStatic"`
	ServicePublic    string            `json:"servicePublic"`
	ServiceEphemeral string            `json:"serviceEphemeral"`
	WatchStatic      string            `json:"watchStatic"`
	WatchPublic      string            `json:"watchPublic"`
	WatchEphemeral   string            `json:"watchEphemeral"`
	Home             string            `json:"home"`
	Prologue         string            `json:"prologue"`
	Msg1Payload      string            `json:"msg1Payload"`
	Request          string            `json:"request"`
	Down             string            `json:"down"`
	Up               string            `json:"up"`
	Msg2Payload      string            `json:"msg2Payload"`
	Answer           string            `json:"answer"`
	Notes            map[string]string `json:"notes"`
}

func TestOnceExchangeVectors(t *testing.T) {
	labels := map[string]string{
		"serviceStatic":    "ftw.cloud once vector service static",
		"serviceEphemeral": "ftw.cloud once x vector service ephemeral",
		"watchStatic":      "ftw.cloud once vector watch static",
		"watchEphemeral":   "ftw.cloud once x vector watch ephemeral",
	}
	serviceStatic := fixedX25519(t, labels["serviceStatic"])
	serviceEph := fixedX25519(t, labels["serviceEphemeral"])
	watchStatic := fixedX25519(t, labels["watchStatic"])
	watchEph := fixedX25519(t, labels["watchEphemeral"])
	raw, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatal(err)
	}
	var signed vectorFile
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	home := signed.BoxKey.Home
	prologue := OncePrologue(home)
	dev := DeviceID(watchStatic.Public())

	// An exchange handshake's message 1, and its message 2 from the box.
	request := bytes.Repeat([]byte{0x5a}, appwire.Message1Overhead+512)
	answer := bytes.Repeat([]byte{0xa5}, appwire.Message2Overhead+1024)
	msg1Payload, err := Unsigned(MsgX, AppX{D: request})
	if err != nil {
		t.Fatal(err)
	}
	msg2Payload, err := Unsigned(MsgX, AppX{D: answer})
	if err != nil {
		t.Fatal(err)
	}
	frame := func(x X) []byte {
		body, err := Marshal(x)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := Envelope{T: MsgX, B: body}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		f, err := EncodeFrame(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(f) > MaxXFrame {
			t.Fatalf("an x frame is %d bytes, over %d", len(f), MaxXFrame)
		}
		return f
	}
	down := frame(X{Dev: dev, ID: 1, D: request})
	up := frame(X{Dev: dev, ID: 1, D: answer})

	watch, err := appwire.NewInitiator(appwire.InitiatorOptions{
		StaticKey: watchStatic, RemoteStatic: serviceStatic.Public(), Prologue: prologue, Ephemeral: &watchEph,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := appwire.NewResponder(appwire.HandshakeOptions{StaticKey: serviceStatic, Prologue: prologue, Ephemeral: &serviceEph})
	if err != nil {
		t.Fatal(err)
	}
	msg1, err := watch.WriteMessage(msg1Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg1) > OnceMessage1MaxBytes {
		t.Fatalf("message 1 is %d bytes", len(msg1))
	}
	got1, err := service.ReadMessage(msg1)
	if err != nil || !bytes.Equal(got1, msg1Payload) {
		t.Fatalf("message 1: %v", err)
	}
	msg2, err := service.WriteMessage(msg2Payload)
	if err != nil {
		t.Fatal(err)
	}
	body, err := OnceAnswer(msg2)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := SplitOnceAnswer(body)
	if err != nil || len(parts) != 1 {
		t.Fatalf("split %d parts: %v", len(parts), err)
	}
	got2, err := watch.ReadMessage(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	env, err := DecodeEnvelope(got2)
	var x AppX
	if err != nil || env.T != MsgX || len(env.Sig) != 0 || Unmarshal(env.B, &x) != nil || !bytes.Equal(x.D, answer) {
		t.Fatalf("the watch read message 2 as %+v: %v", env, err)
	}

	out := onceXVectorFile{
		ServiceStatic: secretHex(labels["serviceStatic"]), ServicePublic: hex.EncodeToString(serviceStatic.Public()),
		ServiceEphemeral: secretHex(labels["serviceEphemeral"]),
		WatchStatic:      secretHex(labels["watchStatic"]), WatchPublic: hex.EncodeToString(watchStatic.Public()),
		WatchEphemeral: secretHex(labels["watchEphemeral"]),
		Home:           home, Prologue: hex.EncodeToString(prologue),
		Msg1Payload: hex.EncodeToString(msg1Payload), Request: hex.EncodeToString(msg1),
		Down: hex.EncodeToString(down), Up: hex.EncodeToString(up),
		Msg2Payload: hex.EncodeToString(msg2Payload), Answer: hex.EncodeToString(body),
		Notes: map[string]string{
			"keys":    "Secrets are SHA-256 of the labels in once_vectors_test.go. X25519 clamps them.",
			"request": "The POST body is message 1, which carries x {d}: one exchange message.",
			"down":    "The frame the service sends the box: x {dev, id, d}, dev from its own handshake, id its own number.",
			"up":      "The box's answer under the same dev and id; d goes to the watch unchanged.",
			"answer":  "Message 2 after its length as u16 big endian. An empty d means no answer came.",
		},
	}
	gotJSON, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	gotJSON = append(gotJSON, '\n')
	if *update {
		if err := os.WriteFile(onceXVectorsPath, gotJSON, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(onceXVectorsPath)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(gotJSON, want) {
		t.Fatalf("the bytes no longer match %s. If the wire change is intended, "+
			"regenerate with -update and pair the change with srcfl/ftw-cloud and the watch", onceXVectorsPath)
	}
}
