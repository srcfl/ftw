// Package wire defines the versioned Home Link relay protocol.
package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
)

const (
	Version             = 1
	MaxHandshakeBytes   = 16 * 1024
	MaxSealedFrameBytes = 256 * 1024
	// MaxCiphertextBytes leaves room for the full JSON envelope at the
	// largest uint64 sequence number.
	MaxCiphertextBytes  = 191 * 1024
	MaxPlaintextBytes   = MaxCiphertextBytes - 16
	MaxBrowserStreams   = 16
	MachineProofDomain  = "ftw-home-link-uplink-auth/v1"
	SessionAcceptDomain = "ftw-home-link-session-accept/v1"
	ConnectionIDBytes   = 16
	MachineNonceBytes   = 32
	StreamIDBytes       = 16
	SessionIDBytes      = 16
	SessionNonceBytes   = 32
)

type Type string

const (
	TypeMachineHello     Type = "machine.hello"
	TypeMachineChallenge Type = "machine.challenge"
	TypeMachineProof     Type = "machine.proof"
	TypeMachineReady     Type = "machine.ready"
	TypeStreamOpen       Type = "stream.open"
	TypeStreamClose      Type = "stream.close"
	TypeSessionHello     Type = "session.hello"
	TypeSessionAccept    Type = "session.accept"
	TypeSealed           Type = "sealed"
)

type header struct {
	Version int  `json:"version"`
	Type    Type `json:"type"`
}

type MachineHello struct {
	Version     int    `json:"version"`
	Type        Type   `json:"type"`
	GatewayID   string `json:"gateway_id"`
	RouteHandle string `json:"route_handle"`
	PublicKey   string `json:"public_key"`
}

type MachineChallenge struct {
	Version      int    `json:"version"`
	Type         Type   `json:"type"`
	ConnectionID string `json:"connection_id"`
	Nonce        string `json:"nonce"`
	ExpiresAtMS  int64  `json:"expires_at_ms"`
}

type MachineProof struct {
	Version      int    `json:"version"`
	Type         Type   `json:"type"`
	ConnectionID string `json:"connection_id"`
	GatewayID    string `json:"gateway_id"`
	RouteHandle  string `json:"route_handle"`
	PublicKey    string `json:"public_key"`
	Nonce        string `json:"nonce"`
	ExpiresAtMS  int64  `json:"expires_at_ms"`
	Signature    string `json:"signature"`
}

type MachineReady struct {
	Version         int    `json:"version"`
	Type            Type   `json:"type"`
	ConnectionID    string `json:"connection_id"`
	RouteHandle     string `json:"route_handle"`
	RouteGeneration uint64 `json:"route_generation"`
}

type StreamOpen struct {
	Version         int    `json:"version"`
	Type            Type   `json:"type"`
	ConnectionID    string `json:"connection_id"`
	RouteGeneration uint64 `json:"route_generation"`
	RouteHandle     string `json:"route_handle"`
	StreamID        string `json:"stream_id"`
}

type StreamClose struct {
	Version  int    `json:"version"`
	Type     Type   `json:"type"`
	StreamID string `json:"stream_id"`
	Code     string `json:"code"`
}

type SessionHello struct {
	Version         int    `json:"version"`
	Type            Type   `json:"type"`
	ConnectionID    string `json:"connection_id"`
	RouteGeneration uint64 `json:"route_generation"`
	RouteHandle     string `json:"route_handle"`
	StreamID        string `json:"stream_id"`
	BrowserKey      string `json:"browser_key"`
	BrowserNonce    string `json:"browser_nonce"`
}

type SessionAccept struct {
	Version             int    `json:"version"`
	Type                Type   `json:"type"`
	ConnectionID        string `json:"connection_id"`
	GatewayID           string `json:"gateway_id"`
	RouteGeneration     uint64 `json:"route_generation"`
	RouteHandle         string `json:"route_handle"`
	StreamID            string `json:"stream_id"`
	SessionID           string `json:"session_id"`
	BrowserKey          string `json:"browser_key"`
	GatewayEphemeralKey string `json:"gateway_ephemeral_key"`
	GatewayPublicKey    string `json:"gateway_public_key"`
	BrowserNonce        string `json:"browser_nonce"`
	GatewayNonce        string `json:"gateway_nonce"`
	ExpiresAtMS         int64  `json:"expires_at_ms"`
	Signature           string `json:"signature"`
}

