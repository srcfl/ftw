// Package homelinksession provides the end-to-end browser-to-Core session.
package homelinksession

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
)

const (
	MaxSessionLifetime = 5 * time.Minute
	keyBytes           = 32
	keyMaterialBytes   = 2 * keyBytes
	sessionKeyDomain   = "ftw-home-link-session-keys/v1"
	sealedADDomain     = "ftw-home-link-sealed-ad/v1"
)

type Manager struct {
	identity gatewayidentity.Identity
	random   io.Reader
	now      func() time.Time
}

type Session struct {
	routeHandle string
	streamID    string
	sessionID   string
	expiresAt   time.Time
	now         func() time.Time
	inbound     cipher.AEAD
	outbound    cipher.AEAD

	mu          sync.Mutex
	inboundSeq  uint64
	outboundSeq uint64
}

func NewManager(identity gatewayidentity.Identity) (*Manager, error) {
	return newManager(identity, rand.Reader, time.Now)
}

func newManager(identity gatewayidentity.Identity, random io.Reader, now func() time.Time) (*Manager, error) {
	if err := gatewayidentity.Validate(identity); err != nil {
		return nil, fmt.Errorf("Home Link session identity: %w", err)
	}
	if random == nil || now == nil {
		return nil, errors.New("Home Link session dependency is missing")
	}
	return &Manager{identity: identity, random: random, now: now}, nil
}

func (m *Manager) Accept(hello wire.SessionHello) (wire.SessionAccept, *Session, error) {
	helloData, err := wire.Encode(hello, wire.MaxHandshakeBytes)
	if err != nil {
		return wire.SessionAccept{}, nil, err
	}
	_, browserKeyRaw, err := wire.DecodeSessionHello(helloData)
	if err != nil {
		return wire.SessionAccept{}, nil, err
	}
	routeHandle, err := gatewayidentity.RouteHandle(m.identity.PublicKey())
	if err != nil || hello.RouteHandle != routeHandle {
		return wire.SessionAccept{}, nil, errors.New("Home Link session route is invalid")
	}
	browserKey, err := parseECDHPublicKey(browserKeyRaw)
	if err != nil {
		return wire.SessionAccept{}, nil, errors.New("Home Link browser key is invalid")
	}
	gatewayPrivate, err := ecdh.P256().GenerateKey(m.random)
	if err != nil {
		return wire.SessionAccept{}, nil, fmt.Errorf("create Home Link ephemeral key: %w", err)
	}
	sharedSecret, err := gatewayPrivate.ECDH(browserKey)
	if err != nil {
		return wire.SessionAccept{}, nil, errors.New("derive Home Link session secret")
	}
	gatewayEphemeral, err := rawECDHPublicKey(gatewayPrivate.PublicKey())
	if err != nil {
		return wire.SessionAccept{}, nil, err
	}
	sessionID, err := randomRawURL(m.random, wire.SessionIDBytes)
	if err != nil {
		return wire.SessionAccept{}, nil, err
	}
	gatewayNonce, err := randomRawURL(m.random, wire.SessionNonceBytes)
	if err != nil {
		return wire.SessionAccept{}, nil, err
	}
	expiresAt := m.now().UTC().Add(MaxSessionLifetime)
	accept := wire.SessionAccept{
		Version: wire.Version, Type: wire.TypeSessionAccept,
		ConnectionID: hello.ConnectionID, GatewayID: m.identity.GatewayID(),
		RouteGeneration: hello.RouteGeneration,
		RouteHandle:     hello.RouteHandle, StreamID: hello.StreamID,
		SessionID: sessionID, BrowserKey: hello.BrowserKey,
		GatewayEphemeralKey: base64.RawURLEncoding.EncodeToString(gatewayEphemeral),
		GatewayPublicKey:    base64.RawURLEncoding.EncodeToString(m.identity.PublicKey()),
		BrowserNonce:        hello.BrowserNonce, GatewayNonce: gatewayNonce,
		ExpiresAtMS: expiresAt.UnixMilli(),
	}
	transcript, err := wire.SessionAcceptMessage(accept)
	if err != nil {
		return wire.SessionAccept{}, nil, err
	}
	signature, err := m.identity.Sign(transcript)
	if err != nil {
		return wire.SessionAccept{}, nil, fmt.Errorf("sign Home Link session: %w", err)
	}
	accept.Signature = base64.RawURLEncoding.EncodeToString(signature)
	inbound, outbound, err := deriveAEADs(sharedSecret, transcript)
	if err != nil {
		return wire.SessionAccept{}, nil, err
	}
	return accept, &Session{
		routeHandle: hello.RouteHandle, streamID: hello.StreamID,
		sessionID: sessionID, expiresAt: expiresAt, now: m.now,
		inbound: inbound, outbound: outbound,
	}, nil
}

