package homelinkuplink

import (
	"context"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/homelink"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
	"github.com/srcfl/ftw/go/internal/homelinksession"
	"github.com/srcfl/ftw/go/internal/state"
)

type readExecutorFunc func(
	context.Context,
	string,
	string,
	homelink.ReadRequest,
	homelink.ReadBinding,
) (homelink.ReadResponse, error)

func (f readExecutorFunc) VerifyAndDispatchBoundRead(
	ctx context.Context,
	token string,
	requestID string,
	request homelink.ReadRequest,
	binding homelink.ReadBinding,
) (homelink.ReadResponse, error) {
	return f(ctx, token, requestID, request, binding)
}

func TestEncryptedReadReturnsTypedResponse(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	var calls atomic.Int32
	service, err := NewServiceWithReads(identity, readExecutorFunc(func(
		_ context.Context,
		token string,
		requestID string,
		request homelink.ReadRequest,
		binding homelink.ReadBinding,
	) (homelink.ReadResponse, error) {
		calls.Add(1)
		if token != testGrantToken(1) || requestID != testRequestID(1) {
			t.Fatalf("read credentials changed: %q %q", token, requestID)
		}
		hash, hashErr := homelink.ReadRequestHash(requestID, request)
		if hashErr != nil || hash != binding.RequestHash {
			t.Fatalf("read hash binding = %x, %v", binding.RequestHash, hashErr)
		}
		if request.GatewayID != uplinkTestGatewayID ||
			request.Scope != homelink.ScopeHealthRead ||
			binding.GatewayID != uplinkTestGatewayID ||
			binding.RouteGeneration != 1 {
			t.Fatalf("read binding = request=%+v binding=%+v", request, binding)
		}
		return homelink.ReadResponse{
			Version: homelink.ReadContractVersion,
			Scope:   homelink.ScopeHealthRead,
			Health: &homelink.HealthReadResponse{
				Status: "ok", CheckedAtMS: 1,
			},
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	transport, accept, browserOutbound, browserInbound, cancel, result :=
		startConfirmedReadService(t, service, identity)
	defer stopReadService(t, cancel, result)

	request := `{"version":1,"type":"read.request","request_id":"` +
		testRequestID(1) + `","grant":"` + testGrantToken(1) +
		`","scope":"ftw.health.read"}`
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 2, []byte(request),
	)}
	sealed := waitValue(t, transport.sealed)
	var response readResponseMessage
	if err := wire.DecodeStrict(
		browserOpen(t, browserInbound, accept, sealed),
		wire.MaxPlaintextBytes,
		&response,
	); err != nil {
		t.Fatal(err)
	}
	if response.Error != "" || response.Response == nil ||
		response.Response.Health == nil ||
		response.Response.Health.Status != "ok" {
		t.Fatalf("typed read response = %+v", response)
	}
	if calls.Load() != 1 {
		t.Fatalf("read calls = %d", calls.Load())
	}
}

func TestReadTimeoutIsEncryptedAndNotRetried(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	var calls atomic.Int32
	service, _ := NewServiceWithReads(identity, readExecutorFunc(func(
		ctx context.Context,
		_ string,
		_ string,
		_ homelink.ReadRequest,
		_ homelink.ReadBinding,
	) (homelink.ReadResponse, error) {
		calls.Add(1)
		<-ctx.Done()
		return homelink.ReadResponse{}, ctx.Err()
	}))
	service.readTimeout = 10 * time.Millisecond
	transport, accept, browserOutbound, browserInbound, cancel, result :=
		startConfirmedReadService(t, service, identity)
	defer stopReadService(t, cancel, result)

	request := `{"version":1,"type":"read.request","request_id":"` +
		testRequestID(2) + `","grant":"` + testGrantToken(2) +
		`","scope":"ftw.plan.read"}`
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 2, []byte(request),
	)}
	var response readResponseMessage
	if err := wire.DecodeStrict(
		browserOpen(t, browserInbound, accept, waitValue(t, transport.sealed)),
		wire.MaxPlaintextBytes,
		&response,
	); err != nil {
		t.Fatal(err)
	}
	if response.Error != "timeout" || response.Response != nil || calls.Load() != 1 {
		t.Fatalf("timeout response = %+v calls=%d", response, calls.Load())
	}
}

