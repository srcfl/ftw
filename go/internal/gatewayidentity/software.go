package gatewayidentity

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"github.com/srcfl/ftw/go/internal/nova"
)

// GatewayIDSource returns an already stable software gateway ID. The runtime
// owner must persist its first derived value; a changing container MAC is not a
// valid source.
type GatewayIDSource func(context.Context) (string, error)

// SoftwareProvider adapts the existing site key file to the neutral identity
// contract. The key file remains compatibility storage, not the policy owner.
type SoftwareProvider struct {
	KeyPath   string
	GatewayID GatewayIDSource
}

func (p SoftwareProvider) Identity(ctx context.Context) (Identity, error) {
	if strings.TrimSpace(p.KeyPath) == "" {
		return nil, errors.New("canonical key path is empty")
	}
	if p.GatewayID == nil {
		return nil, errors.New("stable gateway id source is missing")
	}
	gatewayID, err := p.GatewayID(ctx)
	if err != nil {
		return nil, fmt.Errorf("read stable gateway id: %w", err)
	}
	gatewayID, err = NormalizeGatewayID(gatewayID)
	if err != nil {
		return nil, err
	}
	key, err := LoadOrCreateUnboundNovaIdentity(p.KeyPath)
	if err != nil {
		return nil, err
	}
	return &softwareIdentity{gatewayID: gatewayID, key: key}, nil
}

// BoundSoftwareProvider loads only an adopted, verified software identity.
// It never creates or replaces nova.key.
type BoundSoftwareProvider struct {
	KeyPath string
}

func (p BoundSoftwareProvider) Identity(context.Context) (Identity, error) {
	identity, _, err := LoadBoundSoftwareIdentity(p.KeyPath)
	return identity, err
}

func LoadBoundSoftwareIdentity(keyPath string) (Identity, SoftwareBinding, error) {
	paths, err := PathsForKey(keyPath)
	if err != nil {
		return nil, SoftwareBinding{}, err
	}
	store, err := openBindingStorage(paths, defaultBindingFileOps())
	if err != nil {
		return nil, SoftwareBinding{}, err
	}
	defer store.Close()
	if err := store.Lock(); err != nil {
		return nil, SoftwareBinding{}, err
	}
	binding, err := loadSoftwareBinding(store, paths)
	if err != nil {
		return nil, SoftwareBinding{}, err
	}
	keyBytes, err := store.Read(filepath.Base(paths.Key))
	if err != nil {
		return nil, SoftwareBinding{}, fmt.Errorf("read bound canonical key: %w", err)
	}
	publicKey, privateKey, err := parseSoftwarePrivateKey(keyBytes)
	if err != nil {
		return nil, SoftwareBinding{}, err
	}
	digest := sha256.Sum256(publicKey)
	if binding.PublicKeySHA256 != hex.EncodeToString(digest[:]) {
		return nil, SoftwareBinding{}, fmt.Errorf("%w: bound canonical key", ErrBindingMismatch)
	}
	identity := &boundSoftwareIdentity{
		gatewayID:  binding.GatewayID,
		publicKey:  publicKey,
		privateKey: privateKey,
	}
	if err := Validate(identity); err != nil {
		return nil, SoftwareBinding{}, err
	}
	return identity, binding, nil
}

// GatewayIDFromMAC follows Sourceful's software gateway layout:
// 0x01,0x23, the first three MAC bytes, 0x01, then the final three MAC bytes.
func GatewayIDFromMAC(mac net.HardwareAddr) (string, error) {
	if len(mac) != 6 {
		return "", fmt.Errorf("gateway MAC must contain 6 bytes")
	}
	return fmt.Sprintf("0123%x01%x", []byte(mac[:3]), []byte(mac[3:])), nil
}

type softwareIdentity struct {
	gatewayID string
	key       *nova.Identity
}

func (i *softwareIdentity) GatewayID() string { return i.gatewayID }

func (i *softwareIdentity) PublicKey() []byte {
	publicKey, _ := hex.DecodeString(i.key.PublicKeyHex())
	return publicKey
}

func (i *softwareIdentity) Sign(message []byte) ([]byte, error) {
	signature, err := i.key.SignRawHex(string(message))
	if err != nil {
		return nil, err
	}
	return hex.DecodeString(signature)
}

type boundSoftwareIdentity struct {
	gatewayID  string
	publicKey  []byte
	privateKey *ecdsa.PrivateKey
}

func (i *boundSoftwareIdentity) GatewayID() string { return i.gatewayID }

func (i *boundSoftwareIdentity) PublicKey() []byte {
	return append([]byte(nil), i.publicKey...)
}

func (i *boundSoftwareIdentity) Sign(message []byte) ([]byte, error) {
	if i.privateKey == nil {
		return nil, errors.New("bound software private key is missing")
	}
	digest := sha256.Sum256(message)
	r, s, err := ecdsa.Sign(rand.Reader, i.privateKey, digest[:])
	if err != nil {
		return nil, err
	}
	return append(
		r.FillBytes(make([]byte, 32)),
		s.FillBytes(make([]byte, 32))...,
	), nil
}
