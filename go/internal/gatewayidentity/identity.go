// Package gatewayidentity defines FTW's one gateway identity contract.
package gatewayidentity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const (
	GatewayIDBytes   = 9
	PublicKeyBytes   = 64
	SignatureBytes   = 64
	GatewayIDHexSize = GatewayIDBytes * 2
)

var ErrHardwareUnavailable = errors.New("gateway identity hardware unavailable")

// Identity exposes no private key bytes. Sign hashes the message once with
// SHA-256 and returns raw P-256 r||s bytes. Each caller must use a unique,
// versioned purpose tag in the message.
type Identity interface {
	GatewayID() string
	PublicKey() []byte
	Sign(message []byte) ([]byte, error)
}

// Provider resolves one identity source. A hardware provider must return
// ErrHardwareUnavailable only when no compatible chip exists.
type Provider interface {
	Identity(context.Context) (Identity, error)
}

type ProviderFunc func(context.Context) (Identity, error)

func (f ProviderFunc) Identity(ctx context.Context) (Identity, error) {
	return f(ctx)
}

type Source string

const (
	SourceHardware Source = "hardware"
	SourceSoftware Source = "software"
)

// Resolve tries compatible hardware first. It falls back only when hardware
// is absent. A present but broken or invalid chip fails closed, which keeps a
// transient hardware fault from rotating the gateway identity.
func Resolve(ctx context.Context, hardware, software Provider) (Identity, Source, error) {
	if hardware != nil {
		identity, err := hardware.Identity(ctx)
		switch {
		case err == nil:
			if err := Validate(identity); err != nil {
				return nil, "", fmt.Errorf("hardware gateway identity: %w", err)
			}
			return identity, SourceHardware, nil
		case !errors.Is(err, ErrHardwareUnavailable):
			return nil, "", fmt.Errorf("hardware gateway identity: %w", err)
		}
	}
	if software == nil {
		return nil, "", errors.New("software gateway identity provider is missing")
	}
	identity, err := software.Identity(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("software gateway identity: %w", err)
	}
	if err := Validate(identity); err != nil {
		return nil, "", fmt.Errorf("software gateway identity: %w", err)
	}
	return identity, SourceSoftware, nil
}

// NormalizeGatewayID accepts one 9-byte hexadecimal gateway ID and returns
// its lower-case wire form.
func NormalizeGatewayID(raw string) (string, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if len(raw) != GatewayIDHexSize {
		return "", fmt.Errorf("gateway id must contain %d hex characters", GatewayIDHexSize)
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != GatewayIDBytes {
		return "", fmt.Errorf("gateway id must contain %d hex characters", GatewayIDHexSize)
	}
	return hex.EncodeToString(decoded), nil
}

func Validate(identity Identity) error {
	if identity == nil {
		return errors.New("identity is nil")
	}
	if _, err := NormalizeGatewayID(identity.GatewayID()); err != nil {
		return err
	}
	return ValidatePublicKey(identity.PublicKey())
}

func ValidatePublicKey(raw []byte) error {
	if len(raw) != PublicKeyBytes {
		return fmt.Errorf("public key must contain %d bytes", PublicKeyBytes)
	}
	x := new(big.Int).SetBytes(raw[:32])
	y := new(big.Int).SetBytes(raw[32:])
	if !elliptic.P256().IsOnCurve(x, y) {
		return errors.New("public key is not a P-256 point")
	}
	return nil
}

// Verify checks a raw 64-byte r||s signature over SHA-256(message).
func Verify(publicKey, message, signature []byte) bool {
	if ValidatePublicKey(publicKey) != nil || len(signature) != SignatureBytes {
		return false
	}
	public := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(publicKey[:32]),
		Y:     new(big.Int).SetBytes(publicKey[32:]),
	}
	digest := sha256.Sum256(message)
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	return ecdsa.Verify(public, digest[:], r, s)
}