func TestReadStreamCloseCancelsLocalRead(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	started := make(chan struct{})
	canceled := make(chan struct{})
	service, _ := NewServiceWithReads(identity, readExecutorFunc(func(
		ctx context.Context,
		_ string,
		_ string,
		_ homelink.ReadRequest,
		_ homelink.ReadBinding,
	) (homelink.ReadResponse, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return homelink.ReadResponse{}, ctx.Err()
	}))
	transport, accept, browserOutbound, _, cancel, result :=
		startConfirmedReadService(t, service, identity)
	defer stopReadService(t, cancel, result)
	request := `{"version":1,"type":"read.request","request_id":"` +
		testRequestID(3) + `","grant":"` + testGrantToken(3) +
		`","scope":"ftw.plan.read"}`
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 2, []byte(request),
	)}
	waitSignal(t, started)
	transport.frames <- Frame{Type: wire.TypeStreamClose, Close: &wire.StreamClose{
		Version: wire.Version, Type: wire.TypeStreamClose,
		StreamID: accept.StreamID, Code: "browser-canceled",
	}}
	waitSignal(t, canceled)
	select {
	case message := <-transport.sealed:
		t.Fatalf("canceled read sent response: %+v", message)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestReadBackpressureClosesBusyStream(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	started := make(chan struct{})
	service, _ := NewServiceWithReads(identity, readExecutorFunc(func(
		ctx context.Context,
		_ string,
		_ string,
		_ homelink.ReadRequest,
		_ homelink.ReadBinding,
	) (homelink.ReadResponse, error) {
		close(started)
		<-ctx.Done()
		return homelink.ReadResponse{}, ctx.Err()
	}))
	transport, accept, browserOutbound, _, cancel, result :=
		startConfirmedReadService(t, service, identity)
	defer stopReadService(t, cancel, result)
	first := `{"version":1,"type":"read.request","request_id":"` +
		testRequestID(4) + `","grant":"` + testGrantToken(4) +
		`","scope":"ftw.plan.read"}`
	second := `{"version":1,"type":"read.request","request_id":"` +
		testRequestID(5) + `","grant":"` + testGrantToken(5) +
		`","scope":"ftw.plan.read"}`
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 2, []byte(first),
	)}
	waitSignal(t, started)
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 3, []byte(second),
	)}
	closed := waitValue(t, transport.closes)
	if closed.StreamID != accept.StreamID || closed.Code != "invalid-read" {
		t.Fatalf("busy stream close = %+v", closed)
	}
}

func TestReadEnvelopeIsClosedAndBounded(t *testing.T) {
	session := testSessionContext()
	valid := `{"version":1,"type":"read.request","request_id":"` +
		testRequestID(7) + `","grant":"` + testGrantToken(7) +
		`","scope":"ftw.energy.history.read",` +
		`"history":{"asset_id":"","since_ms":1,"until_ms":300001,"bucket_ms":300000,"limit":1}}`
	if _, request, binding, err := decodeReadRequest([]byte(valid), session); err != nil {
		t.Fatal(err)
	} else if request.History == nil || binding.RequestHash == ([32]byte{}) {
		t.Fatalf("decoded read = %+v %+v", request, binding)
	}
	for _, invalid := range []string{
		strings.Replace(valid, `"limit":1`, `"limit":513`, 1),
		strings.Replace(valid, `"scope":"ftw.energy.history.read"`, `"scope":"ftw.control.write"`, 1),
		strings.Replace(valid, `"grant":"`+testGrantToken(7)+`"`,
			`"grant":"`+testGrantToken(7)+`","grant":"`+testGrantToken(8)+`"`, 1),
		strings.Replace(valid, `"request_id":"`+testRequestID(7)+`"`, `"request_id":"not-canonical=="`, 1),
		strings.Replace(valid, `"scope":"ftw.energy.history.read"`,
			`"scope":"ftw.energy.history.read","path":"/api/config"`, 1),
		valid + `{}`,
		strings.Repeat(" ", maxReadRequestBytes+1),
	} {
		if _, _, _, err := decodeReadRequest([]byte(invalid), session); err == nil {
			t.Fatalf("accepted invalid read envelope: %s", invalid)
		}
	}
}

func TestReadErrorsAreFixedAndRedacted(t *testing.T) {
	secret := errors.New("database path and operator secret")
	payload, err := encodeReadResponse(
		testRequestID(13), homelink.ReadResponse{}, secret,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), secret.Error()) {
		t.Fatal("read response exposed an internal error")
	}
	var response readResponseMessage
	if err := wire.DecodeStrict(payload, wire.MaxPlaintextBytes, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "read-failed" || response.Response != nil {
		t.Fatalf("fixed error response = %+v", response)
	}
}

func TestUnsafeCoreResponseTextBecomesStrictFixedError(t *testing.T) {
	for _, unsafe := range []string{"\u202e", "\u2066", "\u2069", "\ufeff", "\u2028", "\u2029"} {
		for name, response := range map[string]homelink.ReadResponse{
			"plan": {
				Version: homelink.ReadContractVersion, Scope: homelink.ScopePlanRead,
				Plan: &homelink.PlanReadResponse{
					Available: true, GeneratedAtMS: 1, Mode: "cost" + unsafe,
					HorizonSlots: 1,
				},
			},
			"asset": {
				Version: homelink.ReadContractVersion, Scope: homelink.ScopeEnergyAssetsRead,
				EnergyAssets: &homelink.EnergyAssetsReadResponse{Assets: []state.EnergyAsset{{
					AssetID: "site/grid" + unsafe, Kind: state.AssetGridMeter,
					FirstSeenMS: 1, LastSeenMS: 1,
				}}},
			},
			"history": {
				Version: homelink.ReadContractVersion, Scope: homelink.ScopeEnergyHistoryRead,
				EnergyHistory: &homelink.EnergyHistoryReadResponse{
					Points: []state.EnergyLedgerPoint{{
						SchemaVersion: 1, AssetID: "site/grid",
						Flow: state.FlowGridImport, BucketStartMS: 1,
						BucketLenMS: state.EnergyLedgerBucketMS,
						Source:      "counter", Quality: "measured",
						Provenance: "source" + unsafe,
					}},
				},
			},
		} {
			t.Run(name+"/"+base64.RawURLEncoding.EncodeToString([]byte(unsafe)), func(t *testing.T) {
				payload, err := encodeReadResponse(testRequestID(14), response, nil)
				if err != nil {
					t.Fatal(err)
				}
				var decoded readResponseMessage
				if err := wire.DecodeStrict(
					payload, wire.MaxPlaintextBytes, &decoded,
				); err != nil {
					t.Fatalf("strict receiver rejected fixed response: %v", err)
				}
				if decoded.Error != "invalid-core-response" ||
					decoded.Response != nil {
					t.Fatalf("unsafe Core response = %+v", decoded)
				}
			})
		}
	}
}

