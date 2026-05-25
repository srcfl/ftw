// Package main — wormhole.go
//
// # Magic-wormhole TCP port-forward shim
//
// wormhole-william v1.0.8 implements only file/text/directory transfer; it has
// no TCP port-forwarding primitive (no forward sub-package, no Tunnel API).
// This file therefore implements TCP forwarding on top of the text-transfer API:
//
//  1. StartWormholeHost:
//     - Binds a relay TCP listener on a random port (127.0.0.1:0).
//     - Sends that listener address as a wormhole text message; the library
//       allocates the human-shareable code and returns it.
//     - Runs an accept loop: for every incoming relay connection, dials
//       remoteAddr and bidirectionally copies bytes (io.Copy pair).
//
//  2. ConnectWormholeClient:
//     - Calls Client.Receive to claim the wormhole code and read the text
//       payload (the relay address).
//     - Binds a local listener on a random port (127.0.0.1:0); exposes that
//       address as LocalAddr.
//     - Runs an accept loop: for every local connection, dials the relay
//       address and bidirectionally copies bytes.
//
// Net result: bytes written to WormholeClient.LocalAddr arrive at the
// remoteAddr that was passed to StartWormholeHost, with a single relay hop
// over the loopback interface on each machine.  The wormhole rendezvous server
// is used only for the initial code-exchange handshake (text message); it
// carries no application data.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/psanford/wormhole-william/wormhole"
)

// WormholeHost is the host-side tunnel handle.  Call Close when done.
type WormholeHost struct {
	// Code is the human-shareable wormhole code to give to the remote peer.
	Code string

	cancel context.CancelFunc
	ln     net.Listener
	wg     sync.WaitGroup
}

// Close cancels the tunnel and closes the relay listener.
func (h *WormholeHost) Close() {
	h.cancel()
	if h.ln != nil {
		h.ln.Close()
	}
	h.wg.Wait()
}

// WormholeClient is the client-side tunnel handle.  Call Close when done.
type WormholeClient struct {
	// LocalAddr is the 127.0.0.1:NNNN address the caller should dial; bytes
	// written to this address arrive at the remoteAddr of the host.
	LocalAddr string

	cancel context.CancelFunc
	ln     net.Listener
	wg     sync.WaitGroup
}

// Close cancels the tunnel and closes the local listener.
func (w *WormholeClient) Close() {
	w.cancel()
	if w.ln != nil {
		w.ln.Close()
	}
	w.wg.Wait()
}

// StartWormholeHost starts a relay listener and advertises its address through
// the wormhole rendezvous protocol.  remoteAddr is a TCP address (e.g.
// "127.0.0.1:8080") that incoming relay connections will be forwarded to.
//
// The function blocks until the wormhole code has been allocated and the
// initial handshake with the rendezvous server has completed; it then returns
// immediately so the caller can share host.Code with the remote peer.
func StartWormholeHost(ctx context.Context, remoteAddr string) (*WormholeHost, error) {
	// 1. Bind the relay listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("wormhole host: bind relay listener: %w", err)
	}

	relayAddr := ln.Addr().String()

	// 2. Send the relay address as a wormhole text message to obtain a code.
	var c wormhole.Client
	code, resultCh, err := c.SendText(ctx, relayAddr)
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("wormhole host: send text: %w", err)
	}

	hostCtx, cancel := context.WithCancel(ctx)
	h := &WormholeHost{
		Code:   code,
		cancel: cancel,
		ln:     ln,
	}

	// 3. Drain the send-result channel so the library can close the mailbox.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		select {
		case <-resultCh:
		case <-hostCtx.Done():
		}
	}()

	// 4. Accept relay connections and proxy them to remoteAddr.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		hostAcceptLoop(hostCtx, ln, remoteAddr)
	}()

	return h, nil
}

// hostAcceptLoop accepts connections on ln and proxies each to remoteAddr.
func hostAcceptLoop(ctx context.Context, ln net.Listener, remoteAddr string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// Listener was closed — normal shutdown.
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			target, err := (&net.Dialer{}).DialContext(ctx, "tcp", remoteAddr)
			if err != nil {
				return
			}
			defer target.Close()
			proxyBidirectional(c, target)
		}(conn)
	}
}

// ConnectWormholeClient claims the wormhole code, reads the relay address
// embedded in the text payload, and starts a local listener whose accepted
// connections are forwarded to that relay address.
//
// Returns a WormholeClient whose LocalAddr can be dialled by the caller.
func ConnectWormholeClient(ctx context.Context, code string) (*WormholeClient, error) {
	// 1. Receive the wormhole text message that contains the relay address.
	var c wormhole.Client
	msg, err := c.Receive(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("wormhole client: receive: %w", err)
	}
	if msg.Type != wormhole.TransferText {
		return nil, fmt.Errorf("wormhole client: expected text transfer, got %v", msg.Type)
	}
	rawAddr, err := io.ReadAll(msg)
	if err != nil {
		return nil, fmt.Errorf("wormhole client: read payload: %w", err)
	}
	relayAddr := string(rawAddr)

	// 2. Bind the local listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("wormhole client: bind local listener: %w", err)
	}

	clientCtx, cancel := context.WithCancel(ctx)
	w := &WormholeClient{
		LocalAddr: ln.Addr().String(),
		cancel:    cancel,
		ln:        ln,
	}

	// 3. Accept local connections and proxy them to the relay.
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		clientAcceptLoop(clientCtx, ln, relayAddr)
	}()

	return w, nil
}

// clientAcceptLoop accepts connections on ln and proxies each to relayAddr.
func clientAcceptLoop(ctx context.Context, ln net.Listener, relayAddr string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			relay, err := (&net.Dialer{}).DialContext(ctx, "tcp", relayAddr)
			if err != nil {
				return
			}
			defer relay.Close()
			proxyBidirectional(c, relay)
		}(conn)
	}
}

// halfCloser is implemented by *net.TCPConn and allows a graceful half-close
// after one direction of the copy finishes.
type halfCloser interface {
	CloseWrite() error
}

// proxyBidirectional copies bytes between a and b until either side closes.
// All connections in this shim are local TCP connections so they satisfy
// halfCloser; the type assertion is safe.  If a connection somehow doesn't
// implement CloseWrite the deferred Close() call above still tears it down.
func proxyBidirectional(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(b, a) //nolint:errcheck
		if hc, ok := b.(halfCloser); ok {
			hc.CloseWrite() //nolint:errcheck
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(a, b) //nolint:errcheck
		if hc, ok := a.(halfCloser); ok {
			hc.CloseWrite() //nolint:errcheck
		}
	}()
	wg.Wait()
}
