package homelinkuplink

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	"github.com/srcfl/ftw/go/internal/homelink"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
	"github.com/srcfl/ftw/go/internal/homelinksession"
	"github.com/srcfl/ftw/go/internal/state"
)

const (
	applicationVersion  = 1
	maxReadRequestBytes = 32 * 1024
	defaultReadTimeout  = 10 * time.Second
	readRequestIDBytes  = 16
	readGrantTokenBytes = 32
	readRequestType     = "read.request"
	readResponseType    = "read.response"
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

type readRequestMessage struct {
	Version   int                 `json:"version"`
	Type      string              `json:"type"`
	RequestID string              `json:"request_id"`
	Grant     string              `json:"grant"`
	Scope     homelink.Scope      `json:"scope"`
	History   *readHistoryRequest `json:"history,omitempty"`
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
	request := homelink.ReadRequest{
		Version:   homelink.ReadContractVersion,
		GatewayID: session.GatewayID,
		Scope:     message.Scope,
	}
	if message.History != nil {
		request.History = &state.EnergyHistoryQuery{
			AssetID:  message.History.AssetID,
			SinceMS:  message.History.SinceMS,
			UntilMS:  message.History.UntilMS,
			BucketMS: message.History.BucketMS,
			Limit:    message.History.Limit,
		}
	}
	if err := request.Validate(); err != nil {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	if request.Scope == homelink.ScopeEnergyHistoryRead &&
		request.History.Limit > homelink.MaxRemotePoints {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{},
			errors.New("Home Link history limit is too large")
	}
	hash, err := homelink.ReadRequestHash(message.RequestID, request)
	if err != nil {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	binding := homelink.ReadBinding{
		GatewayID:       session.GatewayID,
		RouteHandle:     session.RouteHandle,
		RouteGeneration: session.RouteGeneration,
		SessionID:       session.SessionID,
		StreamID:        session.StreamID,
		RequestHash:     hash,
	}
	if err := binding.Validate(); err != nil {
		return message, homelink.ReadRequest{}, homelink.ReadBinding{}, err
	}
	return message, request, binding, nil
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