func FuzzDecodeReadRequestDoesNotPanic(f *testing.F) {
	f.Add([]byte(`{"version":1,"type":"read.request"}`))
	f.Add([]byte{0xff, 0x00, '{'})
	session := testSessionContext()
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _, _ = decodeReadRequest(data, session)
	})
}

func TestOversizedTypedResponseBecomesFixedError(t *testing.T) {
	points := make([]state.EnergyLedgerPoint, homelink.MaxRemotePoints)
	for i := range points {
		points[i] = state.EnergyLedgerPoint{
			SchemaVersion: 1, AssetID: "site/grid_meter",
			Flow: state.FlowGridImport, BucketStartMS: int64(i + 1),
			BucketLenMS: state.EnergyLedgerBucketMS, Source: "counter",
			Quality: "measured", Provenance: strings.Repeat("p", homelink.MaxReadStringBytes),
		}
	}
	payload, err := encodeReadResponse(testRequestID(8), homelink.ReadResponse{
		Version:       homelink.ReadContractVersion,
		Scope:         homelink.ScopeEnergyHistoryRead,
		EnergyHistory: &homelink.EnergyHistoryReadResponse{Points: points},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var response readResponseMessage
	if err := wire.DecodeStrict(payload, wire.MaxPlaintextBytes, &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != "response-too-large" || response.Response != nil {
		t.Fatalf("oversized response = %+v", response)
	}
}

func startConfirmedReadService(
	t *testing.T,
	service *Service,
	identity uplinkTestIdentity,
) (
	*fakeTransport,
	wire.SessionAccept,
	cipher.AEAD,
	cipher.AEAD,
	context.CancelFunc,
	<-chan error,
) {
	t.Helper()
	transport := newFakeTransport()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- service.serve(ctx, transport) }()
	handle, _ := gatewayidentity.RouteHandle(identity.publicKey)
	streamID := testRequestID(9)
	browserPrivate, browserKey := newBrowserECDH(t)
	hello := wire.SessionHello{
		Version: wire.Version, Type: wire.TypeSessionHello,
		ConnectionID: testRequestID(10), RouteGeneration: 1,
		RouteHandle: handle, StreamID: streamID,
		BrowserKey:   base64.RawURLEncoding.EncodeToString(browserKey),
		BrowserNonce: base64.RawURLEncoding.EncodeToString(make([]byte, wire.SessionNonceBytes)),
	}
	transport.frames <- Frame{Type: wire.TypeStreamOpen, Open: &wire.StreamOpen{
		Version: wire.Version, Type: wire.TypeStreamOpen,
		ConnectionID: hello.ConnectionID, RouteGeneration: hello.RouteGeneration,
		RouteHandle: handle, StreamID: streamID,
	}}
	transport.frames <- Frame{Type: wire.TypeSessionHello, SessionHello: &hello}
	accept := waitValue(t, transport.accepts)
	outbound, inbound := browserServiceKeys(t, browserPrivate, accept)
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, outbound, accept, 1,
		[]byte(`{"version":1,"type":"session.confirm"}`),
	)}
	_ = waitValue(t, transport.sealed)
	return transport, accept, outbound, inbound, cancel, result
}

func stopReadService(t *testing.T, cancel context.CancelFunc, result <-chan error) {
	t.Helper()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("read service stop = %v", err)
	}
}

func waitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for signal")
	}
}

func testRequestID(fill byte) string {
	value := make([]byte, readRequestIDBytes)
	for i := range value {
		value[i] = fill
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func testGrantToken(fill byte) string {
	value := make([]byte, readGrantTokenBytes)
	for i := range value {
		value[i] = fill
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func testSessionContext() homelinksession.Context {
	return homelinksession.Context{
		GatewayID:       uplinkTestGatewayID,
		RouteGeneration: 1,
		RouteHandle:     base64.RawURLEncoding.EncodeToString(make([]byte, gatewayidentity.RouteHandleBytes)),
		StreamID:        testRequestID(11),
		SessionID:       testRequestID(12),
		ExpiresAt:       time.Now().Add(time.Minute),
	}
}