type Sealed struct {
	Version    int    `json:"version"`
	Type       Type   `json:"type"`
	StreamID   string `json:"stream_id"`
	Sequence   uint64 `json:"sequence"`
	Ciphertext string `json:"ciphertext"`
}

func MessageType(data []byte, limit int) (Type, error) {
	if err := validateJSON(data, limit); err != nil {
		return "", err
	}
	var h header
	if err := json.Unmarshal(data, &h); err != nil {
		return "", errors.New("wire header is invalid")
	}
	if h.Version != Version {
		return "", errors.New("wire version is unsupported")
	}
	switch h.Type {
	case TypeMachineHello, TypeMachineChallenge, TypeMachineProof, TypeMachineReady,
		TypeStreamOpen, TypeStreamClose, TypeSessionHello, TypeSessionAccept, TypeSealed:
		return h.Type, nil
	default:
		return "", errors.New("wire message type is unsupported")
	}
}

func DecodeMachineHello(data []byte) (MachineHello, []byte, error) {
	var message MachineHello
	if err := decodeExact(data, MaxHandshakeBytes, &message); err != nil {
		return MachineHello{}, nil, err
	}
	if err := validateHeader(message.Version, message.Type, TypeMachineHello); err != nil {
		return MachineHello{}, nil, err
	}
	normalized, err := gatewayidentity.NormalizeGatewayID(message.GatewayID)
	if err != nil || normalized != message.GatewayID {
		return MachineHello{}, nil, errors.New("machine gateway id is invalid")
	}
	publicKey, err := decodeCanonical(message.PublicKey, gatewayidentity.PublicKeyBytes, "machine public key")
	if err != nil {
		return MachineHello{}, nil, err
	}
	if err := gatewayidentity.ValidatePublicKey(publicKey); err != nil {
		return MachineHello{}, nil, errors.New("machine public key is invalid")
	}
	handle, err := gatewayidentity.RouteHandle(publicKey)
	if err != nil || handle != message.RouteHandle {
		return MachineHello{}, nil, errors.New("machine route handle does not match public key")
	}
	return message, publicKey, nil
}

func DecodeMachineChallenge(data []byte) (MachineChallenge, error) {
	var message MachineChallenge
	if err := decodeExact(data, MaxHandshakeBytes, &message); err != nil {
		return MachineChallenge{}, err
	}
	if err := validateHeader(message.Version, message.Type, TypeMachineChallenge); err != nil {
		return MachineChallenge{}, err
	}
	if _, err := decodeCanonical(message.ConnectionID, ConnectionIDBytes, "connection id"); err != nil {
		return MachineChallenge{}, err
	}
	if _, err := decodeCanonical(message.Nonce, MachineNonceBytes, "machine nonce"); err != nil {
		return MachineChallenge{}, err
	}
	if message.ExpiresAtMS <= 0 {
		return MachineChallenge{}, errors.New("machine challenge expiry is invalid")
	}
	return message, nil
}

func DecodeMachineProof(data []byte) (MachineProof, []byte, []byte, error) {
	var message MachineProof
	if err := decodeExact(data, MaxHandshakeBytes, &message); err != nil {
		return MachineProof{}, nil, nil, err
	}
	if err := validateHeader(message.Version, message.Type, TypeMachineProof); err != nil {
		return MachineProof{}, nil, nil, err
	}
	publicKey, err := validateMachineProofFields(message)
	if err != nil {
		return MachineProof{}, nil, nil, err
	}
	signature, err := decodeCanonical(message.Signature, gatewayidentity.SignatureBytes, "machine signature")
	if err != nil {
		return MachineProof{}, nil, nil, err
	}
	return message, publicKey, signature, nil
}

