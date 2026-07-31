package homelinkuplink

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/srcfl/ftw/go/internal/homelink"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
	"github.com/srcfl/ftw/go/internal/homelinksession"
	"github.com/srcfl/ftw/go/internal/state"
)

const (
	applicationVersion        = 1
	maxReadRequestBytes       = 32 * 1024
	maxAssertionBytes         = 16 * 1024
	defaultReadTimeout        = 10 * time.Second
	readRequestIDBytes        = 16
	readGrantTokenBytes       = 32
	readRequestType           = "read.request"
	assertionBeginType        = "assertion.begin"
	assertionResultType       = "assertion.challenge"
	registrationBeginType     = "registration.begin"
	registrationChallengeType = "registration.challenge"
	registrationFinishType    = "registration.finish"
	registrationResultType    = "registration.result"
	authorizedReadType        = "read.authorize"
	sessionAuthorizeType      = "session.authorize"
	sessionAuthorizedType     = "session.authorized"
	sessionReadType           = "session.read"
	readResponseType          = "read.response"
)

type ReadExecutor interface {
	VerifyAndDispatchBoundRead(
		context.Context,
		string,
		string,
		homelink.ReadRequest,
		homelink.ReadBinding,
	) (homelink.ReadResponse, error)
}

type AuthorizedReadExecutor interface {
	ReadExecutor
	BeginLocalAssertion(context.Context) (homelink.LocalAssertionChallenge, error)
	IssueOneUseBoundAccess(
		context.Context,
		string,
		homelink.PasskeyAssertion,
		homelink.Scope,
		time.Duration,
		homelink.ReadBinding,
	) (homelink.Grant, error)
	IssueReadSession(
		context.Context,
		string,
		homelink.PasskeyAssertion,
		time.Duration,
		homelink.ReadSessionBinding,
	) (homelink.ReadSessionGrant, error)
	VerifyAndDispatchSessionRead(
		context.Context,
		string,
		string,
		homelink.ReadRequest,
		homelink.ReadBinding,
	) (homelink.ReadResponse, error)
}

type RemoteAccessExecutor interface {
	AuthorizedReadExecutor
	BeginRegistration(
		context.Context,
		string,
		string,
		string,
	) (homelink.RegistrationChallenge, error)
	FinishRegistration(
		context.Context,
		string,
		[]byte,
	) (homelink.CredentialSummary, error)
}

type assertionBeginMessage struct {
	Version   int    `json:"version"`
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
}

type assertionChallengeMessage struct {
	Version          int      `json:"version"`
	Type             string   `json:"type"`
	RequestID        string   `json:"request_id"`
	ChallengeID      string   `json:"challenge_id,omitempty"`
	Challenge        string   `json:"challenge,omitempty"`
	RPID             string   `json:"rp_id,omitempty"`
	AllowCredentials []string `json:"allow_credentials,omitempty"`
	UserVerification string   `json:"user_verification,omitempty"`
	Error            string   `json:"error,omitempty"`
}

type authorizedReadRequestMessage struct {
	Version     int                 `json:"version"`
	Type        string              `json:"type"`
	RequestID   string              `json:"request_id"`
	ChallengeID string              `json:"challenge_id"`
	Assertion   json.RawMessage     `json:"assertion"`
	Scope       homelink.Scope      `json:"scope"`
	History     *readHistoryRequest `json:"history,omitempty"`
}

type sessionAuthorizeMessage struct {
	Version     int             `json:"version"`
	Type        string          `json:"type"`
	RequestID   string          `json:"request_id"`
	ChallengeID string          `json:"challenge_id"`
	Assertion   json.RawMessage `json:"assertion"`
}

type sessionAuthorizedMessage struct {
	Version     int    `json:"version"`
	Type        string `json:"type"`
	RequestID   string `json:"request_id"`
	ExpiresAtMS int64  `json:"expires_at_ms,omitempty"`
	Error       string `json:"error,omitempty"`
}

type sessionReadRequestMessage struct {
	Version   int                 `json:"version"`
	Type      string              `json:"type"`
	RequestID string              `json:"request_id"`
	Scope     homelink.Scope      `json:"scope"`
	History   *readHistoryRequest `json:"history,omitempty"`
}

type registrationBeginMessage struct {
	Version       int    `json:"version"`
	Type          string `json:"type"`
	RequestID     string `json:"request_id"`
	PairingID     string `json:"pairing_id"`
	PairingSecret string `json:"pairing_secret"`
	Label         string `json:"label"`
}

