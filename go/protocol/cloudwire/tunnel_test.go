package cloudwire

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/protocol/appwire"
)

// The largest d fits the frame limit on both sockets, with the longest
// device id the box makes.
func TestTheLargestTunFitsItsFrame(t *testing.T) {
	if TunDataMax != appwire.BulkBuckets[len(appwire.BulkBuckets)-1]+appwire.TransportOverhead {
		t.Fatalf("TunDataMax %d is not the largest bulk bucket in a transport message", TunDataMax)
	}
	body, err := Marshal(Tun{Dev: strings.Repeat("x", 8), Sid: ^uint32(0), D: make([]byte, TunDataMax)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Envelope{T: MsgTun, B: body}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	frame, err := EncodeFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) > MaxTunFrame {
		t.Fatalf("a full tun frame is %d bytes, over MaxTunFrame %d", len(frame), MaxTunFrame)
	}
	if MaxTunFrame+appwire.TransportOverhead > MaxPayload {
		t.Fatal("a tun on the app session would not fit one Noise message")
	}
}

// The downlink grows by tun and x alone: the two carry ciphertext the box
// judges itself.
func TestTheDownlinkIsSevenTypes(t *testing.T) {
	want := []string{MsgChallenge, MsgHelloOK, MsgWatch, MsgResync, MsgPurged, MsgTun, MsgX}
	if !slices.Equal(Downlink, want) {
		t.Fatalf("Downlink is %v, want %v", Downlink, want)
	}
}

// A phone's tun carries no device id, and one it adds is not read.
func TestAnAppTunHasNoDevice(t *testing.T) {
	forged, err := Marshal(map[string]any{"dev": "someone", "sid": 3, "d": []byte{1}})
	if err != nil {
		t.Fatal(err)
	}
	var got AppTun
	if err := Unmarshal(forged, &got); err != nil {
		t.Fatal(err)
	}
	if got.Sid != 3 || !bytes.Equal(got.D, []byte{1}) {
		t.Fatalf("decoded %+v", got)
	}
	var box Tun
	if err := Unmarshal(forged, &box); err != nil || box.Dev != "someone" {
		t.Fatalf("the uplink shape lost its dev: %+v %v", box, err)
	}
}

func TestEndedIsAnEmptyD(t *testing.T) {
	if !(Tun{D: nil}).Ended() || !(Tun{D: []byte{}}).Ended() || (Tun{D: []byte{0}}).Ended() {
		t.Fatal("only an empty d ends a session")
	}
}

// The device id is the box's: the first eight characters of the key in
// base64url.
func TestDeviceIDIsTheBoxs(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 32)
	if got, want := DeviceID(key), base64.RawURLEncoding.EncodeToString(key)[:8]; got != want {
		t.Fatalf("DeviceID = %q, want %q", got, want)
	}
}

// The pairing hash binds the home and the code, and nothing else.
func TestPairingHash(t *testing.T) {
	home := "fjzyr67buzpyncqmjliwgezvde"
	code := bytes.Repeat([]byte{0x88}, 16)
	h := PairingHash(home, code)
	if len(h) != PairingHashBytes {
		t.Fatalf("%d bytes", len(h))
	}
	// The known answer is in cloud-app-vectors.json, under pairing.
	if bytes.Equal(h, PairingHash("ajzyr67buzpyncqmjliwgezvde", code)) {
		t.Fatal("two homes share a pairing hash")
	}
	other := bytes.Clone(code)
	other[0] ^= 1
	if bytes.Equal(h, PairingHash(home, other)) {
		t.Fatal("two codes share a pairing hash")
	}
	if hex.EncodeToString(h) == hex.EncodeToString(code) {
		t.Fatal("the hash is the code")
	}
}
