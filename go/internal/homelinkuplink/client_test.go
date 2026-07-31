package homelinkuplink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
	"github.com/srcfl/ftw/go/internal/homelinkrelay"
)

const uplinkTestGatewayID = "0123dca63201f838f7"

type uplinkTestIdentity struct {
	privateKey *ecdsa.PrivateKey
	publicKey  []byte
}

func (i uplinkTestIdentity) GatewayID() string { return uplinkTestGatewayID }
func (i uplinkTestIdentity) PublicKey() []byte { return append([]byte(nil), i.publicKey...) }
func (i uplinkTestIdentity) Sign(message []byte) ([]byte, error) {
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

func TestClientAuthenticatesAndCarriesSealedFrames(t *testing.T) {
	identity := newUplinkTestIdentity(t)
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
	defer connection.Close()
	handle, _ := gatewayidentity.RouteHandle(identity.publicKey)
	if connection.RouteHandle() != handle || connection.ConnectionID() == "" ||
		connection.RouteGeneration() != 1 {
		t.Fatalf("authenticated connection = %q %q %d",
			connection.RouteHandle(), connection.ConnectionID(), connection.RouteGeneration())
	}

	headers := http.Header{"Origin": []string{homelinkrelay.DefaultBrowserOrigin}}
	browser, _, err := websocket.DefaultDialer.Dial(
		wsURL(server.URL, "/v1/browser/"+handle), headers,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()
	frame, err := connection.ReadFrame()
	if err != nil || frame.Type != wire.TypeStreamOpen || frame.Open == nil {
		t.Fatalf("stream open = %+v, %v", frame, err)
	}
	streamID := frame.Open.StreamID
	var browserOpened wire.StreamOpen
	readTestJSON(t, browser, &browserOpened)
	if browserOpened.StreamID != streamID || browserOpened.RouteHandle != handle {
		t.Fatalf("browser stream open = %+v", browserOpened)
	}
	browserIdentity := newUplinkTestIdentity(t)
	sessionHello := wire.SessionHello{
		Version: wire.Version, Type: wire.TypeSessionHello,
		ConnectionID: browserOpened.ConnectionID, RouteGeneration: browserOpened.RouteGeneration,
		RouteHandle: handle, StreamID: streamID,
		BrowserKey:   base64.RawURLEncoding.EncodeToString(browserIdentity.publicKey),
		BrowserNonce: rawURL(wire.SessionNonceBytes, 3),
	}
	writeTestJSON(t, browser, sessionHello)
	frame, err = connection.ReadFrame()
	if err != nil || frame.SessionHello == nil || *frame.SessionHello != sessionHello {
		t.Fatalf("session hello = %+v, %v", frame, err)
	}
	accept := wire.SessionAccept{
		Version: wire.Version, Type: wire.TypeSessionAccept,
		ConnectionID: sessionHello.ConnectionID, GatewayID: uplinkTestGatewayID,
		RouteGeneration: sessionHello.RouteGeneration,
		RouteHandle:     handle, StreamID: sessionHello.StreamID,
		SessionID:           rawURL(wire.SessionIDBytes, 4),
		BrowserKey:          sessionHello.BrowserKey,
		GatewayEphemeralKey: base64.RawURLEncoding.EncodeToString(browserIdentity.publicKey),
		GatewayPublicKey:    base64.RawURLEncoding.EncodeToString(identity.publicKey),
		BrowserNonce:        sessionHello.BrowserNonce,
		GatewayNonce:        rawURL(wire.SessionNonceBytes, 5),
		ExpiresAtMS:         time.Now().Add(time.Minute).UnixMilli(),
		Signature:           rawURL(gatewayidentity.SignatureBytes, 6),
	}
	if err := connection.SendSessionAccept(accept); err != nil {
		t.Fatal(err)
	}
	var browserAccept wire.SessionAccept
	readTestJSON(t, browser, &browserAccept)
	if browserAccept != accept {
		t.Fatalf("browser accept = %+v", browserAccept)
	}

	browserMessage := wire.Sealed{
		Version: wire.Version, Type: wire.TypeSealed,
		StreamID: streamID, Sequence: 1,
		Ciphertext: base64.RawURLEncoding.EncodeToString([]byte("browser ciphertext")),
	}
	writeTestJSON(t, browser, browserMessage)
	frame, err = connection.ReadFrame()
	if err != nil || frame.Sealed == nil || *frame.Sealed != browserMessage {
		t.Fatalf("browser frame = %+v, %v", frame, err)
	}

	gatewayMessage := wire.Sealed{
		Version: wire.Version, Type: wire.TypeSealed,
		StreamID: frame.Sealed.StreamID, Sequence: 1,
		Ciphertext: base64.RawURLEncoding.EncodeToString([]byte("gateway ciphertext")),
	}
	if err := connection.SendSealed(gatewayMessage); err != nil {
		t.Fatal(err)
	}
	var received wire.Sealed
	readTestJSON(t, browser, &received)
	if received != gatewayMessage {
		t.Fatalf("gateway frame = %+v", received)
	}
}

func TestClientRejectsChallengeTooFarAhead(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	now := time.Unix(1_800_000_000, 0)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		if _, _, err := connection.ReadMessage(); err != nil {
			return
		}
		writeTestJSON(t, connection, wire.MachineChallenge{
			Version: wire.Version, Type: wire.TypeMachineChallenge,
			ConnectionID: rawURL(wire.ConnectionIDBytes, 1),
			Nonce:        rawURL(wire.MachineNonceBytes, 2),
			ExpiresAtMS:  now.Add(maxChallengeAhead + time.Millisecond).UnixMilli(),
		})
	}))
	defer server.Close()
	client, err := newClient(identity, wsURL(server.URL, "/v1/uplink"),
		websocket.DefaultDialer, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Dial(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "expiry") {
		t.Fatalf("far-future challenge = %v", err)
	}
}

