package appwire

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

// Two ends of a session. Where the keys came from is the handshake's business,
// checked against the Cacophony vector in noise_test.go; what matters here is
// that each direction has its own counter and its own window.
func connect(t *testing.T) (app, box *Transport) {
	t.Helper()

	k1 := make([]byte, KeyBytes)
	k2 := make([]byte, KeyBytes)
	if _, err := rand.Read(k1); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if _, err := rand.Read(k2); err != nil {
		t.Fatalf("rand: %v", err)
	}

	hash := bytes.Repeat([]byte{0xab}, HashBytes)
	remote := bytes.Repeat([]byte{0xcd}, DHBytes)

	app = NewTransport(&Session{
		Send:          NewCipherState(append([]byte{}, k1...)),
		Recv:          NewCipherState(append([]byte{}, k2...)),
		HandshakeHash: hash,
		RemoteStatic:  remote,
	})
	box = NewTransport(&Session{
		Send:          NewCipherState(append([]byte{}, k2...)),
		Recv:          NewCipherState(append([]byte{}, k1...)),
		HandshakeHash: hash,
		RemoteStatic:  remote,
	})
	return app, box
}

func tickFrame(t *testing.T) []byte {
	t.Helper()
	wire, err := EncodeFrame(control(Envelope{T: "tick", B: body(t, map[string]int{"seq": 1})}, 0, 512))
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	return wire
}

func deltaFrame(t *testing.T, fields map[string]int) []byte {
	t.Helper()
	wire, err := EncodeFrame(control(Envelope{
		T: "delta",
		B: body(t, map[string]any{"seq": 2, "fields": fields}),
	}, 0, 512))
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	return wire
}

func TestTransportRoundTrip(t *testing.T) {
	app, box := connect(t)
	// Negative grid and battery power: energy leaving the site. A codec that
	// loses the sign loses the direction.
	frame := deltaFrame(t, map[string]int{"2": -4200, "3": 6200, "4": -1500, "5": 875})

	wire, err := app.Encrypt(frame)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := box.Decrypt(wire)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatal("app to box round trip changed the frame")
	}

	wire, err = box.Encrypt(frame)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err = app.Decrypt(wire)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatal("box to app round trip changed the frame")
	}
}

func TestTransportKeepsALongStreamInStep(t *testing.T) {
	app, box := connect(t)

	for i := range 200 {
		frame := deltaFrame(t, map[string]int{"2": i * 13})
		wire, err := app.Encrypt(frame)
		if err != nil {
			t.Fatalf("Encrypt %d: %v", i, err)
		}
		got, err := box.Decrypt(wire)
		if err != nil {
			t.Fatalf("Decrypt %d: %v", i, err)
		}
		if !bytes.Equal(got, frame) {
			t.Fatalf("frame %d changed in flight", i)
		}
	}

	if app.NextSeq() != 200 {
		t.Fatalf("NextSeq = %d, want 200", app.NextSeq())
	}
}

// Lane 0 must be one constant size on the wire, whatever happened in the
// house. Fixed overhead is what keeps the padding meaningful once encryption
// is applied.
func TestTransportOverheadIsFixed(t *testing.T) {
	app, _ := connect(t)

	frames := [][]byte{
		tickFrame(t),
		deltaFrame(t, map[string]int{}),
		deltaFrame(t, map[string]int{"2": 1, "3": 2, "4": 3, "5": 4, "6": 5}),
	}

	for _, frame := range frames {
		wire, err := app.Encrypt(frame)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if len(wire) != 512+TransportOverhead {
			t.Fatalf("wire is %d bytes, want %d", len(wire), 512+TransportOverhead)
		}
	}
}

