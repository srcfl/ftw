// Package main — wormhole.go
//
// Re-export shim: the actual implementation lives in
// go/internal/wormhole. This file keeps the old package-main names
// available so the rest of ftw-pair doesn't need to change.
package main

import (
	"context"

	wh "github.com/frahlg/forty-two-watts/go/internal/wormhole"
)

// WormholeHost is an alias for wh.Host — host-side tunnel handle.
type WormholeHost = wh.Host

// WormholeClient is an alias for wh.Client — client-side tunnel handle.
type WormholeClient = wh.Client

// StartWormholeHost starts the relay-based tunnel and returns a WormholeHost
// whose Code field contains the 6-word token to share with the peer.
// The relay address is resolved from the -relay-addr flag (via relayAddrFlag),
// the FTW_PAIR_RELAY env var, or the Sourceful default.
func StartWormholeHost(ctx context.Context, remoteAddr string) (*WormholeHost, error) {
	return wh.StartHost(ctx, remoteAddr, wh.WithRelayAddr(*relayAddrFlag))
}
