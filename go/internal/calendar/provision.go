package calendar

import (
	"crypto/rand"
	"encoding/base64"
)

// GenerateToken returns a URL-safe random secret carrying nBytes of entropy.
// Used by main.go to mint the managed CalDAV password on first enable; the
// in-process native server (internal/caldavserver) authenticates against it.
func GenerateToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