type registrationChallengeMessage struct {
	Version          int    `json:"version"`
	Type             string `json:"type"`
	RequestID        string `json:"request_id"`
	ExpectationID    string `json:"expectation_id,omitempty"`
	Challenge        string `json:"challenge,omitempty"`
	RPID             string `json:"rp_id,omitempty"`
	UserHandle       string `json:"user_handle,omitempty"`
	Algorithm        int    `json:"algorithm,omitempty"`
	Attestation      string `json:"attestation,omitempty"`
	UserVerification string `json:"user_verification,omitempty"`
	Error            string `json:"error,omitempty"`
}

type registrationFinishMessage struct {
	Version       int             `json:"version"`
	Type          string          `json:"type"`
	RequestID     string          `json:"request_id"`
	ExpectationID string          `json:"expectation_id"`
	Response      json.RawMessage `json:"response"`
}

type registrationResultMessage struct {
	Version    int                         `json:"version"`
	Type       string                      `json:"type"`
	RequestID  string                      `json:"request_id"`
	Credential *homelink.CredentialSummary `json:"credential,omitempty"`
	Error      string                      `json:"error,omitempty"`
}

type readRequestMessage struct {
	Version   int                 `json:"version"`
	Type      string              `json:"type"`
	RequestID string              `json:"request_id"`
	Grant     string              `json:"grant"`
	Scope     homelink.Scope      `json:"scope"`
	History   *readHistoryRequest `json:"history,omitempty"`
}

func applicationMessageType(data []byte) (string, error) {
	var object map[string]json.RawMessage
	if err := wire.DecodeStrict(data, maxReadRequestBytes, &object); err != nil {
		return "", err
	}
	var version int
	var messageType string
	if err := json.Unmarshal(object["version"], &version); err != nil ||
		json.Unmarshal(object["type"], &messageType) != nil ||
		version != applicationVersion {
		return "", errors.New("Home Link application envelope is invalid")
	}
	switch messageType {
	case readRequestType, assertionBeginType, authorizedReadType,
		sessionAuthorizeType, sessionReadType,
		registrationBeginType, registrationFinishType:
		return messageType, nil
	default:
		return "", errors.New("Home Link application type is invalid")
	}
}

func decodeSessionAuthorize(data []byte) (sessionAuthorizeMessage, error) {
	var message sessionAuthorizeMessage
	if err := wire.DecodeStrict(data, maxReadRequestBytes, &message); err != nil {
		return message, err
	}
	if message.Version != applicationVersion ||
		message.Type != sessionAuthorizeType ||
		!validReadRequestID(message.RequestID) ||
		message.ChallengeID == "" ||
		len(message.Assertion) == 0 {
		return message, errors.New("Home Link session authorization is invalid")
	}
	var assertionObject map[string]json.RawMessage
	if err := wire.DecodeStrict(
		message.Assertion, maxAssertionBytes, &assertionObject,
	); err != nil || assertionObject == nil {
		return message, errors.New("Home Link passkey assertion is invalid")
	}
	return message, nil
}

func encodeSessionAuthorized(
	requestID string,
	expiresAt time.Time,
	err error,
) ([]byte, error) {
	message := sessionAuthorizedMessage{
		Version: applicationVersion, Type: sessionAuthorizedType,
		RequestID: requestID,
	}
	if err != nil {
		message.Error = readErrorCode(err)
	} else {
		message.ExpiresAtMS = expiresAt.UnixMilli()
	}
	return wire.Encode(message, wire.MaxPlaintextBytes)
}

func decodeRegistrationBegin(data []byte) (registrationBeginMessage, error) {
	var message registrationBeginMessage
	if err := wire.DecodeStrict(data, maxReadRequestBytes, &message); err != nil {
		return message, err
	}
	if message.Version != applicationVersion ||
		message.Type != registrationBeginType ||
		!validReadRequestID(message.RequestID) ||
		!validRawURL(message.PairingID, 24) ||
		!validRawURL(message.PairingSecret, 32) ||
		!safeApplicationText(message.Label, 80) {
		return message, errors.New("Home Link registration envelope is invalid")
	}
	return message, nil
}

func encodeRegistrationChallenge(
	requestID string,
	challenge homelink.RegistrationChallenge,
	err error,
) ([]byte, error) {
	message := registrationChallengeMessage{
		Version: applicationVersion, Type: registrationChallengeType,
		RequestID: requestID,
	}
	if err != nil {
		message.Error = readErrorCode(err)
	} else {
		if len(challenge.Algorithms) != 1 {
			return nil, errors.New("Home Link registration algorithm is invalid")
		}
		message.ExpectationID = challenge.ID
		message.Challenge = base64.RawURLEncoding.EncodeToString(challenge.Challenge)
		message.RPID = challenge.RPID
		message.UserHandle = base64.RawURLEncoding.EncodeToString(challenge.UserHandle)
		message.Algorithm = challenge.Algorithms[0]
		message.Attestation = challenge.Attestation
		message.UserVerification = "required"
	}
	return wire.Encode(message, wire.MaxPlaintextBytes)
}

