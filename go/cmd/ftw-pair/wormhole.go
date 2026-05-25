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

// StartWormholeHost starts a fowld subprocess and waits for the wormhole code
// to be allocated.  See wh.StartHost for the full contract.
func StartWormholeHost(ctx context.Context, remoteAddr string) (*WormholeHost, error) {
	return wh.StartHost(ctx, remoteAddr)
}

// ConnectWormholeClient joins an existing wormhole session and sets up a local
// TCP listener forwarded to the host's MCP server.  See wh.Connect for the
// full contract.
func ConnectWormholeClient(ctx context.Context, code string) (*WormholeClient, error) {
	return wh.Connect(ctx, code)
}
