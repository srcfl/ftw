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

type authorizedReadExecutor struct {
	begin func(context.Context) (homelink.LocalAssertionChallenge, error)
	issue func(
		context.Context,
		string,
		homelink.PasskeyAssertion,
		homelink.Scope,
		time.Duration,
		homelink.ReadBinding,
	) (homelink.Grant, error)
	dispatch readExecutorFunc
}

type remoteAccessExecutor struct {
	authorizedReadExecutor
	beginRegistration func(
		context.Context,
		string,
		string,
		string,
	) (homelink.RegistrationChallenge, error)
	finishRegistration func(
		context.Context,
		string,
		[]byte,
	) (homelink.CredentialSummary, error)
}

func (f remoteAccessExecutor) BeginRegistration(
	ctx context.Context,
	pairingID string,
	pairingSecret string,
	label string,
) (homelink.RegistrationChallenge, error) {
	return f.beginRegistration(ctx, pairingID, pairingSecret, label)
}

func (f remoteAccessExecutor) FinishRegistration(
	ctx context.Context,
	expectationID string,
	responseJSON []byte,
) (homelink.CredentialSummary, error) {
	return f.finishRegistration(ctx, expectationID, responseJSON)
}

func (f authorizedReadExecutor) BeginLocalAssertion(
	ctx context.Context,
) (homelink.LocalAssertionChallenge, error) {
	return f.begin(ctx)
}

func (f authorizedReadExecutor) IssueOneUseBoundAccess(
	ctx context.Context,
	challengeID string,
	assertion homelink.PasskeyAssertion,
	scope homelink.Scope,
	ttl time.Duration,
	binding homelink.ReadBinding,
) (homelink.Grant, error) {
	return f.issue(ctx, challengeID, assertion, scope, ttl, binding)
}