func decodeRegistrationFinish(data []byte) (registrationFinishMessage, error) {
	var message registrationFinishMessage
	if err := wire.DecodeStrict(data, maxReadRequestBytes, &message); err != nil {
		return message, err
	}
	if message.Version != applicationVersion ||
		message.Type != registrationFinishType ||
		!validReadRequestID(message.RequestID) ||
		message.ExpectationID == "" ||
		len(message.Response) == 0 {
		return message, errors.New("Home Link registration result is invalid")
	}
	var response map[string]json.RawMessage
	if err := wire.DecodeStrict(message.Response, maxAssertionBytes, &response); err != nil ||
		response == nil {
		return message, errors.New("Home Link registration response is invalid")
	}
	return message, nil
}

func encodeRegistrationResult(
	requestID string,
	credential homelink.CredentialSummary,
	err error,
) ([]byte, error) {
	message := registrationResultMessage{
		Version: applicationVersion, Type: registrationResultType,
		RequestID: requestID,
	}
	if err != nil {
		message.Error = readErrorCode(err)
	} else {
		message.Credential = &credential
	}
	return wire.Encode(message, wire.MaxPlaintextBytes)
}

func safeApplicationText(value string, maxBytes int) bool {
	if value == "" || value != strings.TrimSpace(value) ||
		len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) ||
			unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

func decodeAssertionBegin(data []byte) (assertionBeginMessage, error) {
	var message assertionBeginMessage
	if err := wire.DecodeStrict(data, maxReadRequestBytes, &message); err != nil {
		return message, err
	}
	if message.Version != applicationVersion ||
		message.Type != assertionBeginType ||
		!validReadRequestID(message.RequestID) {
		return message, errors.New("Home Link assertion envelope is invalid")
	}
	return message, nil
}

func encodeAssertionChallenge(
	requestID string,
	challenge homelink.LocalAssertionChallenge,
	err error,
) ([]byte, error) {
	message := assertionChallengeMessage{
		Version: applicationVersion, Type: assertionResultType, RequestID: requestID,
	}
	if err != nil {
		message.Error = readErrorCode(err)
	} else {
		message.ChallengeID = challenge.ID
		message.Challenge = base64.RawURLEncoding.EncodeToString(challenge.Challenge)
		message.RPID = challenge.RPID
		message.UserVerification = "required"
		message.AllowCredentials = make([]string, len(challenge.AllowCredentials))
		for index, credentialID := range challenge.AllowCredentials {
			message.AllowCredentials[index] =
				base64.RawURLEncoding.EncodeToString(credentialID)
		}
	}
	return wire.Encode(message, wire.MaxPlaintextBytes)
}

type readHistoryRequest struct {
	AssetID  string `json:"asset_id"`
	SinceMS  int64  `json:"since_ms"`
	UntilMS  int64  `json:"until_ms"`
	BucketMS int64  `json:"bucket_ms"`
	Limit    int    `json:"limit"`
}

type readResponseMessage struct {
	Version   int                    `json:"version"`
	Type      string                 `json:"type"`
	RequestID string                 `json:"request_id"`
	Response  *homelink.ReadResponse `json:"response,omitempty"`
	Error     string                 `json:"error,omitempty"`
}

