package homelinksession

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
)

type sessionTestIdentity struct {
	privateKey *ecdsa.PrivateKey
	publicKey  []byte
}

func (i sessionTestIdentity) GatewayID() string { return "0123dca63201f838f7" }
func (i sessionTestIdentity) PublicKey() []byte { return append([]byte(nil), i.publicKey...) }
func (i sessionTestIdentity) Sign(message []byte) ([]byte, error) {
	digest := sha256.Sum256(message)
	r, s, err := ecdsa.Sign(rand.Reader, i.privateKey, digest[:])
	if err != nil {
		return nil, err
	}
	signature := make([]byte, gatewayidentity.SignatureBytes)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signature, nil
}

func TestSessionHandshakeSignatureAndBidirectionalEncryption(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	identity := newSessionTestIdentity(t)
	manager, err := newManager(identity, rand.Reader, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	browserPrivate, hello := newBrowserHello(t, identity)
	accept, session, err := manager.Accept(hello)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := wire.SessionAcceptMessage(accept)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(accept.Signature)
	if err != nil || !gatewayidentity.Verify(identity.publicKey, transcript, signature) {
		t.Fatal("session accept signature is invalid")
	}
	handle, _ := gatewayidentity.RouteHandle(identity.publicKey)
	if accept.RouteHandle != handle || accept.BrowserKey != hello.BrowserKey ||
		accept.GatewayID != identity.GatewayID() ||
		accept.BrowserNonce != hello.BrowserNonce ||
		accept.ExpiresAtMS != now.Add(MaxSessionLifetime).UnixMilli() {
		t.Fatalf("session accept lost binding: %+v", accept)
	}

	browserOutbound, browserInbound := browserAEADs(t, browserPrivate, accept, transcript)
	request := []byte(`{"type":"session.confirm"}`)
	requestCiphertext := browserOutbound.Seal(nil, nonce(1), request,
		session.additionalData("browser-to-gateway", 1))
	sealed := wire.Sealed{
		Version: wire.Version, Type: wire.TypeSealed,
		StreamID: hello.StreamID, Sequence: 1,
		Ciphertext: base64.RawURLEncoding.EncodeToString(requestCiphertext),
	}
	plaintext, err := session.Decrypt(sealed)
	if err != nil || string(plaintext) != string(request) {
		t.Fatalf("decrypt = %q, %v", plaintext, err)
	}

	response := []byte(`{"type":"session.ready"}`)
	encrypted, err := session.Encrypt(response)
	if err != nil {
		t.Fatal(err)
	}
	responseCiphertext, _ := base64.RawURLEncoding.DecodeString(encrypted.Ciphertext)
	opened, err := browserInbound.Open(nil, nonce(encrypted.Sequence),
		responseCiphertext, session.additionalData("gateway-to-browser", encrypted.Sequence))
	if err != nil || string(opened) != string(response) {
		t.Fatalf("browser decrypt = %q, %v", opened, err)
	}
}

func TestVerifyAcceptRejectsCrossSiteAndSignatureMutation(t *testing.T) {
	identity := newSessionTestIdentity(t)
	manager, err := NewManager(identity)
	if err != nil {
		t.Fatal(err)
	}
	_, hello := newBrowserHello(t, identity)
	accept, _, err := manager.Accept(hello)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAccept(identity.GatewayID(), hello.RouteHandle, accept); err != nil {
		t.Fatalf("valid accept = %v", err)
	}
	if err := VerifyAccept("0123aabbcc01ddeeff", hello.RouteHandle, accept); err == nil {
		t.Fatal("cross-site accept was accepted")
	}
	tampered := accept
	tampered.GatewayID = "0123aabbcc01ddeeff"
	if err := VerifyAccept(tampered.GatewayID, hello.RouteHandle, tampered); err == nil {
		t.Fatal("unsigned gateway mutation was accepted")
	}
}

func TestEncryptHonorsExactWireFrameBoundaryWithoutAdvancingSequence(t *testing.T) {
	identity := newSessionTestIdentity(t)
	manager, _ := NewManager(identity)
	_, hello := newBrowserHello(t, identity)
	_, session, err := manager.Accept(hello)
	if err != nil {
		t.Fatal(err)
	}
	maximum := make([]byte, wire.MaxPlaintextBytes)
	message, err := session.Encrypt(maximum)
	if err != nil {
		t.Fatalf("maximum plaintext: %v", err)
	}
	data, err := wire.Encode(message, wire.MaxSealedFrameBytes)
	if err != nil || len(data) > wire.MaxSealedFrameBytes {
		t.Fatalf("maximum frame = %d, %v", len(data), err)
	}
	if _, err := session.Encrypt(make([]byte, wire.MaxPlaintextBytes+1)); err == nil {
		t.Fatal("oversized plaintext was accepted")
	}
	next, err := session.Encrypt([]byte("next"))
	if err != nil {
		t.Fatal(err)
	}
	if next.Sequence != 2 {
		t.Fatalf("failed encryption advanced sequence to %d", next.Sequence)
	}
}

func TestSessionRejectsReplayTamperWrongStreamAndExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	identity := newSessionTestIdentity(t)
	manager, _ := newManager(identity, rand.Reader, func() time.Time { return now })
	browserPrivate, hello := newBrowserHello(t, identity)
	accept, session, err := manager.Accept(hello)
	if err != nil {
		t.Fatal(err)
	}
	transcript, _ := wire.SessionAcceptMessage(accept)
	browserOutbound, _ := browserAEADs(t, browserPrivate, accept, transcript)
	ciphertext := browserOutbound.Seal(nil, nonce(1), []byte("confirm"),
		session.additionalData("browser-to-gateway", 1))
	message := wire.Sealed{
		Version: wire.Version, Type: wire.TypeSealed,
		StreamID: hello.StreamID, Sequence: 1,
		Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext),
	}

	tampered := message
	raw, _ := base64.RawURLEncoding.DecodeString(tampered.Ciphertext)
	raw[len(raw)-1] ^= 1
	tampered.Ciphertext = base64.RawURLEncoding.EncodeToString(raw)
	if _, err := session.Decrypt(tampered); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
	if _, err := session.Decrypt(message); err != nil {
		t.Fatalf("valid frame after tamper = %v", err)
	}
	if _, err := session.Decrypt(message); err == nil {
		t.Fatal("replayed sequence was accepted")
	}
	wrongStream := message
	wrongStream.StreamID = rawURL(wire.StreamIDBytes, 8)
	wrongStream.Sequence = 2
	if _, err := session.Decrypt(wrongStream); err == nil {
		t.Fatal("wrong stream was accepted")
	}
	now = now.Add(MaxSessionLifetime)
	if _, err := session.Encrypt([]byte("late")); err == nil {
		t.Fatal("expired session encrypted a response")
	}
}