func validateMachineProofFields(message MachineProof) ([]byte, error) {
	if _, err := decodeCanonical(message.ConnectionID, ConnectionIDBytes, "connection id"); err != nil {
		return nil, err
	}
	normalized, err := gatewayidentity.NormalizeGatewayID(message.GatewayID)
	if err != nil || normalized != message.GatewayID {
		return nil, errors.New("machine gateway id is invalid")
	}
	publicKey, err := decodeCanonical(message.PublicKey, gatewayidentity.PublicKeyBytes, "machine public key")
	if err != nil || gatewayidentity.ValidatePublicKey(publicKey) != nil {
		return nil, errors.New("machine public key is invalid")
	}
	handle, err := gatewayidentity.RouteHandle(publicKey)
	if err != nil || handle != message.RouteHandle {
		return nil, errors.New("machine route handle does not match public key")
	}
	if _, err := decodeCanonical(message.Nonce, MachineNonceBytes, "machine nonce"); err != nil {
		return nil, err
	}
	if message.ExpiresAtMS <= 0 {
		return nil, errors.New("machine challenge expiry is invalid")
	}
	return publicKey, nil
}

func DecodeStreamOpen(data []byte) (StreamOpen, error) {
	var message StreamOpen
	if err := decodeExact(data, MaxHandshakeBytes, &message); err != nil {
		return StreamOpen{}, err
	}
	if err := validateHeader(message.Version, message.Type, TypeStreamOpen); err != nil {
		return StreamOpen{}, err
	}
	if _, err := decodeCanonical(message.ConnectionID, ConnectionIDBytes, "connection id"); err != nil {
		return StreamOpen{}, err
	}
	if message.RouteGeneration == 0 {
		return StreamOpen{}, errors.New("route generation must be positive")
	}
	if _, err := decodeCanonical(message.RouteHandle, gatewayidentity.RouteHandleBytes, "route handle"); err != nil {
		return StreamOpen{}, err
	}
	if _, err := decodeCanonical(message.StreamID, StreamIDBytes, "stream id"); err != nil {
		return StreamOpen{}, err
	}
	return message, nil
}

func DecodeMachineReady(data []byte) (MachineReady, error) {
	var message MachineReady
	if err := decodeExact(data, MaxHandshakeBytes, &message); err != nil {
		return MachineReady{}, err
	}
	if err := validateHeader(message.Version, message.Type, TypeMachineReady); err != nil {
		return MachineReady{}, err
	}
	if _, err := decodeCanonical(message.ConnectionID, ConnectionIDBytes, "connection id"); err != nil {
		return MachineReady{}, err
	}
	if _, err := decodeCanonical(message.RouteHandle, gatewayidentity.RouteHandleBytes, "route handle"); err != nil {
		return MachineReady{}, err
	}
	if message.RouteGeneration == 0 {
		return MachineReady{}, errors.New("route generation must be positive")
	}
	return message, nil
}

func DecodeStreamClose(data []byte) (StreamClose, error) {
	var message StreamClose
	if err := decodeExact(data, MaxHandshakeBytes, &message); err != nil {
		return StreamClose{}, err
	}
	if err := validateHeader(message.Version, message.Type, TypeStreamClose); err != nil {
		return StreamClose{}, err
	}
	if _, err := decodeCanonical(message.StreamID, StreamIDBytes, "stream id"); err != nil {
		return StreamClose{}, err
	}
	if message.Code == "" || len(message.Code) > 64 || hasUnsafeText(message.Code) {
		return StreamClose{}, errors.New("stream close code is invalid")
	}
	return message, nil
}

func DecodeSessionHello(data []byte) (SessionHello, []byte, error) {
	var message SessionHello
	if err := decodeExact(data, MaxHandshakeBytes, &message); err != nil {
		return SessionHello{}, nil, err
	}
	if err := validateHeader(message.Version, message.Type, TypeSessionHello); err != nil {
		return SessionHello{}, nil, err
	}
	if _, err := decodeCanonical(message.ConnectionID, ConnectionIDBytes, "connection id"); err != nil {
		return SessionHello{}, nil, err
	}
	if message.RouteGeneration == 0 {
		return SessionHello{}, nil, errors.New("route generation must be positive")
	}
	if _, err := decodeCanonical(message.RouteHandle, gatewayidentity.RouteHandleBytes, "route handle"); err != nil {
		return SessionHello{}, nil, err
	}
	if _, err := decodeCanonical(message.StreamID, StreamIDBytes, "stream id"); err != nil {
		return SessionHello{}, nil, err
	}
	browserKey, err := decodeCanonical(message.BrowserKey, gatewayidentity.PublicKeyBytes, "browser key")
	if err != nil || gatewayidentity.ValidatePublicKey(browserKey) != nil {
		return SessionHello{}, nil, errors.New("browser key is invalid")
	}
	if _, err := decodeCanonical(message.BrowserNonce, SessionNonceBytes, "browser nonce"); err != nil {
		return SessionHello{}, nil, err
	}
	return message, browserKey, nil
}