func TestTransportRejectsTampering(t *testing.T) {
	tests := []struct {
		name string
		bend func(wire []byte) []byte
		want error
	}{
		{"flipped bit in the body", func(w []byte) []byte { w[100] ^= 0x01; return w }, ErrNoiseAuth},
		{"truncated tag", func(w []byte) []byte { return w[:400] }, ErrNoiseAuth},
		{"forged tag", func(w []byte) []byte { w[len(w)-1] ^= 0x80; return w }, ErrNoiseAuth},
		// The sequence number is authenticated, so a captured message cannot be
		// renumbered into the acceptable window.
		{"renumbered", func(w []byte) []byte { w[SeqBytes-1] = 9; return w }, ErrNoiseAuth},
		{"too short for a sequence number and a tag", func([]byte) []byte {
			return make([]byte, TransportOverhead-1)
		}, ErrNoiseMessage},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, box := connect(t)
			wire, err := app.Encrypt(tickFrame(t))
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			if _, err := box.Decrypt(tc.bend(wire)); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReplayRejectsAFrameAlreadyAccepted(t *testing.T) {
	app, box := connect(t)
	wire, err := app.Encrypt(tickFrame(t))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if _, err := box.Decrypt(wire); err != nil {
		t.Fatalf("first Decrypt: %v", err)
	}
	if _, err := box.Decrypt(wire); !errors.Is(err, ErrReplay) {
		t.Fatalf("err = %v, want ErrReplay", err)
	}
}

func TestReplayRejectsAnOldFrameAfterTheStreamMovedOn(t *testing.T) {
	app, box := connect(t)

	first, err := app.Encrypt(tickFrame(t))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := box.Decrypt(first); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	for i := range 10 {
		wire, err := app.Encrypt(deltaFrame(t, map[string]int{"2": i}))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if _, err := box.Decrypt(wire); err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
	}

	if _, err := box.Decrypt(first); !errors.Is(err, ErrReplay) {
		t.Fatalf("err = %v, want ErrReplay", err)
	}
}

// A DataChannel may reorder. Dropping a late frame would cost a reading for no
// reason, so the window forgives it.
func TestReplayAcceptsOutOfOrderInsideTheWindow(t *testing.T) {
	app, box := connect(t)

	wire := make([][]byte, 5)
	for i := range wire {
		w, err := app.Encrypt(deltaFrame(t, map[string]int{"2": i}))
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		wire[i] = w
	}

	if _, err := box.Decrypt(wire[4]); err != nil {
		t.Fatalf("Decrypt newest: %v", err)
	}
	for _, i := range []int{0, 1, 2, 3} {
		if _, err := box.Decrypt(wire[i]); err != nil {
			t.Fatalf("Decrypt %d: %v", i, err)
		}
	}
}

func TestReplayRejectsAFrameOlderThanTheWindow(t *testing.T) {
	app, box := connect(t)

	first, err := app.Encrypt(tickFrame(t))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	for i := range ReplayWindow + 1 {
		if _, err := app.Encrypt(deltaFrame(t, map[string]int{"2": i})); err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
	}

	latest, err := app.Encrypt(deltaFrame(t, map[string]int{"2": 99}))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := box.Decrypt(latest); err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	if _, err := box.Decrypt(first); !errors.Is(err, ErrReplay) {
		t.Fatalf("err = %v, want ErrReplay", err)
	}
}

// If a failed decryption moved the window, anyone able to write to the carrier
// could push it far ahead and lock the real peer out.
func TestAnInjectedFrameCannotBurnSequenceNumbers(t *testing.T) {
	app, box := connect(t)

	forged := make([]byte, TransportOverhead+512)
	forged[SeqBytes-1] = 200
	if _, err := box.Decrypt(forged); err == nil {
		t.Fatal("a forged frame must not authenticate")
	}

	frame := tickFrame(t)
	wire, err := app.Encrypt(frame)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := box.Decrypt(wire)
	if err != nil {
		t.Fatalf("the real peer was locked out: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatal("frame changed in flight")
	}
}

func TestTransportFailsRatherThanReusingANonce(t *testing.T) {
	send := NewCipherState(bytes.Repeat([]byte{3}, KeyBytes))
	recv := NewCipherState(bytes.Repeat([]byte{4}, KeyBytes))
	if err := send.SetNonce(MaxNonce - 1); err != nil {
		t.Fatalf("SetNonce: %v", err)
	}

	transport := NewTransport(&Session{
		Send:          send,
		Recv:          recv,
		HandshakeHash: make([]byte, HashBytes),
		RemoteStatic:  make([]byte, DHBytes),
	})

	if _, err := transport.Encrypt(tickFrame(t)); err != nil {
		t.Fatalf("last usable nonce: %v", err)
	}
	if _, err := transport.Encrypt(tickFrame(t)); !errors.Is(err, ErrNonceExhausted) {
		t.Fatalf("err = %v; the counter must not wrap", err)
	}
}

func TestTransportIsUnusableAfterClose(t *testing.T) {
	app, _ := connect(t)
	app.Close()

	if _, err := app.Encrypt(tickFrame(t)); !errors.Is(err, ErrNoiseClosed) {
		t.Fatalf("Encrypt err = %v, want ErrNoiseClosed", err)
	}
	if _, err := app.Decrypt(make([]byte, 600)); !errors.Is(err, ErrNoiseClosed) {
		t.Fatalf("Decrypt err = %v, want ErrNoiseClosed", err)
	}
}

// The relay sees every byte. What it must not be able to do is tell a house at
// full tilt from a house asleep.
func TestAPassiveObserverLearnsOnlyThatAFrameWentBy(t *testing.T) {
	busy := deltaFrame(t, map[string]int{"2": 11400, "3": 6200, "4": -4200, "5": 875, "6": 13400})
	quiet := tickFrame(t)

	if len(busy) != len(quiet) {
		t.Fatalf("frames differ in length before encryption: %d and %d", len(busy), len(quiet))
	}

	app, _ := connect(t)
	a, err := app.Encrypt(busy)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	b, err := app.Encrypt(quiet)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("wire lengths differ: %d and %d", len(a), len(b))
	}

	// The same frame twice must not produce the same bytes; only the counter
	// repeats a pattern, and a counter is a counter.
	c, err := app.Encrypt(busy)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(a[SeqBytes:], c[SeqBytes:]) {
		t.Fatal("the same frame encrypted to the same ciphertext twice")
	}

	// 512 identical input bytes. A run of eight zeros out of ChaCha has
	// probability around 2^-58, so this fails on a leak, not on luck.
	zeros, err := app.Encrypt(make([]byte, 512))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	run, longest := 0, 0
	for _, by := range zeros[SeqBytes:] {
		if by == 0 {
			run++
		} else {
			run = 0
		}
		if run > longest {
			longest = run
		}
	}
	if longest >= 8 {
		t.Fatalf("a run of %d zero bytes survived encryption", longest)
	}
}