func (f authorizedReadExecutor) VerifyAndDispatchBoundRead(
	ctx context.Context,
	token string,
	requestID string,
	request homelink.ReadRequest,
	binding homelink.ReadBinding,
) (homelink.ReadResponse, error) {
	return f.dispatch(ctx, token, requestID, request, binding)
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

func TestEncryptedRemotePasskeyAuthorizesOneBoundRead(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	var beginCalls atomic.Int32
	var issueCalls atomic.Int32
	var dispatchCalls atomic.Int32
	executor := authorizedReadExecutor{
		begin: func(context.Context) (homelink.LocalAssertionChallenge, error) {
			beginCalls.Add(1)
			return homelink.LocalAssertionChallenge{
				ID: "challenge-1", Challenge: []byte("browser-challenge"),
				RPID:                     "home.sourceful.energy",
				AllowCredentials:         [][]byte{{1, 2, 3}},
				UserVerificationRequired: true,
			}, nil
		},
		issue: func(
			_ context.Context,
			challengeID string,
			assertion homelink.PasskeyAssertion,
			scope homelink.Scope,
			ttl time.Duration,
			binding homelink.ReadBinding,
		) (homelink.Grant, error) {
			issueCalls.Add(1)
			if challengeID != "challenge-1" ||
				string(assertion.ResponseJSON) != `{"id":"credential","response":{}}` ||
				scope != homelink.ScopeHealthRead ||
				ttl != defaultReadTimeout ||
				binding.RequestHash == ([32]byte{}) {
				t.Fatalf("authorized issue changed: id=%q assertion=%s scope=%q ttl=%v binding=%+v",
					challengeID, assertion.ResponseJSON, scope, ttl, binding)
			}
			return homelink.Grant{Token: testGrantToken(21)}, nil
		},
		dispatch: func(
			_ context.Context,
			token string,
			requestID string,
			request homelink.ReadRequest,
			binding homelink.ReadBinding,
		) (homelink.ReadResponse, error) {
			dispatchCalls.Add(1)
			if token != testGrantToken(21) || requestID != testRequestID(21) ||
				request.Scope != homelink.ScopeHealthRead ||
				binding.RequestHash == ([32]byte{}) {
				t.Fatalf("authorized dispatch changed: token=%q id=%q request=%+v binding=%+v",
					token, requestID, request, binding)
			}
			return homelink.ReadResponse{
				Version: homelink.ReadContractVersion,
				Scope:   homelink.ScopeHealthRead,
				Health:  &homelink.HealthReadResponse{Status: "ok", CheckedAtMS: 1},
			}, nil
		},
	}
	service, err := NewServiceWithAuthorizedReads(identity, executor)
	if err != nil {
		t.Fatal(err)
	}
	transport, accept, browserOutbound, browserInbound, cancel, result :=
		startConfirmedReadService(t, service, identity)
	defer stopReadService(t, cancel, result)

	begin := `{"version":1,"type":"assertion.begin","request_id":"` +
		testRequestID(20) + `"}`
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 2, []byte(begin),
	)}
	var challenge assertionChallengeMessage
	if err := wire.DecodeStrict(
		browserOpen(t, browserInbound, accept, waitValue(t, transport.sealed)),
		wire.MaxPlaintextBytes,
		&challenge,
	); err != nil {
		t.Fatal(err)
	}
	if challenge.Type != assertionResultType ||
		challenge.RequestID != testRequestID(20) ||
		challenge.ChallengeID != "challenge-1" ||
		challenge.Challenge != base64.RawURLEncoding.EncodeToString([]byte("browser-challenge")) ||
		challenge.RPID != "home.sourceful.energy" ||
		len(challenge.AllowCredentials) != 1 ||
		challenge.UserVerification != "required" ||
		challenge.Error != "" {
		t.Fatalf("assertion challenge = %+v", challenge)
	}

	authorized := `{"version":1,"type":"read.authorize","request_id":"` +
		testRequestID(21) +
		`","challenge_id":"challenge-1","assertion":{"id":"credential","response":{}}` +
		`,"scope":"ftw.health.read"}`
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 3, []byte(authorized),
	)}
	var response readResponseMessage
	if err := wire.DecodeStrict(
		browserOpen(t, browserInbound, accept, waitValue(t, transport.sealed)),
		wire.MaxPlaintextBytes,
		&response,
	); err != nil {
		t.Fatal(err)
	}
	if response.Error != "" || response.Response == nil ||
		response.Response.Health == nil ||
		response.Response.Health.Status != "ok" ||
		beginCalls.Load() != 1 || issueCalls.Load() != 1 ||
		dispatchCalls.Load() != 1 {
		t.Fatalf("authorized read = %+v calls=%d/%d/%d",
			response, beginCalls.Load(), issueCalls.Load(), dispatchCalls.Load())
	}
}

