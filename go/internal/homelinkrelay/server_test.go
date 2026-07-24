package homelinkrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
)

const relayTestGatewayID = "0123dca63201f838f7"

type relayTestIdentity struct {
	privateKey *ecdsa.PrivateKey
	publicKey  []byte
	handle     string
}

func TestRelayAuthenticatesAndForwardsOnlySealedFrames(t *testing.T) {
	identity := newRelayTestIdentity(t)
	server := newRelayTestServer(t, identity)
	uplink, ready := connectUplink(t, server.URL, identity)
	defer uplink.Close()
	if ready.RouteHandle != identity.handle || ready.RouteGeneration != 1 {
		t.Fatalf("ready = %+v", ready)
	}

	browser := connectBrowser(t, server.URL, identity.handle)
	defer browser.Close()
	var opened wire.StreamOpen
	readWireJSON(t, uplink, &opened)
	if opened.Type != wire.TypeStreamOpen || opened.RouteHandle != identity.handle {
		t.Fatalf("stream open = %+v", opened)
	}
	var browserOpened wire.StreamOpen
	readWireJSON(t, browser, &browserOpened)
	if browserOpened != opened {
		t.Fatalf("browser stream open = %+v, want %+v", browserOpened, opened)
	}
	browserIdentity := newRelayTestIdentity(t)
	hello := wire.SessionHello{
		Version: wire.Version, Type: wire.TypeSessionHello,
		ConnectionID: opened.ConnectionID, RouteGeneration: opened.RouteGeneration,
		RouteHandle: identity.handle, StreamID: opened.StreamID,
		BrowserKey:   base64.RawURLEncoding.EncodeToString(browserIdentity.publicKey),
		BrowserNonce: testRawURL(wire.SessionNonceBytes, 7),
	}
	writeWireJSON(t, browser, hello)
	var gatewayHello wire.SessionHello
	readWireJSON(t, uplink, &gatewayHello)
	if gatewayHello != hello {
		t.Fatalf("gateway session hello = %+v", gatewayHello)
	}
	accepted := wire.SessionAccept{
		Version: wire.Version, Type: wire.TypeSessionAccept,
		ConnectionID: opened.ConnectionID, GatewayID: relayTestGatewayID,
		RouteGeneration: opened.RouteGeneration,
		RouteHandle:     identity.handle, StreamID: opened.StreamID,
		SessionID:           testRawURL(wire.SessionIDBytes, 8),
		BrowserKey:          hello.BrowserKey,
		GatewayEphemeralKey: base64.RawURLEncoding.EncodeToString(browserIdentity.publicKey),
		GatewayPublicKey:    base64.RawURLEncoding.EncodeToString(identity.publicKey),
		BrowserNonce:        hello.BrowserNonce,
		GatewayNonce:        testRawURL(wire.SessionNonceBytes, 9),
		ExpiresAtMS:         time.Now().Add(time.Minute).UnixMilli(),
		Signature:           testRawURL(gatewayidentity.SignatureBytes, 10),
	}
	writeWireJSON(t, uplink, accepted)
	var browserAccept wire.SessionAccept
	readWireJSON(t, browser, &browserAccept)
	if browserAccept != accepted {
		t.Fatalf("browser session accept = %+v", browserAccept)
	}

	toGateway := wire.Sealed{
		Version: wire.Version, Type: wire.TypeSealed,
		StreamID: opened.StreamID, Sequence: 1,
		Ciphertext: base64.RawURLEncoding.EncodeToString([]byte("opaque-browser-ciphertext")),
	}
	writeWireJSON(t, browser, toGateway)
	var gatewayFrame wire.Sealed
	readWireJSON(t, uplink, &gatewayFrame)
	if gatewayFrame != toGateway {
		t.Fatalf("gateway frame = %+v, want %+v", gatewayFrame, toGateway)
	}

	toBrowser := wire.Sealed{
		Version: wire.Version, Type: wire.TypeSealed,
		StreamID: opened.StreamID, Sequence: 1,
		Ciphertext: base64.RawURLEncoding.EncodeToString([]byte("opaque-gateway-ciphertext")),
	}
	writeWireJSON(t, uplink, toBrowser)
	var browserFrame wire.Sealed
	readWireJSON(t, browser, &browserFrame)
	if browserFrame != toBrowser {
		t.Fatalf("browser frame = %+v, want %+v", browserFrame, toBrowser)
	}

	writeWireJSON(t, browser, map[string]any{
		"version": 1, "type": "proxy", "stream_id": opened.StreamID,
		"path": "/api/status",
	})
	if _, _, err := browser.ReadMessage(); err == nil {
		t.Fatal("browser proxy frame did not close the stream")
	}
}

