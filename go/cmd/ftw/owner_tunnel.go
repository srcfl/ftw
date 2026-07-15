// owner_tunnel.go — small shared helper for the owner-access relay registration.
// The owner HTTP request/response tunnel was removed in the P2P-only cutover
// (slice 6); only the crypto-rand shim used to mint the stable host_id remains.
package main

import (
	"crypto/rand"
)

// cryptoRandRead is exported via package-internal alias so the
// owner_relay_register.go helper has a single place to swap in tests.
func cryptoRandRead(b []byte) (int, error) {
	return rand.Read(b)
}