func TestSessionRejectsRouteMismatch(t *testing.T) {
	identity := newSessionTestIdentity(t)
	manager, _ := NewManager(identity)
	_, hello := newBrowserHello(t, identity)
	otherIdentity := newSessionTestIdentity(t)
	hello.RouteHandle, _ = gatewayidentity.RouteHandle(otherIdentity.publicKey)
	if _, _, err := manager.Accept(hello); err == nil {
		t.Fatal("wrong route handle was accepted")
	}
}

func newBrowserHello(t *testing.T, identity sessionTestIdentity) (*ecdh.PrivateKey, wire.SessionHello) {
	t.Helper()
	privateKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := rawECDHPublicKey(privateKey.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	handle, _ := gatewayidentity.RouteHandle(identity.publicKey)
	return privateKey, wire.SessionHello{
		Version: wire.Version, Type: wire.TypeSessionHello,
		ConnectionID: rawURL(wire.ConnectionIDBytes, 4), RouteGeneration: 1,
		RouteHandle: handle, StreamID: rawURL(wire.StreamIDBytes, 5),
		BrowserKey:   base64.RawURLEncoding.EncodeToString(raw),
		BrowserNonce: rawURL(wire.SessionNonceBytes, 6),
	}
}

func browserAEADs(
	t *testing.T,
	privateKey *ecdh.PrivateKey,
	accept wire.SessionAccept,
	transcript []byte,
) (browserOutbound, browserInbound cipher.AEAD) {
	t.Helper()
	gatewayRaw, err := base64.RawURLEncoding.DecodeString(accept.GatewayEphemeralKey)
	if err != nil {
		t.Fatal(err)
	}
	gatewayKey, err := parseECDHPublicKey(gatewayRaw)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := privateKey.ECDH(gatewayKey)
	if err != nil {
		t.Fatal(err)
	}
	outbound, inbound, err := deriveAEADs(shared, transcript)
	if err != nil {
		t.Fatal(err)
	}
	return outbound, inbound
}

func newSessionTestIdentity(t *testing.T) sessionTestIdentity {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := make([]byte, gatewayidentity.PublicKeyBytes)
	privateKey.X.FillBytes(publicKey[:32])
	privateKey.Y.FillBytes(publicKey[32:])
	return sessionTestIdentity{privateKey: privateKey, publicKey: publicKey}
}

func rawURL(length int, fill byte) string {
	raw := make([]byte, length)
	for i := range raw {
		raw[i] = fill
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
