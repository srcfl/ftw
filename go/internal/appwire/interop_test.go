package appwire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// Interop against the app.
//
// testdata/webapp-vectors.json holds bytes the TypeScript in srcfl/ftw-webapp
// actually produced — a handshake, a frame of every awkward shape, and a
// transport script in both directions. Regenerate it with
// testdata/run-generator.mjs after any change to either implementation.
//
// Without this the two sides only prove they agree with themselves. With it,
// drift fails here rather than in a house.

type interopVectors struct {
	ProtocolName string `json:"protocolName"`

	Handshake struct {
		Prologue              string `json:"prologue"`
		InitiatorStatic       string `json:"initiatorStatic"`
		InitiatorStaticPublic string `json:"initiatorStaticPublic"`
		ResponderStatic       string `json:"responderStatic"`
		ResponderStaticPublic string `json:"responderStaticPublic"`
		ResponderEphemeral    string `json:"responderEphemeral"`
		Message1Payload       string `json:"message1Payload"`
		Message1              string `json:"message1"`
		Message2Payload       string `json:"message2Payload"`
		Message2              string `json:"message2"`
		HandshakeHash         string `json:"handshakeHash"`
	} `json:"handshake"`

	Frames []struct {
		Name         string   `json:"name"`
		Lane         uint8    `json:"lane"`
		Flags        uint8    `json:"flags"`
		Bucket       int      `json:"bucket"`
		PayloadLen   int      `json:"payloadLen"`
		EnvelopeKeys []string `json:"envelopeKeys"`
		Bytes        string   `json:"bytes"`
	} `json:"frames"`

	Transport []struct {
		From  string `json:"from"`
		Frame string `json:"frame"`
		Seq   uint64 `json:"seq"`
		Wire  string `json:"wire"`
	} `json:"transport"`
}

func loadVectors(t *testing.T) *interopVectors {
	t.Helper()

	raw, err := os.ReadFile("testdata/webapp-vectors.json")
	if err != nil {
		t.Fatalf("reading vectors: %v", err)
	}
	var v interopVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parsing vectors: %v", err)
	}
	return &v
}

func TestInteropProtocolName(t *testing.T) {
	if got := loadVectors(t).ProtocolName; got != ProtocolName {
		t.Fatalf("the app speaks %q, this box speaks %q", got, ProtocolName)
	}
}

// Decodes every frame the app produced, and re-encodes the ones it can.
//
// A frame carrying keys this box does not know cannot be re-encoded byte for
// byte, because dropping them is the whole point — so those are checked one
// way only.
func TestInteropFrames(t *testing.T) {
	v := loadVectors(t)
	if len(v.Frames) == 0 {
		t.Fatal("no frame vectors")
	}

	for _, want := range v.Frames {
		t.Run(want.Name, func(t *testing.T) {
			wire := unhex(t, want.Bytes)
			if len(wire) != want.Bucket {
				t.Fatalf("frame is %d bytes, bucket is %d", len(wire), want.Bucket)
			}

			frame, err := DecodeFrame(wire)
			if err != nil {
				t.Fatalf("DecodeFrame: %v", err)
			}
			if frame.Bucket != want.Bucket {
				t.Errorf("bucket = %d, want %d", frame.Bucket, want.Bucket)
			}
			if frame.Lane != want.Lane {
				t.Errorf("lane = %d, want %d", frame.Lane, want.Lane)
			}
			if frame.Flags != want.Flags {
				t.Errorf("flags = %#x, want %#x", frame.Flags, want.Flags)
			}

			// Everything past the declared length is padding and must be
			// zeros, whichever side wrote it.
			for i, b := range wire[HeaderBytes+want.PayloadLen:] {
				if b != 0 {
					t.Fatalf("padding byte %d is %#x", i, b)
				}
			}

			known := true
			for _, key := range want.EnvelopeKeys {
				if !slices.Contains([]string{"t", "id", "b"}, key) {
					known = false
				}
			}
			if !known {
				return
			}

			got, err := EncodeFrame(frame)
			if err != nil {
				t.Fatalf("EncodeFrame: %v", err)
			}
			if !bytes.Equal(got, wire) {
				t.Fatalf("re-encoded frame differs from the app's bytes\n got %x\nwant %x", got, wire)
			}
		})
	}
}

// Runs the app's handshake against this box and then its transport script.
func TestInteropSession(t *testing.T) {
	v := loadVectors(t)

	boxStatic := keyFromHex(t, v.Handshake.ResponderStatic)
	if got := hex.EncodeToString(boxStatic.Public()); got != v.Handshake.ResponderStaticPublic {
		t.Fatalf("box public = %s, want %s", got, v.Handshake.ResponderStaticPublic)
	}

	boxEphemeral := keyFromHex(t, v.Handshake.ResponderEphemeral)
	responder, err := NewResponder(HandshakeOptions{
		StaticKey: boxStatic,
		Prologue:  unhex(t, v.Handshake.Prologue),
		Ephemeral: &boxEphemeral,
	})
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}

	payload, err := responder.ReadMessage(unhex(t, v.Handshake.Message1))
	if err != nil {
		t.Fatalf("reading the app's first message: %v", err)
	}
	if got := hex.EncodeToString(payload); got != v.Handshake.Message1Payload {
		t.Fatalf("message 1 payload = %s, want %s", got, v.Handshake.Message1Payload)
	}

	message2, err := responder.WriteMessage(unhex(t, v.Handshake.Message2Payload))
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if got := hex.EncodeToString(message2); got != v.Handshake.Message2 {
		t.Fatalf("message 2 = %s\n           want %s", got, v.Handshake.Message2)
	}

	session, err := responder.Split()
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if got := hex.EncodeToString(session.HandshakeHash); got != v.Handshake.HandshakeHash {
		t.Fatalf("handshake hash = %s, want %s", got, v.Handshake.HandshakeHash)
	}
	// The app's identity comes from the handshake, not from what it claimed.
	if got := hex.EncodeToString(session.RemoteStatic); got != v.Handshake.InitiatorStaticPublic {
		t.Fatalf("remote static = %s, want %s", got, v.Handshake.InitiatorStaticPublic)
	}

	frames := map[string][]byte{}
	for _, f := range v.Frames {
		frames[f.Name] = unhex(t, f.Bytes)
	}

	box := NewTransport(session)
	for i, step := range v.Transport {
		wire := unhex(t, step.Wire)
		frame := frames[step.Frame]
		if frame == nil {
			t.Fatalf("step %d names an unknown frame %q", i, step.Frame)
		}

		switch step.From {
		case "app":
			got, err := box.Decrypt(wire)
			if err != nil {
				t.Fatalf("step %d: decrypting the app's frame: %v", i, err)
			}
			if !bytes.Equal(got, frame) {
				t.Fatalf("step %d: decrypted frame does not match", i)
			}
		case "box":
			// The counters are per direction: a box that shared one with the
			// app would go out of step at the first reply, so this checks the
			// sequence number as well as the bytes.
			if box.NextSeq() != step.Seq {
				t.Fatalf("step %d: NextSeq = %d, want %d", i, box.NextSeq(), step.Seq)
			}
			got, err := box.Encrypt(frame)
			if err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
			if !bytes.Equal(got, wire) {
				t.Fatalf("step %d: encrypted frame differs from the app's bytes\n got %x\nwant %x", i, got, wire)
			}
		default:
			t.Fatalf("step %d: unknown sender %q", i, step.From)
		}
	}
}
