package appwire

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// The Cacophony vector for this exact protocol, copied verbatim from the
// reference suite every other implementation tests against (snow:
// tests/vectors/cacophony.txt), and the same one the app checks itself with.
//
// A round trip inside this package only proves it agrees with itself. This
// proves it agrees with every other Noise implementation, which is the claim
// that matters when the peer is somebody else's code.
var cacophony = struct {
	protocolName    string
	prologue        string
	initStatic      string
	initRemoteStatc string
	respStatic      string
	respEphemeral   string
	handshakeHash   string
	messages        []struct{ payload, ciphertext string }
}{
	protocolName:    "Noise_IK_25519_ChaChaPoly_SHA256",
	prologue:        "4a6f686e2047616c74",
	initStatic:      "e61ef9919cde45dd5f82166404bd08e38bceb5dfdfded0a34c8df7ed542214d1",
	initRemoteStatc: "31e0303fd6418d2f8c0e78b91f22e8caed0fbe48656dcf4767e4834f701b8f62",
	respStatic:      "4a3acbfdb163dec651dfa3194dece676d437029c62a408b4c5ea9114246e4893",
	respEphemeral:   "bbdb4cdbd309f1a1f2e1456967fe288cadd6f712d65dc7b7793d5e63da6b375b",
	handshakeHash:   "0b0f68fb0c27e03ce9b97565995ed4838cc0581b762ef72b062f6a546419fad7",
	messages: []struct{ payload, ciphertext string }{
		{"4c756477696720766f6e204d69736573", "ca35def5ae56cec33dc2036731ab14896bc4c75dbb07a61f879f8e3afa4c7944718da798efbcd91528520204f904b9bd6c7413dccdc214d951e15253e39987f18146e8cd0873654207148333479d4d16c289f0294b29960a72f48e0b7bba2e89083169825e59642148d492020664ccf7"},
		{"4d757272617920526f746862617264", "95ebc60d2b1fa672c1f46a8aa265ef51bfe38e7ccb39ec5be34069f1448088435361e70b2ed446e6c9ec387d1d6b3b840f194e373979d241b203c4acafccf5"},
		{"462e20412e20486179656b", "050e9f3c8fac16b68dbce8f8c4bfbf6617c897f9ada4aa29aa19c8"},
		{"4361726c204d656e676572", "344233a6cabb7141d80f3da2fedc311d9646bbb0f505afe403a667"},
		{"4a65616e2d426170746973746520536179", "62cdeeb172ad7ade7aa7d9e069da5790f12331bfa00177787a1d0810c67dc3b2b4"},
		{"457567656e2042f6686d20766f6e2042617765726b", "029bead1b40992327044d409d9a1f3ad8f36c3c452775d557e18bbeb2e8dfcead32d514024"},
	},
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func keyFromHex(t *testing.T, s string) KeyPair {
	t.Helper()
	k, err := KeyPairFromSecret(unhex(t, s))
	if err != nil {
		t.Fatalf("KeyPairFromSecret: %v", err)
	}
	return k
}

func TestProtocolNameMatchesTheVector(t *testing.T) {
	if ProtocolName != cacophony.protocolName {
		t.Fatalf("ProtocolName = %q, want %q", ProtocolName, cacophony.protocolName)
	}
}

func TestCacophonyVectorAsResponder(t *testing.T) {
	respEphemeral := keyFromHex(t, cacophony.respEphemeral)
	responder, err := NewResponder(HandshakeOptions{
		StaticKey: keyFromHex(t, cacophony.respStatic),
		Prologue:  unhex(t, cacophony.prologue),
		Ephemeral: &respEphemeral,
	})
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}

	// The vector's responder static must be the key the initiator pinned;
	// otherwise the rest of this test is checking the wrong thing.
	if got := hex.EncodeToString(keyFromHex(t, cacophony.respStatic).Public()); got != cacophony.initRemoteStatc {
		t.Fatalf("responder public = %s, want %s", got, cacophony.initRemoteStatc)
	}

	payload, err := responder.ReadMessage(unhex(t, cacophony.messages[0].ciphertext))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got := hex.EncodeToString(payload); got != cacophony.messages[0].payload {
		t.Fatalf("message 1 payload = %s, want %s", got, cacophony.messages[0].payload)
	}

	message2, err := responder.WriteMessage(unhex(t, cacophony.messages[1].payload))
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if got := hex.EncodeToString(message2); got != cacophony.messages[1].ciphertext {
		t.Fatalf("message 2 = %s, want %s", got, cacophony.messages[1].ciphertext)
	}

	session, err := responder.Split()
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if got := hex.EncodeToString(session.HandshakeHash); got != cacophony.handshakeHash {
		t.Fatalf("handshake hash = %s, want %s", got, cacophony.handshakeHash)
	}

	// The box learns who the app is from the handshake rather than on trust.
	wantRemote := keyFromHex(t, cacophony.initStatic).Public()
	if !bytes.Equal(session.RemoteStatic, wantRemote) {
		t.Fatalf("remote static = %x, want %x", session.RemoteStatic, wantRemote)
	}

	// Transport messages alternate, starting with the initiator.
	for i, msg := range cacophony.messages[2:] {
		fromInitiator := i%2 == 0
		if fromInitiator {
			plaintext, err := session.Recv.DecryptWithAD(nil, unhex(t, msg.ciphertext))
			if err != nil {
				t.Fatalf("transport %d: %v", i, err)
			}
			if got := hex.EncodeToString(plaintext); got != msg.payload {
				t.Fatalf("transport %d plaintext = %s, want %s", i, got, msg.payload)
			}
			continue
		}

		ciphertext, err := session.Send.EncryptWithAD(nil, unhex(t, msg.payload))
		if err != nil {
			t.Fatalf("transport %d: %v", i, err)
		}
		if got := hex.EncodeToString(ciphertext); got != msg.ciphertext {
			t.Fatalf("transport %d ciphertext = %s, want %s", i, got, msg.ciphertext)
		}
	}
}

