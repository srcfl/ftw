package gatewayidentity

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
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
	key, err := nova.LoadOrCreateIdentity(p.KeyPath)
	if err != nil {
		return nil, err
	}
	return &softwareIdentity{gatewayID: gatewayID, key: key}, nil
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
