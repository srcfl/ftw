// Package main — subetha.go
//
// Re-export shim: the actual implementation lives in go/internal/subetha.
// This file keeps the package-main names ergonomic for the rest of ftw-pair.
package main

import (
	"context"

	"github.com/frahlg/forty-two-watts/go/internal/subetha"
)

// SubethaHost is an alias for subetha.Host — host-side tunnel handle.
type SubethaHost = subetha.Host

// SubethaClient is an alias for subetha.Client — client-side tunnel handle.
type SubethaClient = subetha.Client

// StartSubethaHost starts the relay-based tunnel and returns a SubethaHost
// whose Code field contains the 6-word token to share with the peer.
// The relay address is resolved from the -relay-addr flag (via relayAddrFlag),
// the FTW_PAIR_RELAY env var, or the Sourceful default.
func StartSubethaHost(ctx context.Context, remoteAddr string) (*SubethaHost, error) {
	return subetha.StartHost(ctx, remoteAddr, subetha.WithRelayAddr(*relayAddrFlag))
}