func (s *Session) Decrypt(message wire.Sealed) ([]byte, error) {
	data, err := wire.Encode(message, wire.MaxSealedFrameBytes)
	if err != nil {
		return nil, err
	}
	decoded, err := wire.DecodeSealed(data)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.now().Before(s.expiresAt) {
		return nil, errors.New("Home Link session has expired")
	}
	if decoded.StreamID != s.streamID || decoded.Sequence != s.inboundSeq+1 {
		return nil, errors.New("Home Link inbound sequence is invalid")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(decoded.Ciphertext)
	if err != nil {
		return nil, errors.New("Home Link ciphertext is invalid")
	}
	plaintext, err := s.inbound.Open(nil, nonce(decoded.Sequence),
		ciphertext, s.additionalData("browser-to-gateway", decoded.Sequence))
	if err != nil {
		return nil, errors.New("Home Link ciphertext authentication failed")
	}
	s.inboundSeq = decoded.Sequence
	return plaintext, nil
}

func (s *Session) Encrypt(plaintext []byte) (wire.Sealed, error) {
	if len(plaintext) == 0 || len(plaintext) > wire.MaxPlaintextBytes {
		return wire.Sealed{}, errors.New("Home Link plaintext size is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.now().Before(s.expiresAt) {
		return wire.Sealed{}, errors.New("Home Link session has expired")
	}
	if s.outboundSeq == ^uint64(0) {
		return wire.Sealed{}, errors.New("Home Link outbound sequence is exhausted")
	}
	sequence := s.outboundSeq + 1
	ciphertext := s.outbound.Seal(nil, nonce(sequence), plaintext,
		s.additionalData("gateway-to-browser", sequence))
	message := wire.Sealed{
		Version: wire.Version, Type: wire.TypeSealed,
		StreamID: s.streamID, Sequence: sequence,
		Ciphertext: base64.RawURLEncoding.EncodeToString(ciphertext),
	}
	data, err := wire.Encode(message, wire.MaxSealedFrameBytes)
	if err != nil {
		return wire.Sealed{}, err
	}
	if _, err := wire.DecodeSealed(data); err != nil {
		return wire.Sealed{}, err
	}
	s.outboundSeq = sequence
	return message, nil
}

// VerifyAccept checks the site, route and gateway signature that a browser
// expects before it derives session keys.
func VerifyAccept(expectedGatewayID, expectedRouteHandle string, accept wire.SessionAccept) error {
	normalized, err := gatewayidentity.NormalizeGatewayID(expectedGatewayID)
	if err != nil || normalized != expectedGatewayID || accept.GatewayID != expectedGatewayID {
		return errors.New("Home Link session gateway is invalid")
	}
	if accept.RouteHandle != expectedRouteHandle {
		return errors.New("Home Link session route is invalid")
	}
	data, err := wire.Encode(accept, wire.MaxHandshakeBytes)
	if err != nil {
		return err
	}
	decoded, _, publicKey, signature, err := wire.DecodeSessionAccept(data)
	if err != nil {
		return err
	}
	transcript, err := wire.SessionAcceptMessage(decoded)
	if err != nil {
		return err
	}
	if !gatewayidentity.Verify(publicKey, transcript, signature) {
		return errors.New("Home Link session signature is invalid")
	}
	return nil
}

func (s *Session) additionalData(direction string, sequence uint64) []byte {
	return []byte(fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%d",
		sealedADDomain, s.routeHandle, s.streamID, s.sessionID, direction, sequence))
}

func deriveAEADs(sharedSecret, transcript []byte) (cipher.AEAD, cipher.AEAD, error) {
	salt := sha256.Sum256(transcript)
	keyMaterial, err := hkdf.Key(sha256.New, sharedSecret, salt[:],
		sessionKeyDomain, keyMaterialBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("derive Home Link session keys: %w", err)
	}
	inbound, err := newGCM(keyMaterial[:keyBytes])
	if err != nil {
		return nil, nil, err
	}
	outbound, err := newGCM(keyMaterial[keyBytes:])
	if err != nil {
		return nil, nil, err
	}
	return inbound, outbound, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(sequence uint64) []byte {
	value := make([]byte, 12)
	binary.BigEndian.PutUint64(value[4:], sequence)
	return value
}

func parseECDHPublicKey(raw []byte) (*ecdh.PublicKey, error) {
	encoded := make([]byte, 1+len(raw))
	encoded[0] = 4
	copy(encoded[1:], raw)
	return ecdh.P256().NewPublicKey(encoded)
}

func rawECDHPublicKey(publicKey *ecdh.PublicKey) ([]byte, error) {
	encoded := publicKey.Bytes()
	if len(encoded) != 1+gatewayidentity.PublicKeyBytes || encoded[0] != 4 {
		return nil, errors.New("Home Link ephemeral key encoding is invalid")
	}
	return append([]byte(nil), encoded[1:]...), nil
}

func randomRawURL(random io.Reader, length int) (string, error) {
	raw := make([]byte, length)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("create Home Link session value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