// The whole reason the pattern is IK. The app pinned the box's static key off
// the QR code, so a relay standing in the middle has no key that satisfies the
// first decryption.
func TestARelayCannotPresentItselfAsABox(t *testing.T) {
	impostor, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}

	responder, err := NewResponder(HandshakeOptions{
		StaticKey: impostor,
		Prologue:  unhex(t, cacophony.prologue),
	})
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}

	_, err = responder.ReadMessage(unhex(t, cacophony.messages[0].ciphertext))
	if !errors.Is(err, ErrNoiseAuth) {
		t.Fatalf("err = %v, want ErrNoiseAuth and nothing more specific", err)
	}
}

func TestPrologueMustMatch(t *testing.T) {
	responder, err := NewResponder(HandshakeOptions{
		StaticKey: keyFromHex(t, cacophony.respStatic),
		Prologue:  []byte("a different prologue"),
	})
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}

	if _, err := responder.ReadMessage(unhex(t, cacophony.messages[0].ciphertext)); !errors.Is(err, ErrNoiseAuth) {
		t.Fatalf("err = %v, want ErrNoiseAuth", err)
	}
}

func TestRejectsAnAlteredHandshakeMessage(t *testing.T) {
	responder, err := NewResponder(HandshakeOptions{
		StaticKey: keyFromHex(t, cacophony.respStatic),
		Prologue:  unhex(t, cacophony.prologue),
	})
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}

	message := unhex(t, cacophony.messages[0].ciphertext)
	message[40] ^= 0x01

	if _, err := responder.ReadMessage(message); !errors.Is(err, ErrNoiseAuth) {
		t.Fatalf("err = %v, want ErrNoiseAuth", err)
	}
}

func TestRejectsATruncatedHandshakeMessage(t *testing.T) {
	responder, err := NewResponder(HandshakeOptions{StaticKey: keyFromHex(t, cacophony.respStatic)})
	if err != nil {
		t.Fatalf("NewResponder: %v", err)
	}

	if _, err := responder.ReadMessage(make([]byte, 40)); !errors.Is(err, ErrNoiseMessage) {
		t.Fatalf("err = %v, want ErrNoiseMessage rather than a read past the end", err)
	}
}