func DecodeSessionAccept(data []byte) (SessionAccept, []byte, []byte, []byte, error) {
	var message SessionAccept
	if err := decodeExact(data, MaxHandshakeBytes, &message); err != nil {
		return SessionAccept{}, nil, nil, nil, err
	}
	gatewayEphemeral, gatewayPublic, err := validateSessionAcceptFields(message)
	if err != nil {
		return SessionAccept{}, nil, nil, nil, err
	}
	signature, err := decodeCanonical(message.Signature, gatewayidentity.SignatureBytes, "session signature")
	if err != nil {
		return SessionAccept{}, nil, nil, nil, err
	}
	return message, gatewayEphemeral, gatewayPublic, signature, nil
}

func validateSessionAcceptFields(message SessionAccept) ([]byte, []byte, error) {
	if err := validateHeader(message.Version, message.Type, TypeSessionAccept); err != nil {
		return nil, nil, err
	}
	if _, err := decodeCanonical(message.ConnectionID, ConnectionIDBytes, "connection id"); err != nil {
		return nil, nil, err
	}
	normalized, err := gatewayidentity.NormalizeGatewayID(message.GatewayID)
	if err != nil || normalized != message.GatewayID {
		return nil, nil, errors.New("session gateway id is invalid")
	}
	if message.RouteGeneration == 0 {
		return nil, nil, errors.New("route generation must be positive")
	}
	if _, err := decodeCanonical(message.RouteHandle, gatewayidentity.RouteHandleBytes, "route handle"); err != nil {
		return nil, nil, err
	}
	if _, err := decodeCanonical(message.StreamID, StreamIDBytes, "stream id"); err != nil {
		return nil, nil, err
	}
	if _, err := decodeCanonical(message.SessionID, SessionIDBytes, "session id"); err != nil {
		return nil, nil, err
	}
	browserKey, err := decodeCanonical(message.BrowserKey, gatewayidentity.PublicKeyBytes, "browser key")
	if err != nil || gatewayidentity.ValidatePublicKey(browserKey) != nil {
		return nil, nil, errors.New("browser key is invalid")
	}
	gatewayEphemeral, err := decodeCanonical(message.GatewayEphemeralKey, gatewayidentity.PublicKeyBytes, "gateway ephemeral key")
	if err != nil || gatewayidentity.ValidatePublicKey(gatewayEphemeral) != nil {
		return nil, nil, errors.New("gateway ephemeral key is invalid")
	}
	gatewayPublic, err := decodeCanonical(message.GatewayPublicKey, gatewayidentity.PublicKeyBytes, "gateway public key")
	if err != nil || gatewayidentity.ValidatePublicKey(gatewayPublic) != nil {
		return nil, nil, errors.New("gateway public key is invalid")
	}
	handle, err := gatewayidentity.RouteHandle(gatewayPublic)
	if err != nil || handle != message.RouteHandle {
		return nil, nil, errors.New("session route handle does not match gateway key")
	}
	if _, err := decodeCanonical(message.BrowserNonce, SessionNonceBytes, "browser nonce"); err != nil {
		return nil, nil, err
	}
	if _, err := decodeCanonical(message.GatewayNonce, SessionNonceBytes, "gateway nonce"); err != nil {
		return nil, nil, err
	}
	if message.ExpiresAtMS <= 0 {
		return nil, nil, errors.New("session expiry is invalid")
	}
	return gatewayEphemeral, gatewayPublic, nil
}

func SessionAcceptMessage(message SessionAccept) ([]byte, error) {
	if _, _, err := validateSessionAcceptFields(message); err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%d\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n%d",
		SessionAcceptDomain,
		message.ConnectionID,
		message.GatewayID,
		message.RouteGeneration,
		message.RouteHandle,
		message.StreamID,
		message.SessionID,
		message.BrowserKey,
		message.GatewayEphemeralKey,
		message.GatewayPublicKey,
		message.BrowserNonce,
		message.GatewayNonce,
		message.ExpiresAtMS,
	)), nil
}