func TestRelayRejectsWrongMachineKey(t *testing.T) {
	invited := newRelayTestIdentity(t)
	attacker := newRelayTestIdentity(t)
	server := newRelayTestServer(t, invited)
	connection, _, err := websocket.DefaultDialer.Dial(wsURL(server.URL, "/v1/uplink"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	hello := wire.MachineHello{
		Version: wire.Version, Type: wire.TypeMachineHello,
		GatewayID: relayTestGatewayID, RouteHandle: attacker.handle,
		PublicKey: base64.RawURLEncoding.EncodeToString(attacker.publicKey),
	}
	writeWireJSON(t, connection, hello)
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("uninvited machine key was accepted")
	}
}

func TestRelayBrowserRequiresExactOriginAndActiveRoute(t *testing.T) {
	identity := newRelayTestIdentity(t)
	server := newRelayTestServer(t, identity)
	headers := http.Header{"Origin": []string{"https://attacker.example"}}
	if connection, response, err := websocket.DefaultDialer.Dial(
		wsURL(server.URL, "/v1/browser/"+identity.handle), headers,
	); err == nil {
		connection.Close()
		t.Fatal("wrong browser origin was accepted")
	} else if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong-origin response = %#v, %v", response, err)
	}

	connection := connectBrowser(t, server.URL, identity.handle)
	defer connection.Close()
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("browser connected without an active route")
	}
}

func TestRelayExposesNoAPIRoute(t *testing.T) {
	identity := newRelayTestIdentity(t)
	server := newRelayTestServer(t, identity)
	for _, path := range []string{"/api/status", "/api/config", "/v1/proxy"} {
		response, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, response.StatusCode)
		}
	}
	response, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != `{"status":"ok"}` {
		t.Fatalf("health = %d %q", response.StatusCode, body)
	}
}

func TestRelayReplacesOldRouteOnlyAfterNewProof(t *testing.T) {
	identity := newRelayTestIdentity(t)
	server := newRelayTestServer(t, identity)
	first, firstReady := connectUplink(t, server.URL, identity)
	defer first.Close()
	second, secondReady := connectUplink(t, server.URL, identity)
	defer second.Close()
	if secondReady.RouteGeneration != firstReady.RouteGeneration+1 {
		t.Fatalf("route generations = %d then %d", firstReady.RouteGeneration, secondReady.RouteGeneration)
	}
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := first.ReadMessage(); err == nil {
		t.Fatal("old route remained connected")
	}

	browser := connectBrowser(t, server.URL, identity.handle)
	defer browser.Close()
	var opened wire.StreamOpen
	readWireJSON(t, second, &opened)
	if opened.RouteHandle != identity.handle {
		t.Fatalf("replacement stream = %+v", opened)
	}
}

