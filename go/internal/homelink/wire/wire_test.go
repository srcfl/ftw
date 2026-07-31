package wire

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
)

func TestMachineHelloBindsRouteToKey(t *testing.T) {
	publicKey := testPublicKey(t)
	handle, err := gatewayidentity.RouteHandle(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	message := MachineHello{
		Version: Version, Type: TypeMachineHello,
		GatewayID: "0123dca63201f838f7", RouteHandle: handle,
		PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
	}
	data, err := Encode(message, MaxHandshakeBytes)
	if err != nil {
		t.Fatal(err)
	}
	got, gotKey, err := DecodeMachineHello(data)
	if err != nil {
		t.Fatal(err)
	}
	if got != message || string(gotKey) != string(publicKey) {
		t.Fatalf("decoded hello = (%+v, %x)", got, gotKey)
	}

	message.RouteHandle = strings.Repeat("A", gatewayidentity.RouteHandleSize)
	data, _ = json.Marshal(message)
	if _, _, err := DecodeMachineHello(data); err == nil {
		t.Fatal("route handle unrelated to public key was accepted")
	}
}

func TestStrictJSONRejectsDuplicateUnknownTrailingAndUnsafeText(t *testing.T) {
	publicKey := testPublicKey(t)
	handle, _ := gatewayidentity.RouteHandle(publicKey)
	key := base64.RawURLEncoding.EncodeToString(publicKey)
	valid := `{"version":1,"type":"machine.hello","gateway_id":"0123dca63201f838f7","route_handle":"` +
		handle + `","public_key":"` + key + `"}`
	for name, data := range map[string]string{
		"duplicate": strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"unknown":   strings.Replace(valid, `"version":1`, `"version":1,"extra":true`, 1),
		"trailing":  valid + `{}`,
		"bidi":      strings.Replace(valid, `"machine.hello"`, `"machine.\u202ehello"`, 1),
		"separator": strings.Replace(valid, `"machine.hello"`, `"machine.\u2028hello"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := DecodeMachineHello([]byte(data)); err == nil {
				t.Fatal("invalid wire JSON was accepted")
			}
		})
	}
}

func TestMachineProofMessageBindsEveryPublicField(t *testing.T) {
	publicKey := testPublicKey(t)
	handle, _ := gatewayidentity.RouteHandle(publicKey)
	proof := MachineProof{
		Version: Version, Type: TypeMachineProof,
		ConnectionID: testRawURL(ConnectionIDBytes, 1),
		GatewayID:    "0123dca63201f838f7", RouteHandle: handle,
		PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		Nonce:     testRawURL(MachineNonceBytes, 2), ExpiresAtMS: 1_800_000_000_000,
		Signature: testRawURL(gatewayidentity.SignatureBytes, 3),
	}
	message, err := MachineProofMessage(proof)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		MachineProofDomain, proof.ConnectionID, proof.GatewayID,
		proof.RouteHandle, proof.PublicKey, proof.Nonce, "1800000000000",
	} {
		if !strings.Contains(string(message), value) {
			t.Fatalf("proof transcript does not bind %q: %q", value, message)
		}
	}
}

func TestSealedFrameBoundsAndCanonicalEncoding(t *testing.T) {
	message := Sealed{
		Version: Version, Type: TypeSealed,
		StreamID: testRawURL(StreamIDBytes, 4), Sequence: 1,
		Ciphertext: base64.RawURLEncoding.EncodeToString([]byte("ciphertext")),
	}
	data, _ := json.Marshal(message)
	if _, err := DecodeSealed(data); err != nil {
		t.Fatal(err)
	}
	message.Sequence = 0
	data, _ = json.Marshal(message)
	if _, err := DecodeSealed(data); err == nil {
		t.Fatal("zero sequence was accepted")
	}
	message.Sequence = 1
	message.Ciphertext = base64.URLEncoding.EncodeToString([]byte("padme"))
	data, _ = json.Marshal(message)
	if _, err := DecodeSealed(data); err == nil {
		t.Fatal("padded ciphertext was accepted")
	}
	message.Ciphertext = base64.RawURLEncoding.EncodeToString(make([]byte, MaxCiphertextBytes+1))
	data, _ = json.Marshal(message)
	if _, err := DecodeSealed(data); err == nil {
		t.Fatal("oversized ciphertext was accepted")
	}
	message.Sequence = ^uint64(0)
	message.Ciphertext = base64.RawURLEncoding.EncodeToString(make([]byte, MaxCiphertextBytes))
	data, err := Encode(message, MaxSealedFrameBytes)
	if err != nil {
		t.Fatalf("maximum ciphertext envelope: %v", err)
	}
	if len(data) > MaxSealedFrameBytes {
		t.Fatalf("maximum ciphertext envelope = %d bytes", len(data))
	}
}

func TestMessageTypeRejectsUnsupportedVersionAndType(t *testing.T) {
	for _, data := range []string{
		`{"version":2,"type":"sealed"}`,
		`{"version":1,"type":"proxy"}`,
	} {
		if _, err := MessageType([]byte(data), MaxHandshakeBytes); err == nil {
			t.Fatalf("unsupported header accepted: %s", data)
		}
	}
}

func FuzzWireDecoders(f *testing.F) {
	f.Add([]byte(`{"version":1,"type":"sealed"}`))
	f.Add([]byte(`{"version":1,"type":"session.hello"}`))
	f.Add([]byte{0xff, 0x00, '{'})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = MessageType(data, MaxSealedFrameBytes)
		_, _, _ = DecodeMachineHello(data)
		_, _ = DecodeMachineChallenge(data)
		_, _, _, _ = DecodeMachineProof(data)
		_, _ = DecodeMachineReady(data)
		_, _ = DecodeStreamOpen(data)
		_, _ = DecodeStreamClose(data)
		_, _, _ = DecodeSessionHello(data)
		_, _, _, _, _ = DecodeSessionAccept(data)
		_, _ = DecodeSealed(data)
	})
}

func testPublicKey(t *testing.T) []byte {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	result := make([]byte, gatewayidentity.PublicKeyBytes)
	privateKey.X.FillBytes(result[:32])
	privateKey.Y.FillBytes(result[32:])
	return result
}

func testRawURL(length int, fill byte) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{fill}), length)))
}