func TestEncryptedRemotePairingRegistersPasskey(t *testing.T) {
	identity := newUplinkTestIdentity(t)
	pairingID := base64.RawURLEncoding.EncodeToString(make([]byte, 24))
	pairingSecret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	var beginCalls atomic.Int32
	var finishCalls atomic.Int32
	executor := remoteAccessExecutor{
		authorizedReadExecutor: authorizedReadExecutor{
			begin: func(context.Context) (homelink.LocalAssertionChallenge, error) {
				return homelink.LocalAssertionChallenge{}, errors.New("not used")
			},
			issue: func(
				context.Context,
				string,
				homelink.PasskeyAssertion,
				homelink.Scope,
				time.Duration,
				homelink.ReadBinding,
			) (homelink.Grant, error) {
				return homelink.Grant{}, errors.New("not used")
			},
			dispatch: func(
				context.Context,
				string,
				string,
				homelink.ReadRequest,
				homelink.ReadBinding,
			) (homelink.ReadResponse, error) {
				return homelink.ReadResponse{}, errors.New("not used")
			},
		},
		beginRegistration: func(
			_ context.Context,
			gotID string,
			gotSecret string,
			label string,
		) (homelink.RegistrationChallenge, error) {
			beginCalls.Add(1)
			if gotID != pairingID || gotSecret != pairingSecret || label != "Fredde's Mac" {
				t.Fatalf("registration pairing changed: %q %q %q", gotID, gotSecret, label)
			}
			return homelink.RegistrationChallenge{
				ID: "registration-1", Challenge: []byte("registration-challenge"),
				RPID: "home.sourceful.energy", UserHandle: []byte("local-user"),
				UserVerificationRequired: true, Attestation: "none",
				Algorithms: []int{-7},
			}, nil
		},
		finishRegistration: func(
			_ context.Context,
			expectationID string,
			responseJSON []byte,
		) (homelink.CredentialSummary, error) {
			finishCalls.Add(1)
			if expectationID != "registration-1" ||
				string(responseJSON) != `{"id":"credential","response":{}}` {
				t.Fatalf("registration finish changed: %q %s", expectationID, responseJSON)
			}
			return homelink.CredentialSummary{
				ID: "credential", Label: "Fredde's Mac", CreatedAtMS: 1, UpdatedAtMS: 1,
			}, nil
		},
	}
	service, err := NewServiceWithRemoteAccess(identity, executor)
	if err != nil {
		t.Fatal(err)
	}
	transport, accept, browserOutbound, browserInbound, cancel, result :=
		startConfirmedReadService(t, service, identity)
	defer stopReadService(t, cancel, result)

	begin := `{"version":1,"type":"registration.begin","request_id":"` +
		testRequestID(30) + `","pairing_id":"` + pairingID +
		`","pairing_secret":"` + pairingSecret + `","label":"Fredde's Mac"}`
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 2, []byte(begin),
	)}
	var challenge registrationChallengeMessage
	if err := wire.DecodeStrict(
		browserOpen(t, browserInbound, accept, waitValue(t, transport.sealed)),
		wire.MaxPlaintextBytes,
		&challenge,
	); err != nil {
		t.Fatal(err)
	}
	if challenge.Type != registrationChallengeType ||
		challenge.RequestID != testRequestID(30) ||
		challenge.ExpectationID != "registration-1" ||
		challenge.Challenge != base64.RawURLEncoding.EncodeToString(
			[]byte("registration-challenge"),
		) ||
		challenge.RPID != "home.sourceful.energy" ||
		challenge.UserHandle != base64.RawURLEncoding.EncodeToString([]byte("local-user")) ||
		challenge.Algorithm != -7 ||
		challenge.Attestation != "none" ||
		challenge.UserVerification != "required" ||
		challenge.Error != "" {
		t.Fatalf("registration challenge = %+v", challenge)
	}

	finish := `{"version":1,"type":"registration.finish","request_id":"` +
		testRequestID(31) +
		`","expectation_id":"registration-1","response":{"id":"credential","response":{}}}`
	transport.frames <- Frame{Type: wire.TypeSealed, Sealed: browserSeal(
		t, browserOutbound, accept, 3, []byte(finish),
	)}
	var registration registrationResultMessage
	if err := wire.DecodeStrict(
		browserOpen(t, browserInbound, accept, waitValue(t, transport.sealed)),
		wire.MaxPlaintextBytes,
		&registration,
	); err != nil {
		t.Fatal(err)
	}
	if registration.Type != registrationResultType ||
		registration.RequestID != testRequestID(31) ||
		registration.Error != "" ||
		registration.Credential == nil ||
		registration.Credential.ID != "credential" ||
		registration.Credential.Label != "Fredde's Mac" ||
		beginCalls.Load() != 1 ||
		finishCalls.Load() != 1 {
		t.Fatalf("registration result = %+v calls=%d/%d",
			registration, beginCalls.Load(), finishCalls.Load())
	}
}