func TestStaticInvitesRejectDuplicateAndInvalidEntries(t *testing.T) {
	identity := newRelayTestIdentity(t)
	key := base64.RawURLEncoding.EncodeToString(identity.publicKey)
	valid := `[{"gateway_id":"` + relayTestGatewayID + `","public_key":"` + key + `"}]`
	otherID := "0123aabbcc01ddeeff"
	invites, err := ParseStaticInvites([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	got, err := invites.CanonicalPublicKey(context.Background(), relayTestGatewayID)
	if err != nil || string(got) != string(identity.publicKey) {
		t.Fatalf("invite lookup = %x, %v", got, err)
	}
	for _, data := range []string{
		valid + `{}`,
		`[{"gateway_id":"` + relayTestGatewayID + `","gateway_id":"` + relayTestGatewayID +
			`","public_key":"` + key + `"}]`,
		`[{"gateway_id":"` + relayTestGatewayID + `","public_key":"` + key +
			`"},{"gateway_id":"` + relayTestGatewayID + `","public_key":"` + key + `"}]`,
		`[{"gateway_id":"` + relayTestGatewayID + `","public_key":"` + key +
			`"},{"gateway_id":"` + otherID + `","public_key":"` + key + `"}]`,
		`[{"gateway_id":"wrong","public_key":"` + key + `"}]`,
	} {
		if _, err := ParseStaticInvites([]byte(data)); err == nil {
			t.Fatalf("invalid invite file accepted: %s", data)
		}
	}
}

func TestRelayRejectsSessionAcceptForAnotherGateway(t *testing.T) {
	identity := newRelayTestIdentity(t)
	server := newRelayTestServer(t, identity)
	uplink, _ := connectUplink(t, server.URL, identity)
	defer uplink.Close()
	browser := connectBrowser(t, server.URL, identity.handle)
	defer browser.Close()
	var opened wire.StreamOpen
	readWireJSON(t, uplink, &opened)
	var browserOpened wire.StreamOpen
	readWireJSON(t, browser, &browserOpened)
	browserIdentity := newRelayTestIdentity(t)
	hello := wire.SessionHello{
		Version: wire.Version, Type: wire.TypeSessionHello,
		ConnectionID: opened.ConnectionID, RouteGeneration: opened.RouteGeneration,
		RouteHandle: opened.RouteHandle, StreamID: opened.StreamID,
		BrowserKey:   base64.RawURLEncoding.EncodeToString(browserIdentity.publicKey),
		BrowserNonce: testRawURL(wire.SessionNonceBytes, 19),
	}
	writeWireJSON(t, browser, hello)
	var forwarded wire.SessionHello
	readWireJSON(t, uplink, &forwarded)
	accept := wire.SessionAccept{
		Version: wire.Version, Type: wire.TypeSessionAccept,
		ConnectionID: opened.ConnectionID, GatewayID: "0123aabbcc01ddeeff",
		RouteGeneration: opened.RouteGeneration, RouteHandle: opened.RouteHandle,
		StreamID: opened.StreamID, SessionID: testRawURL(wire.SessionIDBytes, 20),
		BrowserKey:          hello.BrowserKey,
		GatewayEphemeralKey: base64.RawURLEncoding.EncodeToString(browserIdentity.publicKey),
		GatewayPublicKey:    base64.RawURLEncoding.EncodeToString(identity.publicKey),
		BrowserNonce:        hello.BrowserNonce, GatewayNonce: testRawURL(wire.SessionNonceBytes, 21),
		ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli(),
		Signature:   testRawURL(gatewayidentity.SignatureBytes, 22),
	}
	writeWireJSON(t, uplink, accept)
	_ = uplink.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := uplink.ReadMessage(); err == nil {
		t.Fatal("cross-site session accept kept the uplink open")
	}
}

func TestRelayHandshakeDeadlineReleasesStreamCapacity(t *testing.T) {
	identity := newRelayTestIdentity(t)
	invites, err := NewStaticInvites([]StaticInvite{{
		GatewayID: relayTestGatewayID,
		PublicKey: base64.RawURLEncoding.EncodeToString(identity.publicKey),
	}})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := New(Options{Invites: invites})
	if err != nil {
		t.Fatal(err)
	}
	relay.confirmLimit = 150 * time.Millisecond
	server := httptest.NewServer(relay.Handler())
	defer server.Close()
	uplink, _ := connectUplink(t, server.URL, identity)
	defer uplink.Close()

	browsers := make([]*websocket.Conn, 0, MaxBrowserStreams)
	for range MaxBrowserStreams {
		browser := connectBrowser(t, server.URL, identity.handle)
		browsers = append(browsers, browser)
		var opened wire.StreamOpen
		readWireJSON(t, uplink, &opened)
		var browserOpened wire.StreamOpen
		readWireJSON(t, browser, &browserOpened)
	}
	time.Sleep(250 * time.Millisecond)
	for _, browser := range browsers {
		_ = browser.SetReadDeadline(time.Now().Add(time.Second))
		if _, _, err := browser.ReadMessage(); err == nil {
			t.Fatal("stalled browser survived the confirmation deadline")
		}
		_ = browser.Close()
	}
	replacement := connectBrowser(t, server.URL, identity.handle)
	defer replacement.Close()
	var opened wire.StreamOpen
	readWireJSON(t, uplink, &opened)
}

func TestRelayConfirmedSessionSurvivesOldConfirmationDeadline(t *testing.T) {
	identity := newRelayTestIdentity(t)
	invites, err := NewStaticInvites([]StaticInvite{{
		GatewayID: relayTestGatewayID,
		PublicKey: base64.RawURLEncoding.EncodeToString(identity.publicKey),
	}})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := New(Options{Invites: invites})
	if err != nil {
		t.Fatal(err)
	}
	relay.confirmLimit = 100 * time.Millisecond
	server := httptest.NewServer(relay.Handler())
	defer server.Close()
	uplink, _ := connectUplink(t, server.URL, identity)
	defer uplink.Close()
	browser := connectBrowser(t, server.URL, identity.handle)
	defer browser.Close()

	var opened wire.StreamOpen
	readWireJSON(t, uplink, &opened)
	var browserOpened wire.StreamOpen
	readWireJSON(t, browser, &browserOpened)
	browserIdentity := newRelayTestIdentity(t)
	hello := wire.SessionHello{
		Version: wire.Version, Type: wire.TypeSessionHello,
		ConnectionID: opened.ConnectionID, RouteGeneration: opened.RouteGeneration,
		RouteHandle: opened.RouteHandle, StreamID: opened.StreamID,
		BrowserKey:   base64.RawURLEncoding.EncodeToString(browserIdentity.publicKey),
		BrowserNonce: testRawURL(wire.SessionNonceBytes, 23),
	}
	writeWireJSON(t, browser, hello)
	var forwarded wire.SessionHello
	readWireJSON(t, uplink, &forwarded)
	accept := wire.SessionAccept{
		Version: wire.Version, Type: wire.TypeSessionAccept,
		ConnectionID: opened.ConnectionID, GatewayID: relayTestGatewayID,
		RouteGeneration: opened.RouteGeneration, RouteHandle: opened.RouteHandle,
		StreamID: opened.StreamID, SessionID: testRawURL(wire.SessionIDBytes, 24),
		BrowserKey:          hello.BrowserKey,
		GatewayEphemeralKey: base64.RawURLEncoding.EncodeToString(browserIdentity.publicKey),
		GatewayPublicKey:    base64.RawURLEncoding.EncodeToString(identity.publicKey),
		BrowserNonce:        hello.BrowserNonce, GatewayNonce: testRawURL(wire.SessionNonceBytes, 25),
		ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli(),
		Signature:   testRawURL(gatewayidentity.SignatureBytes, 26),
	}
	writeWireJSON(t, uplink, accept)
	var browserAccept wire.SessionAccept
	readWireJSON(t, browser, &browserAccept)
	browserConfirm := wire.Sealed{
		Version: wire.Version, Type: wire.TypeSealed,
		StreamID: opened.StreamID, Sequence: 1,
		Ciphertext: base64.RawURLEncoding.EncodeToString([]byte("confirm")),
	}
	writeWireJSON(t, browser, browserConfirm)
	var gatewayConfirm wire.Sealed
	readWireJSON(t, uplink, &gatewayConfirm)
	gatewayReady := wire.Sealed{
		Version: wire.Version, Type: wire.TypeSealed,
		StreamID: opened.StreamID, Sequence: 1,
		Ciphertext: base64.RawURLEncoding.EncodeToString([]byte("ready")),
	}
	writeWireJSON(t, uplink, gatewayReady)
	var browserReady wire.Sealed
	readWireJSON(t, browser, &browserReady)

	time.Sleep(150 * time.Millisecond)
	next := browserConfirm
	next.Sequence = 2
	next.Ciphertext = base64.RawURLEncoding.EncodeToString([]byte("read"))
	writeWireJSON(t, browser, next)
	var forwardedNext wire.Sealed
	readWireJSON(t, uplink, &forwardedNext)
	if forwardedNext != next {
		t.Fatalf("post-confirm frame = %+v, want %+v", forwardedNext, next)
	}
}

func newRelayTestServer(t *testing.T, identity relayTestIdentity) *httptest.Server {
	t.Helper()
	invites, err := NewStaticInvites([]StaticInvite{{
		GatewayID: relayTestGatewayID,
		PublicKey: base64.RawURLEncoding.EncodeToString(identity.publicKey),
	}})
	if err != nil {
		t.Fatal(err)
	}
	relay, err := New(Options{Invites: invites})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	return server
}

func connectUplink(t *testing.T, serverURL string, identity relayTestIdentity) (*websocket.Conn, wire.MachineReady) {
	t.Helper()
	connection, _, err := websocket.DefaultDialer.Dial(wsURL(serverURL, "/v1/uplink"), nil)
	if err != nil {
		t.Fatal(err)
	}
	hello := wire.MachineHello{
		Version: wire.Version, Type: wire.TypeMachineHello,
		GatewayID: relayTestGatewayID, RouteHandle: identity.handle,
		PublicKey: base64.RawURLEncoding.EncodeToString(identity.publicKey),
	}
	writeWireJSON(t, connection, hello)
	var challenge wire.MachineChallenge
	readWireJSON(t, connection, &challenge)
	proof := wire.MachineProof{
		Version: wire.Version, Type: wire.TypeMachineProof,
		ConnectionID: challenge.ConnectionID,
		GatewayID:    relayTestGatewayID, RouteHandle: identity.handle,
		PublicKey: hello.PublicKey, Nonce: challenge.Nonce,
		ExpiresAtMS: challenge.ExpiresAtMS,
	}
	transcript, err := wire.MachineProofMessage(proof)
	if err != nil {
		t.Fatal(err)
	}
	proof.Signature = base64.RawURLEncoding.EncodeToString(identity.sign(t, transcript))
	writeWireJSON(t, connection, proof)
	var ready wire.MachineReady
	readWireJSON(t, connection, &ready)
	return connection, ready
}

func connectBrowser(t *testing.T, serverURL, handle string) *websocket.Conn {
	t.Helper()
	headers := http.Header{"Origin": []string{DefaultBrowserOrigin}}
	connection, _, err := websocket.DefaultDialer.Dial(
		wsURL(serverURL, "/v1/browser/"+handle), headers,
	)
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func writeWireJSON(t *testing.T, connection *websocket.Conn, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
}

func readWireJSON(t *testing.T, connection *websocket.Conn, value any) {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	messageType, data, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("message type = %d", messageType)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatalf("decode %q: %v", data, err)
	}
}

func newRelayTestIdentity(t *testing.T) relayTestIdentity {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := make([]byte, gatewayidentity.PublicKeyBytes)
	privateKey.X.FillBytes(publicKey[:32])
	privateKey.Y.FillBytes(publicKey[32:])
	handle, err := gatewayidentity.RouteHandle(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return relayTestIdentity{privateKey: privateKey, publicKey: publicKey, handle: handle}
}

func (i relayTestIdentity) sign(t *testing.T, message []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(message)
	r, s, err := ecdsa.Sign(rand.Reader, i.privateKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := make([]byte, gatewayidentity.SignatureBytes)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signature
}

func wsURL(serverURL, path string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + path
}

func testRawURL(length int, fill byte) string {
	raw := make([]byte, length)
	for i := range raw {
		raw[i] = fill
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
