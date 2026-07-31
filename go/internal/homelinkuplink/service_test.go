package homelinkuplink

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/homelink"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
	"github.com/srcfl/ftw/go/internal/homelinkrelay"
)

type fakeTransport struct {
	frames  chan Frame
	accepts chan wire.SessionAccept
	sealed  chan wire.Sealed
	closes  chan wire.StreamClose
	done    chan struct{}
	once    sync.Once
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		frames: make(chan Frame, 8), accepts: make(chan wire.SessionAccept, 2),
		sealed: make(chan wire.Sealed, 2), closes: make(chan wire.StreamClose, 2),
		done: make(chan struct{}),
	}
}

func (f *fakeTransport) ReadFrame() (Frame, error) {
	select {
	case frame := <-f.frames:
		return frame, nil
	case <-f.done:
		return Frame{}, context.Canceled
	}
}
func (f *fakeTransport) SendSessionAccept(message wire.SessionAccept) error {
	f.accepts <- message
	return nil
}
func (f *fakeTransport) SendSealed(message wire.Sealed) error {
	f.sealed <- message
	return nil
}
func (f *fakeTransport) CloseStream(streamID, code string) error {
	f.closes <- wire.StreamClose{StreamID: streamID, Code: code}
	return nil
}
func (f *fakeTransport) Close() error {
	f.once.Do(func() { close(f.done) })
	return nil
}

func TestServiceCompletesOnlyEncryptedSessionConfirmation(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	service, err := NewService(identity)
	if err != nil {
		t.Fatal(err)
	}
	transport := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- service.serve(ctx, transport) }()

	handle, _ := gatewayidentity.RouteHandle(identity.publicKey)
	streamID := rawURL(wire.StreamIDBytes, 11)
	browserPrivate, browserKey := newBrowserECDH(t)
	hello := wire.SessionHello{
		Version: wire.Version, Type: wire.TypeSessionHello,
		ConnectionID: rawURL(wire.ConnectionIDBytes, 10), RouteGeneration: 1,
		RouteHandle: handle, StreamID: streamID,
		BrowserKey:   base64.RawURLEncoding.EncodeToString(browserKey),
		BrowserNonce: rawURL(wire.SessionNonceBytes, 12),
	}
	transport.frames <- Frame{Type: wire.TypeStreamOpen, Open: &wire.StreamOpen{
		Version: wire.Version, Type: wire.TypeStreamOpen,
		ConnectionID: hello.ConnectionID, RouteGeneration: hello.RouteGeneration,
		RouteHandle: handle, StreamID: streamID,
	}}
	transport.frames <- Frame{Type: wire.TypeSessionHello, SessionHello: &hello}
	accept := waitValue(t, transport.accepts)

	browserOutbound, browserInbound := browserServiceKeys(t, browserPrivate, accept)
	confirm := []byte(`{"version":1,"type":"session.confirm"}`)
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 1, confirm,
	)}
	ready := waitValue(t, transport.sealed)
	plaintext := browserOpen(t, browserInbound, accept, ready)
	if string(plaintext) != `{"version":1,"type":"session.ready"}` {
		t.Fatalf("session ready = %q", plaintext)
	}

	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 2, []byte(`{"version":1,"type":"read"}`),
	)}
	closed := waitValue(t, transport.closes)
	if closed.StreamID != streamID || closed.Code != "remote-disabled" {
		t.Fatalf("post-confirm application frame close = %+v", closed)
	}
	cancel()
	if err := <-result; !errorsIsCanceled(err) {
		t.Fatalf("serve after cancel = %v", err)
	}
}