func TestRemoteRegistrationEnvelopeIsStrict(t *testing.T) {
	requestID := testRequestID(40)
	pairingID := base64.RawURLEncoding.EncodeToString(make([]byte, 24))
	pairingSecret := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	validBegin := `{"version":1,"type":"registration.begin","request_id":"` +
		requestID + `","pairing_id":"` + pairingID +
		`","pairing_secret":"` + pairingSecret + `","label":"Browser"}`
	if _, err := decodeRegistrationBegin([]byte(validBegin)); err != nil {
		t.Fatalf("valid registration begin: %v", err)
	}
	for name, input := range map[string]string{
		"wrong id size": strings.Replace(validBegin, pairingID, testRequestID(1), 1),
		"padded secret": strings.Replace(validBegin, pairingSecret, pairingSecret+"=", 1),
		"empty label":   strings.Replace(validBegin, `"Browser"`, `""`, 1),
		"format label":  strings.Replace(validBegin, `"Browser"`, `"Browser\u202e"`, 1),
		"unknown field": strings.TrimSuffix(validBegin, "}") + `,"extra":true}`,
		"duplicate":     strings.Replace(validBegin, `"label":"Browser"`, `"label":"Browser","label":"Other"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRegistrationBegin([]byte(input)); err == nil {
				t.Fatal("invalid registration begin passed")
			}
		})
	}

	validFinish := `{"version":1,"type":"registration.finish","request_id":"` +
		requestID + `","expectation_id":"registration-1","response":{}}`
	if _, err := decodeRegistrationFinish([]byte(validFinish)); err != nil {
		t.Fatalf("valid registration finish: %v", err)
	}
	for name, input := range map[string]string{
		"null response":    strings.Replace(validFinish, `"response":{}`, `"response":null`, 1),
		"array response":   strings.Replace(validFinish, `"response":{}`, `"response":[]`, 1),
		"unknown field":    strings.TrimSuffix(validFinish, "}") + `,"extra":true}`,
		"duplicate result": strings.Replace(validFinish, `"response":{}`, `"response":{},"response":{}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRegistrationFinish([]byte(input)); err == nil {
				t.Fatal("invalid registration finish passed")
			}
		})
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

func TestRemotePasskeyEnvelopesAreClosedAndBounded(t *testing.T) {
	begin := `{"version":1,"type":"assertion.begin","request_id":"` +
		testRequestID(22) + `"}`
	if messageType, err := applicationMessageType([]byte(begin)); err != nil ||
		messageType != assertionBeginType {
		t.Fatalf("begin type = %q, %v", messageType, err)
	}
	if _, err := decodeAssertionBegin([]byte(begin)); err != nil {
		t.Fatal(err)
	}
	authorized := `{"version":1,"type":"read.authorize","request_id":"` +
		testRequestID(23) +
		`","challenge_id":"challenge-1","assertion":{"id":"credential","response":{}}` +
		`,"scope":"ftw.plan.read"}`
	if messageType, err := applicationMessageType([]byte(authorized)); err != nil ||
		messageType != authorizedReadType {
		t.Fatalf("authorized type = %q, %v", messageType, err)
	}
	if _, request, binding, err := decodeAuthorizedReadRequest(
		[]byte(authorized), testSessionContext(),
	); err != nil {
		t.Fatal(err)
	} else if request.Scope != homelink.ScopePlanRead ||
		binding.RequestHash == ([32]byte{}) {
		t.Fatalf("authorized request = %+v %+v", request, binding)
	}
	for _, invalid := range []string{
		strings.Replace(begin, `"request_id"`, `"extra":1,"request_id"`, 1),
		strings.Replace(begin, `"type":"assertion.begin"`,
			`"type":"assertion.begin","type":"assertion.begin"`, 1),
		strings.Replace(authorized, `"assertion":{"id":"credential","response":{}}`,
			`"assertion":null`, 1),
		strings.Replace(authorized, `"challenge_id":"challenge-1"`,
			`"challenge_id":""`, 1),
		authorized + `{}`,
		strings.Repeat(" ", maxReadRequestBytes+1),
	} {
		messageType, err := applicationMessageType([]byte(invalid))
		if err == nil {
			switch messageType {
			case assertionBeginType:
				_, err = decodeAssertionBegin([]byte(invalid))
			case authorizedReadType:
				_, _, _, err = decodeAuthorizedReadRequest(
					[]byte(invalid), testSessionContext(),
				)
			}
		}
		if err == nil {
			t.Fatalf("accepted invalid passkey envelope: %s", invalid)
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