func TestHandshakeStateMachine(t *testing.T) {
	newResponder := func(t *testing.T) *HandshakeState {
		t.Helper()
		respEphemeral := keyFromHex(t, cacophony.respEphemeral)
		h, err := NewResponder(HandshakeOptions{
			StaticKey: keyFromHex(t, cacophony.respStatic),
			Prologue:  unhex(t, cacophony.prologue),
			Ephemeral: &respEphemeral,
		})
		if err != nil {
			t.Fatalf("NewResponder: %v", err)
		}
		return h
	}

	t.Run("refuses to write before reading", func(t *testing.T) {
		if _, err := newResponder(t).WriteMessage(nil); !errors.Is(err, ErrNoiseState) {
			t.Fatalf("err = %v, want ErrNoiseState", err)
		}
	})

	t.Run("refuses to read twice", func(t *testing.T) {
		h := newResponder(t)
		if _, err := h.ReadMessage(unhex(t, cacophony.messages[0].ciphertext)); err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		if _, err := h.ReadMessage(unhex(t, cacophony.messages[0].ciphertext)); !errors.Is(err, ErrNoiseState) {
			t.Fatalf("err = %v, want ErrNoiseState", err)
		}
	})

	t.Run("refuses to split before the pattern is done", func(t *testing.T) {
		if _, err := newResponder(t).Split(); !errors.Is(err, ErrNoiseState) {
			t.Fatalf("err = %v, want ErrNoiseState", err)
		}
	})

	// Two splits would mint two send ciphers with the same key, both at nonce
	// 0: an observer gets the XOR of two plaintexts and Poly1305 tags become
	// forgeable. Reconnection is the code that naturally reaches for it, so
	// the refusal lives here rather than in every caller.
	t.Run("refuses to split twice", func(t *testing.T) {
		h := newResponder(t)
		if _, err := h.ReadMessage(unhex(t, cacophony.messages[0].ciphertext)); err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		if _, err := h.WriteMessage(unhex(t, cacophony.messages[1].payload)); err != nil {
			t.Fatalf("WriteMessage: %v", err)
		}
		if !h.Complete() {
			t.Fatal("handshake should be complete")
		}
		if _, err := h.Split(); err != nil {
			t.Fatalf("first Split: %v", err)
		}
		if _, err := h.Split(); !errors.Is(err, ErrNoiseState) {
			t.Fatalf("second Split err = %v, want ErrNoiseState", err)
		}
	})
}

func TestRejectsAStaticKeyOfTheWrongSize(t *testing.T) {
	if _, err := KeyPairFromSecret(make([]byte, 16)); !errors.Is(err, ErrNoiseKey) {
		t.Fatalf("err = %v, want ErrNoiseKey rather than deriving from it", err)
	}
}

// ChaCha20-Poly1305 fails catastrophically on nonce reuse, so the counter
// fails at the end rather than wrapping.
func TestNonceExhaustion(t *testing.T) {
	c := NewCipherState(bytes.Repeat([]byte{7}, KeyBytes))
	if err := c.SetNonce(MaxNonce - 1); err != nil {
		t.Fatalf("SetNonce: %v", err)
	}

	if _, err := c.EncryptWithAD(nil, make([]byte, 8)); err != nil {
		t.Fatalf("last usable nonce: %v", err)
	}
	if c.Nonce() != MaxNonce {
		t.Fatalf("nonce = %d, want %d", c.Nonce(), MaxNonce)
	}
	if _, err := c.EncryptWithAD(nil, make([]byte, 8)); !errors.Is(err, ErrNonceExhausted) {
		t.Fatalf("err = %v; the reserved nonce must never encrypt", err)
	}
}

func TestSetNonceRefusesTheReservedValue(t *testing.T) {
	c := NewCipherState(bytes.Repeat([]byte{7}, KeyBytes))
	if err := c.SetNonce(MaxNonce); !errors.Is(err, ErrNonceExhausted) {
		t.Fatalf("err = %v, want ErrNonceExhausted", err)
	}
}

func TestDestroyedCipherFailsRatherThanEmittingPlaintext(t *testing.T) {
	c := NewCipherState(bytes.Repeat([]byte{7}, KeyBytes))
	c.Destroy()

	if c.HasKey() {
		t.Error("key survived Destroy")
	}
	if _, err := c.EncryptWithAD(nil, make([]byte, 8)); !errors.Is(err, ErrNoiseClosed) {
		t.Fatalf("err = %v, want ErrNoiseClosed", err)
	}
}
