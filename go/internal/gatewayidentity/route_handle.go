package gatewayidentity

import (
	"crypto/sha256"
	"encoding/base64"
)

const (
	RouteHandleDomain = "ftw-home-link-route-v1"
	RouteHandleBytes  = sha256.Size
	RouteHandleSize   = 43
)

// RouteHandle is a self-certifying relay address. It is not a gateway ID,
// display name, user identity, or grant.
func RouteHandle(publicKey []byte) (string, error) {
	if err := ValidatePublicKey(publicKey); err != nil {
		return "", err
	}
	input := make([]byte, 0, len(RouteHandleDomain)+len(publicKey))
	input = append(input, RouteHandleDomain...)
	input = append(input, publicKey...)
	digest := sha256.Sum256(input)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}