func TestClientRejectsEndpointDrift(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	for _, endpoint := range []string{
		"wss://uplink.home.sourceful.energy/v2/uplink",
		"wss://uplink.home.sourceful.energy/v1/uplink?redirect=1",
		"not a URL",
	} {
		if _, err := newClient(identity, endpoint, websocket.DefaultDialer, time.Now); err == nil {
			t.Fatalf("endpoint %q was accepted", endpoint)
		}
	}
	client, err := New(identity)
	if err != nil {
		t.Fatal(err)
	}
	if client.endpoint != Endpoint {
		t.Fatalf("production endpoint = %q", client.endpoint)
	}
}

func TestConnectionRejectsInvalidOutboundFrame(t *testing.T) {
	connection := &Connection{}
	if err := connection.SendSealed(wire.Sealed{}); err == nil {
		t.Fatal("invalid sealed frame was accepted")
	}
	if err := connection.CloseStream("bad", "closed"); err == nil {
		t.Fatal("invalid stream id was accepted")
	}
}

func TestRunBacksOffRapidDisconnectsAndResetsAfterStableConnection(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	now := time.Unix(1_800_000_000, 0)
	client, err := newClient(identity, Endpoint, websocket.DefaultDialer,
		func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := 0
	var attempts []int
	err = client.run(ctx,
		func(context.Context) (*Connection, error) { return &Connection{}, nil },
		func(context.Context, *Connection) error {
			served++
			if served == 3 {
				now = now.Add(stableConnection)
			} else {
				now = now.Add(time.Second)
			}
			return errors.New("connection closed")
		},
		func(attempt int) (time.Duration, error) {
			attempts = append(attempts, attempt)
			if len(attempts) == 4 {
				cancel()
			}
			return time.Millisecond, nil
		},
	)
	if err != context.Canceled {
		t.Fatalf("run stop = %v", err)
	}
	want := []int{0, 1, 0, 1}
	if !slices.Equal(attempts, want) {
		t.Fatalf("retry attempts = %v, want %v", attempts, want)
	}
}

func TestRunWithStatusReportsReadyAndDisconnected(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	client, err := newClient(identity, Endpoint, websocket.DefaultDialer, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var states []bool
	err = client.runWithStatus(
		ctx,
		func(context.Context) (*Connection, error) { return &Connection{}, nil },
		func(context.Context, *Connection) error { return errors.New("closed") },
		func(int) (time.Duration, error) {
			cancel()
			return 0, nil
		},
		func(connected bool, _ error) { states = append(states, connected) },
	)
	if err != context.Canceled {
		t.Fatalf("run stop = %v", err)
	}
	if !slices.Equal(states, []bool{true, false}) {
		t.Fatalf("status states = %v", states)
	}
}

func TestRetryDelayBoundsAndCancellation(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	client, err := newClient(identity, Endpoint, websocket.DefaultDialer, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	client.random = bytes.NewReader(make([]byte, 128))
	for attempt := 0; attempt <= 10; attempt++ {
		delay, err := client.retryDelay(attempt)
		if err != nil {
			t.Fatal(err)
		}
		maximum := time.Second << min(attempt, 6)
		if maximum > 60*time.Second {
			maximum = 60 * time.Second
		}
		if delay < time.Second || delay > maximum {
			t.Fatalf("attempt %d delay = %v, want 1s..%v", attempt, delay, maximum)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitContext(ctx, time.Hour); err != context.Canceled {
		t.Fatalf("canceled wait = %v", err)
	}
}

func newUplinkTestIdentity(t *testing.T) uplinkTestIdentity {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := make([]byte, gatewayidentity.PublicKeyBytes)
	privateKey.X.FillBytes(publicKey[:32])
	privateKey.Y.FillBytes(publicKey[32:])
	return uplinkTestIdentity{privateKey: privateKey, publicKey: publicKey}
}

func writeTestJSON(t *testing.T, connection *websocket.Conn, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
}

func readTestJSON(t *testing.T, connection *websocket.Conn, value any) {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}

func wsURL(serverURL, path string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + path
}

func rawURL(length int, fill byte) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{fill}), length)))
}