func TestServiceRejectsDuplicateConfirmationFields(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	service, _ := NewService(identity)
	transport := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- service.serve(ctx, transport) }()

	handle, _ := gatewayidentity.RouteHandle(identity.publicKey)
	streamID := rawURL(wire.StreamIDBytes, 21)
	browserPrivate, browserKey := newBrowserECDH(t)
	hello := wire.SessionHello{
		Version: wire.Version, Type: wire.TypeSessionHello,
		ConnectionID: rawURL(wire.ConnectionIDBytes, 20), RouteGeneration: 1,
		RouteHandle: handle, StreamID: streamID,
		BrowserKey:   base64.RawURLEncoding.EncodeToString(browserKey),
		BrowserNonce: rawURL(wire.SessionNonceBytes, 22),
	}
	transport.frames <- Frame{Type: wire.TypeStreamOpen, Open: &wire.StreamOpen{
		Version: wire.Version, Type: wire.TypeStreamOpen,
		ConnectionID: hello.ConnectionID, RouteGeneration: hello.RouteGeneration,
		RouteHandle: handle, StreamID: streamID,
	}}
	transport.frames <- Frame{Type: wire.TypeSessionHello, SessionHello: &hello}
	accept := waitValue(t, transport.accepts)
	browserOutbound, _ := browserServiceKeys(t, browserPrivate, accept)
	duplicate := []byte(`{"version":1,"type":"session.confirm","type":"session.confirm"}`)
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 1, duplicate,
	)}
	closed := waitValue(t, transport.closes)
	if closed.Code != "invalid-confirmation" {
		t.Fatalf("duplicate confirmation close = %+v", closed)
	}
	cancel()
	<-result
}

func TestApplicationResponseEncodeFailureClosesStream(t *testing.T) {
	transport := newFakeTransport()
	state := &streamState{open: wire.StreamOpen{StreamID: "stream-1"}}
	(&Service{}).sendApplicationResponse(
		context.Background(), transport, state, nil, fmt.Errorf("encode failed"),
	)
	closed := waitValue(t, transport.closes)
	if closed.StreamID != "stream-1" || closed.Code != "response-failed" {
		t.Fatalf("application failure close = %+v", closed)
	}
	if state.canReply() {
		t.Fatal("failed application response left the stream open")
	}
}