func decodeReadRequest(
	data []byte,
	session homelinksession.Context,
) (readRequestMessage, homelink.ReadRequest, homelink.ReadBinding, error) {
	var message readRequestMessage
	if err := wire.DecodeStrict(data, maxReadRequestBytes, &message); err != nil {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	if message.Version != applicationVersion || message.Type != readRequestType ||
		!validRawURL(message.Grant, readGrantTokenBytes) ||
		!validReadRequestID(message.RequestID) {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{},
			errors.New("Home Link read envelope is invalid")
	}
	request, binding, err := buildReadRequest(
		message.RequestID, message.Scope, message.History, session,
	)
	if err != nil {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	return message, request, binding, nil
}

func decodeSessionReadRequest(
	data []byte,
	session homelinksession.Context,
) (
	sessionReadRequestMessage,
	homelink.ReadRequest,
	homelink.ReadBinding,
	error,
) {
	var message sessionReadRequestMessage
	if err := wire.DecodeStrict(data, maxReadRequestBytes, &message); err != nil {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	if message.Version != applicationVersion ||
		message.Type != sessionReadType ||
		!validReadRequestID(message.RequestID) {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{},
			errors.New("Home Link session read envelope is invalid")
	}
	request, binding, err := buildReadRequest(
		message.RequestID, message.Scope, message.History, session,
	)
	if err != nil {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	return message, request, binding, nil
}

func decodeAuthorizedReadRequest(
	data []byte,
	session homelinksession.Context,
) (
	authorizedReadRequestMessage,
	homelink.ReadRequest,
	homelink.ReadBinding,
	error,
) {
	var message authorizedReadRequestMessage
	if err := wire.DecodeStrict(data, maxReadRequestBytes, &message); err != nil {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	if message.Version != applicationVersion ||
		message.Type != authorizedReadType ||
		!validReadRequestID(message.RequestID) ||
		message.ChallengeID == "" ||
		len(message.Assertion) == 0 {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{},
			errors.New("Home Link authorized read envelope is invalid")
	}
	var assertionObject map[string]json.RawMessage
	if err := wire.DecodeStrict(
		message.Assertion, maxAssertionBytes, &assertionObject,
	); err != nil || assertionObject == nil {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{},
			errors.New("Home Link passkey assertion is invalid")
	}
	request, binding, err := buildReadRequest(
		message.RequestID, message.Scope, message.History, session,
	)
	if err != nil {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	return message, request, binding, nil
}

func buildReadRequest(
	requestID string,
	scope homelink.Scope,
	history *readHistoryRequest,
	session homelinksession.Context,
) (homelink.ReadRequest, homelink.ReadBinding, error) {
	request := homelink.ReadRequest{
		Version: homelink.ReadContractVersion, GatewayID: session.GatewayID,
		Scope: scope,
	}
	if history != nil {
		request.History = &state.EnergyHistoryQuery{
			AssetID: history.AssetID, SinceMS: history.SinceMS,
			UntilMS: history.UntilMS, BucketMS: history.BucketMS,
			Limit: history.Limit,
		}
	}
	if err := request.Validate(); err != nil {
		return homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	if request.Scope == homelink.ScopeEnergyHistoryRead &&
		request.History.Limit > homelink.MaxRemotePoints {
		return homelink.ReadRequest{}, homelink.ReadBinding{},
			errors.New("Home Link history limit is too large")
	}
	hash, err := homelink.ReadRequestHash(requestID, request)
	if err != nil {
		return homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	binding := homelink.ReadBinding{
		GatewayID: session.GatewayID, RouteHandle: session.RouteHandle,
		RouteGeneration: session.RouteGeneration, SessionID: session.SessionID,
		StreamID: session.StreamID, RequestHash: hash,
	}
	if err := binding.Validate(); err != nil {
		return homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	return request, binding, nil
}

func validReadRequestID(value string) bool {
	return validRawURL(value, readRequestIDBytes)
}

func validRawURL(value string, size int) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == size &&
		base64.RawURLEncoding.EncodeToString(raw) == value
}

func encodeReadResponse(
	requestID string,
	response homelink.ReadResponse,
	err error,
) ([]byte, error) {
	message := readResponseMessage{
		Version:   applicationVersion,
		Type:      readResponseType,
		RequestID: requestID,
	}
	if err == nil {
		if err := response.Validate(); err != nil {
			message.Error = "invalid-core-response"
		} else {
			message.Response = &response
		}
	} else {
		message.Error = readErrorCode(err)
	}
	data, encodeErr := wire.Encode(message, wire.MaxPlaintextBytes)
	if encodeErr == nil {
		return data, nil
	}
	message.Response = nil
	message.Error = "response-too-large"
	return wire.Encode(message, wire.MaxPlaintextBytes)
}

func readErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, homelink.ErrRemoteDisabled):
		return "remote-disabled"
	case errors.Is(err, homelink.ErrGrantExpired):
		return "grant-expired"
	case errors.Is(err, homelink.ErrGrantConsumed):
		return "grant-consumed"
	case errors.Is(err, homelink.ErrGrantRevoked),
		errors.Is(err, homelink.ErrCredentialRevoked):
		return "grant-revoked"
	case errors.Is(err, homelink.ErrWrongBinding),
		errors.Is(err, homelink.ErrWrongScope),
		errors.Is(err, homelink.ErrWrongSite),
		errors.Is(err, homelink.ErrInvalidGrant):
		return "denied"
	default:
		return "read-failed"
	}
}