func DecodeSealed(data []byte) (Sealed, error) {
	var message Sealed
	if err := decodeExact(data, MaxSealedFrameBytes, &message); err != nil {
		return Sealed{}, err
	}
	if err := validateHeader(message.Version, message.Type, TypeSealed); err != nil {
		return Sealed{}, err
	}
	if _, err := decodeCanonical(message.StreamID, StreamIDBytes, "stream id"); err != nil {
		return Sealed{}, err
	}
	if message.Sequence == 0 {
		return Sealed{}, errors.New("sealed sequence must be positive")
	}
	if _, err := decodeCanonicalRange(message.Ciphertext, 1, MaxCiphertextBytes, "ciphertext"); err != nil {
		return Sealed{}, err
	}
	return message, nil
}

func MachineProofMessage(message MachineProof) ([]byte, error) {
	if err := validateHeader(message.Version, message.Type, TypeMachineProof); err != nil {
		return nil, err
	}
	publicKey, err := validateMachineProofFields(message)
	if err != nil {
		return nil, err
	}
	publicKeyText := base64.RawURLEncoding.EncodeToString(publicKey)
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%s\n%d",
		MachineProofDomain,
		message.ConnectionID,
		message.GatewayID,
		message.RouteHandle,
		publicKeyText,
		message.Nonce,
		message.ExpiresAtMS,
	)), nil
}

func Encode(message any, limit int) ([]byte, error) {
	data, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode wire message: %w", err)
	}
	if len(data) > limit {
		return nil, errors.New("wire message exceeds its size limit")
	}
	return data, nil
}

// DecodeStrict decodes one duplicate-free JSON value with no unknown fields
// or trailing input.
func DecodeStrict(data []byte, limit int, destination any) error {
	return decodeExact(data, limit, destination)
}

func validateHeader(version int, got, want Type) error {
	if version != Version {
		return errors.New("wire version is unsupported")
	}
	if got != want {
		return errors.New("wire message type is invalid")
	}
	return nil
}

func decodeExact(data []byte, limit int, destination any) error {
	if err := validateJSON(data, limit); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("wire message is invalid")
	}
	if err := requireEOF(decoder); err != nil {
		return err
	}
	return nil
}

func validateJSON(data []byte, limit int) error {
	if len(data) == 0 || len(data) > limit {
		return errors.New("wire message size is invalid")
	}
	if !json.Valid(data) {
		return errors.New("wire message is invalid JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := checkJSONValue(decoder); err != nil {
		return err
	}
	return requireEOF(decoder)
}

func checkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return errors.New("wire message is invalid JSON")
	}
	switch delimiter := token.(type) {
	case json.Delim:
		switch delimiter {
		case '{':
			names := make(map[string]struct{})
			for decoder.More() {
				nameToken, err := decoder.Token()
				if err != nil {
					return errors.New("wire message is invalid JSON")
				}
				name, ok := nameToken.(string)
				if !ok || hasUnsafeText(name) {
					return errors.New("wire object name is invalid")
				}
				if _, exists := names[name]; exists {
					return errors.New("wire object contains a duplicate name")
				}
				names[name] = struct{}{}
				if err := checkJSONValue(decoder); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return errors.New("wire object is incomplete")
			}
		case '[':
			for decoder.More() {
				if err := checkJSONValue(decoder); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return errors.New("wire array is incomplete")
			}
		default:
			return errors.New("wire message has an unexpected delimiter")
		}
	case string:
		if hasUnsafeText(delimiter) {
			return errors.New("wire string contains unsafe Unicode")
		}
	}
	return nil
}

func requireEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("wire message has trailing data")
	}
	return nil
}

func hasUnsafeText(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return true
		}
	}
	return false
}

func decodeCanonical(value string, length int, name string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != length || base64.RawURLEncoding.EncodeToString(raw) != value ||
		strings.Contains(value, "=") {
		return nil, fmt.Errorf("%s is invalid", name)
	}
	return raw, nil
}

func decodeCanonicalRange(value string, minimum, maximum int, name string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) < minimum || len(raw) > maximum ||
		base64.RawURLEncoding.EncodeToString(raw) != value || strings.Contains(value, "=") {
		return nil, fmt.Errorf("%s is invalid", name)
	}
	return raw, nil
}