func TestEncryptedSessionEndToEndThroughRelay(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	var readCalls atomic.Int32
	invites, err := homelinkrelay.NewStaticInvites([]homelinkrelay.StaticInvite{{
		GatewayID: uplinkTestGatewayID,
		PublicKey: base64.RawURLEncoding.EncodeToString(identity.publicKey),
	}})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := homelinkrelay.New(homelinkrelay.Options{Invites: invites})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(relay.Handler())
	defer server.Close()
	client, err := newClient(identity, wsURL(server.URL, "/v1/uplink"),
		websocket.DefaultDialer, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := client.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewServiceWithReads(identity, readExecutorFunc(func(
		_ context.Context,
		_ string,
		_ string,
		request homelink.ReadRequest,
		_ homelink.ReadBinding,
	) (homelink.ReadResponse, error) {
		readCalls.Add(1)
		if request.Scope != homelink.ScopePlanRead {
			t.Fatalf("relay read scope = %q", request.Scope)
		}
		return homelink.ReadResponse{
			Version: homelink.ReadContractVersion,
			Scope:   homelink.ScopePlanRead,
			Plan: &homelink.PlanReadResponse{
				Available: true, GeneratedAtMS: 1, Mode: "cost", HorizonSlots: 24,
			},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- service.Serve(ctx, connection) }()

	headers := http.Header{"Origin": []string{homelinkrelay.DefaultBrowserOrigin}}
	browser, _, err := websocket.DefaultDialer.Dial(
		wsURL(server.URL, "/v1/browser/"+connection.RouteHandle()), headers,
	)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer browser.Close()
	var opened wire.StreamOpen
	readTestJSON(t, browser, &opened)
	browserPrivate, browserKey := newBrowserECDH(t)
	hello := wire.SessionHello{
		Version: wire.Version, Type: wire.TypeSessionHello,
		ConnectionID: opened.ConnectionID, RouteGeneration: opened.RouteGeneration,
		RouteHandle: connection.RouteHandle(), StreamID: opened.StreamID,
		BrowserKey:   base64.RawURLEncoding.EncodeToString(browserKey),
		BrowserNonce: rawURL(wire.SessionNonceBytes, 31),
	}
	writeTestJSON(t, browser, hello)
	var accept wire.SessionAccept
	readTestJSON(t, browser, &accept)
	transcript, err := wire.SessionAcceptMessage(accept)
	if err != nil {
		t.Fatal(err)
	}
	signature, _ := base64.RawURLEncoding.DecodeString(accept.Signature)
	if !gatewayidentity.Verify(identity.publicKey, transcript, signature) {
		t.Fatal("browser could not verify the gateway session signature")
	}
	browserOutbound, browserInbound := browserServiceKeys(t, browserPrivate, accept)
	confirm := browserSeal(t, browserOutbound, accept, 1,
		[]byte(`{"version":1,"type":"session.confirm"}`))
	writeTestJSON(t, browser, confirm)
	var ready wire.Sealed
	readTestJSON(t, browser, &ready)
	if plaintext := browserOpen(t, browserInbound, accept, ready); string(plaintext) != `{"version":1,"type":"session.ready"}` {
		t.Fatalf("end-to-end ready = %q", plaintext)
	}
	read := browserSeal(t, browserOutbound, accept, 2, []byte(
		`{"version":1,"type":"read.request","request_id":"`+
			testRequestID(31)+`","grant":"`+testGrantToken(31)+
			`","scope":"ftw.plan.read"}`,
	))
	writeTestJSON(t, browser, read)
	var sealedResponse wire.Sealed
	readTestJSON(t, browser, &sealedResponse)
	var readResponse readResponseMessage
	if err := wire.DecodeStrict(
		browserOpen(t, browserInbound, accept, sealedResponse),
		wire.MaxPlaintextBytes,
		&readResponse,
	); err != nil {
		t.Fatal(err)
	}
	if readResponse.Error != "" || readResponse.Response == nil ||
		readResponse.Response.Plan == nil || readCalls.Load() != 1 {
		t.Fatalf("end-to-end read = %+v calls=%d", readResponse, readCalls.Load())
	}
	cancel()
	if err := <-result; !errorsIsCanceled(err) {
		t.Fatalf("end-to-end service stop = %v", err)
	}
}

func newBrowserECDH(t *testing.T) (*ecdh.PrivateKey, []byte) {
	t.Helper()
	privateKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded := privateKey.PublicKey().Bytes()
	if len(encoded) != 65 || encoded[0] != 4 {
		t.Fatal("unexpected browser public-key encoding")
	}
	return privateKey, append([]byte(nil), encoded[1:]...)
}

func browserServiceKeys(
	t *testing.T,
	privateKey *ecdh.PrivateKey,
	accept wire.SessionAccept,
) (outbound, inbound cipher.AEAD) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(accept.GatewayEphemeralKey)
	if err != nil {
		t.Fatal(err)
	}
	encoded := append([]byte{4}, raw...)
	gatewayKey, err := ecdh.P256().NewPublicKey(encoded)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := privateKey.ECDH(gatewayKey)
	if err != nil {
		t.Fatal(err)
	}
	transcript, err := wire.SessionAcceptMessage(accept)
	if err != nil {
		t.Fatal(err)
	}
	salt := sha256.Sum256(transcript)
	keyMaterial, err := hkdf.Key(sha256.New, shared, salt[:],
		"ftw-home-link-session-keys/v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	return testGCM(t, keyMaterial[:32]), testGCM(t, keyMaterial[32:])
}

func browserSeal(
	t *testing.T,
	aead cipher.AEAD,
	accept wire.SessionAccept,
	sequence uint64,
	plaintext []byte,
) *wire.Sealed {
	t.Helper()
	ciphertext := aead.Seal(nil, testNonce(sequence), plaintext,
		testAD(accept, "browser-to-gateway", sequence))
	return &wire.Sealed{
		Version: wire.Version, Type: wire.TypeSealed,
		StreamID: accept.StreamID, Sequence: sequence,
		Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext),
	}
}

func browserOpen(
	t *testing.T,
	aead cipher.AEAD,
	accept wire.SessionAccept,
	message wire.Sealed,
) []byte {
	t.Helper()
	ciphertext, err := base64.RawURLEncoding.DecodeString(message.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := aead.Open(nil, testNonce(message.Sequence), ciphertext,
		testAD(accept, "gateway-to-browser", message.Sequence))
	if err != nil {
		t.Fatal(err)
	}
	return plaintext
}

func testGCM(t *testing.T, key []byte) cipher.AEAD {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return aead
}

func testNonce(sequence uint64) []byte {
	value := make([]byte, 12)
	binary.BigEndian.PutUint64(value[4:], sequence)
	return value
}

func testAD(accept wire.SessionAccept, direction string, sequence uint64) []byte {
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%d",
		"ftw-home-link-sealed-ad/v1", accept.RouteHandle, accept.StreamID,
		accept.SessionID, direction, sequence))
}

func waitValue[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(2 * time.Second):
		var zero T
		t.Fatal("timed out waiting for service output")
		return zero
	}
}

func errorsIsCanceled(err error) bool {
	return err == context.Canceled
}
