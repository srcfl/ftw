// owner_tunnel.go — thin wrapper around internal/tunnel.Host so the
// owner-access long-poll loop has a stable construction point.
package main

import (
	"crypto/rand"
	"net/http"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

func newOwnerTunnelHost(relayURL, hostID string, h http.Handler) *tunnel.Host {
	return tunnel.NewHost(relayURL, hostID, h)
}

// cryptoRandRead is exported via package-internal alias so the
// owner_relay_register.go helper has a single place to swap in tests.
func cryptoRandRead(b []byte) (int, error) {
	return rand.Read(b)
}
