// Package wormhole — relay client (replaces fowld subprocess wrapper).
//
// Pure-Go TCP relay transport. Both host and client connect to a Sourceful-
// operated relay over TCP; the relay matches them by shared token and pipes
// bytes bidirectionally. Traffic is AEAD-encrypted end-to-end using keys
// derived from the token — the relay sees only ciphertext.
//
// Protocol handshake (client → relay):
//
//	[1 byte]  version = 0x01
//	[1 byte]  token length N
//	[N bytes] token (UTF-8)
//
// Relay responses:
//
//	0x00 = matched immediately (you are the second peer; piping starts)
//	0x01 = waiting (you are the first peer; a second 0x00 follows when matched)
//	0x02 = error
//
// AEAD direction labels:
//
//	Host sends    with key from: "ftw-pair v1 host→relay"
//	Host receives with key from: "ftw-pair v1 relay→host"
//	Client sends    with: "ftw-pair v1 relay→host"
//	Client receives with: "ftw-pair v1 host→relay"
//
// Default relay address: pair-relay.fortytwowatts.com:7777
// Override: FTW_PAIR_RELAY env var or -relay-addr flag (passed by callers).
package wormhole

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultRelayAddr is the default relay server address.
	// This host is not yet deployed at time of writing; tests use an in-process relay.
	DefaultRelayAddr = "pair-relay.fortytwowatts.com:7777"

	// relayProtoVersion is the handshake version byte sent to the relay.
	relayProtoVersion = 0x01

	// tokenWordCount is the number of BIP39 words in a generated token.
	tokenWordCount = 6

	// dialTimeout is how long we wait for a TCP connection to the relay.
	dialTimeout = 15 * time.Second

	// relayWaitTimeout is how long a first peer waits for the second peer at the relay.
	relayWaitTimeout = 5 * time.Minute
)

// RelayAddr returns the relay address to use, honouring the FTW_PAIR_RELAY
// environment variable if set, then the provided override, then the default.
func RelayAddr(override string) string {
	if override != "" {
		return override
	}
	if env := os.Getenv("FTW_PAIR_RELAY"); env != "" {
		return env
	}
	return DefaultRelayAddr
}

// TokenWordCount returns the number of BIP39 words in a generated token.
// Exported for testing.
func TokenWordCount() int { return tokenWordCount }

// generateToken returns a 6-word token sampled uniformly from bip39Words.
// Each word is drawn independently with crypto/rand; the caller owns the string.
func generateToken() (string, error) {
	n := big.NewInt(int64(len(bip39Words)))
	words := make([]string, tokenWordCount)
	for i := range words {
		idx, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", fmt.Errorf("generate token word %d: %w", i, err)
		}
		words[i] = bip39Words[idx.Int64()]
	}
	return strings.Join(words, "-"), nil
}

// relayConn is the raw TCP connection after the relay handshake is complete.
// It is NOT yet AEAD-wrapped — the caller decides when to wrap.
type relayConn struct {
	conn net.Conn
}

// connectToRelay dials the relay, sends the handshake, reads the first ack, and
// returns the raw connection. If the relay says "waiting" (0x01), the caller is
// responsible for reading the second 0x00 ack before sending application data.
// Returns (conn, isWaiting, error).
func connectToRelay(ctx context.Context, relayAddr, token string) (net.Conn, bool, error) {
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", relayAddr)
	if err != nil {
		return nil, false, fmt.Errorf("dial relay %s: %w", relayAddr, err)
	}

	// Send handshake: version byte + token length byte + token bytes.
	tok := []byte(token)
	if len(tok) > 255 {
		conn.Close()
		return nil, false, fmt.Errorf("token too long (%d bytes, max 255)", len(tok))
	}
	hdr := []byte{relayProtoVersion, byte(len(tok))}
	hdr = append(hdr, tok...)
	if _, err := conn.Write(hdr); err != nil {
		conn.Close()
		return nil, false, fmt.Errorf("send relay handshake: %w", err)
	}

	// Read the first ack byte (with a short deadline — the relay should respond fast).
	conn.SetDeadline(time.Now().Add(30 * time.Second)) //nolint:errcheck
	var ack [1]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		conn.Close()
		return nil, false, fmt.Errorf("relay handshake ack: %w", err)
	}
	conn.SetDeadline(time.Time{}) //nolint:errcheck

	switch ack[0] {
	case 0x00:
		// Matched immediately.
		return conn, false, nil
	case 0x01:
		// Waiting for second peer.
		return conn, true, nil
	case 0x02:
		conn.Close()
		return nil, false, errors.New("relay returned error — token may be in use or malformed")
	default:
		conn.Close()
		return nil, false, fmt.Errorf("unexpected relay ack byte: 0x%02x", ack[0])
	}
}

// waitForPeer reads the second ack byte that the relay sends when the matching
// peer arrives. Called only for "waiting" connections.
func waitForPeer(ctx context.Context, conn net.Conn) error {
	var deadline <-chan time.Time
	t := time.NewTimer(relayWaitTimeout)
	defer t.Stop()
	deadline = t.C

	readDone := make(chan error, 1)
	go func() {
		conn.SetDeadline(time.Now().Add(relayWaitTimeout)) //nolint:errcheck
		var ack2 [1]byte
		if _, err := io.ReadFull(conn, ack2[:]); err != nil {
			readDone <- err
			return
		}
		conn.SetDeadline(time.Time{}) //nolint:errcheck
		if ack2[0] != 0x00 {
			readDone <- fmt.Errorf("relay error waiting for peer (code=%d)", ack2[0])
			return
		}
		readDone <- nil
	}()

	select {
	case err := <-readDone:
		return err
	case <-deadline:
		return errors.New("timed out waiting for peer to connect")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wrapRelayConn applies AEAD framing to a matched relay connection.
// side must be "host" or "client".
func wrapRelayConn(conn net.Conn, token, side string) (net.Conn, error) {
	var writeDir, readDir string
	switch side {
	case "host":
		writeDir = "host→relay"
		readDir = "relay→host"
	case "client":
		writeDir = "relay→host"
		readDir = "host→relay"
	default:
		return nil, fmt.Errorf("wrapRelayConn: unknown side %q", side)
	}

	wk, err := aeadKey(token, writeDir)
	if err != nil {
		return nil, err
	}
	rk, err := aeadKey(token, readDir)
	if err != nil {
		return nil, err
	}

	ap, err := newAEADPipe(conn, wk, rk)
	if err != nil {
		return nil, err
	}
	return &aeadConn{Conn: conn, pipe: ap}, nil
}

// aeadConn wraps a net.Conn replacing Read/Write with AEAD-framed versions.
type aeadConn struct {
	net.Conn
	pipe *aeadPipe
}

func (c *aeadConn) Read(b []byte) (int, error)  { return c.pipe.Read(b) }
func (c *aeadConn) Write(b []byte) (int, error) { return c.pipe.Write(b) }

// ── Host ──────────────────────────────────────────────────────────────────────

// Host is the host-side tunnel handle. Created by StartHost.
// Call Close when the session is done.
type Host struct {
	// Code is the 6-word token to share with the remote peer.
	// Format: "word1-word2-word3-word4-word5-word6"
	Code string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Close shuts down the host tunnel.
func (h *Host) Close() {
	h.cancel()
	h.wg.Wait()
}

// Option configures StartHost or Connect behaviour.
type Option func(*relayOptions)

type relayOptions struct {
	relayAddr string
	token     string // override random token generation (for tests)
}

// WithRelayAddr sets a custom relay address (overrides env var + default).
func WithRelayAddr(addr string) Option {
	return func(o *relayOptions) { o.relayAddr = addr }
}

// WithToken sets an explicit token instead of generating one randomly.
// Intended for tests only.
func WithToken(token string) Option {
	return func(o *relayOptions) { o.token = token }
}

// StartHost opens a relay connection, generates (or uses the provided) token,
// and starts piping accepted connections to remoteAddr through the relay tunnel.
//
// remoteAddr is the TCP address of the local MCP server (e.g. "127.0.0.1:9999").
// The function returns immediately after the relay TCP connection is established;
// the relay matching (waiting for the second peer) happens in the background.
func StartHost(ctx context.Context, remoteAddr string, opts ...Option) (*Host, error) {
	o := &relayOptions{}
	for _, fn := range opts {
		fn(o)
	}
	addr := RelayAddr(o.relayAddr)

	token := o.token
	if token == "" {
		var err error
		token, err = generateToken()
		if err != nil {
			return nil, fmt.Errorf("wormhole host: %w", err)
		}
	}

	cctx, cancel := context.WithCancel(ctx)

	rawConn, isWaiting, err := connectToRelay(cctx, addr, token)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("wormhole host: relay connect: %w", err)
	}

	h := &Host{Code: token, cancel: cancel}

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer rawConn.Close()

		// If the relay said "waiting", block until the peer arrives.
		if isWaiting {
			if err := waitForPeer(cctx, rawConn); err != nil {
				if cctx.Err() == nil {
					slog.Error("wormhole host: peer wait failed", "err", err)
				}
				return
			}
		}

		// Wrap the connection with AEAD framing.
		relayConn, err := wrapRelayConn(rawConn, token, "host")
		if err != nil {
			slog.Error("wormhole host: wrap relay conn", "err", err)
			return
		}

		// Dial the local MCP server with retry.
		var localConn net.Conn
		for attempt := 0; attempt < 10; attempt++ {
			select {
			case <-cctx.Done():
				return
			default:
			}
			var dialErr error
			localConn, dialErr = net.DialTimeout("tcp", remoteAddr, 2*time.Second)
			if dialErr == nil {
				break
			}
			slog.Debug("wormhole host: waiting for local MCP server", "addr", remoteAddr, "attempt", attempt+1, "err", dialErr)
			time.Sleep(300 * time.Millisecond)
		}
		if localConn == nil {
			slog.Error("wormhole host: could not connect to local MCP server", "addr", remoteAddr)
			return
		}
		defer localConn.Close()

		slog.Info("wormhole host: tunnel established", "relay", addr, "remote", remoteAddr)
		pipeConns(cctx, relayConn, localConn)
		slog.Info("wormhole host: tunnel closed")
	}()

	return h, nil
}

// ── Client ────────────────────────────────────────────────────────────────────

// Client is the client-side tunnel handle. Created by Connect.
// Call Close when the session is done.
type Client struct {
	// LocalAddr is the 127.0.0.1:NNNN address to dial for MCP access.
	// Bytes written to this address are forwarded through the relay to the host.
	LocalAddr string

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Close shuts down the client tunnel.
func (c *Client) Close() {
	c.cancel()
	c.wg.Wait()
}

// Connect joins a relay session identified by the 6-word code and opens
// a local TCP listener that forwards traffic to the host's MCP server.
//
// code is the token produced by StartHost on the host side.
func Connect(ctx context.Context, code string, opts ...Option) (*Client, error) {
	o := &relayOptions{}
	for _, fn := range opts {
		fn(o)
	}
	addr := RelayAddr(o.relayAddr)
	token := strings.TrimSpace(code)

	localPort, err := pickFreePort()
	if err != nil {
		return nil, fmt.Errorf("wormhole client: pick free port: %w", err)
	}
	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)

	ln, err := net.Listen("tcp", localAddr)
	if err != nil {
		return nil, fmt.Errorf("wormhole client: local listen %s: %w", localAddr, err)
	}

	cctx, cancel := context.WithCancel(ctx)

	cl := &Client{LocalAddr: localAddr, cancel: cancel}

	cl.wg.Add(1)
	go func() {
		defer cl.wg.Done()
		defer ln.Close()

		for {
			select {
			case <-cctx.Done():
				return
			default:
			}

			ln.(*net.TCPListener).SetDeadline(time.Now().Add(500 * time.Millisecond)) //nolint:errcheck
			localConn, err := ln.Accept()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}

			go func(lc net.Conn) {
				defer lc.Close()

				rawConn, isWaiting, err := connectToRelay(cctx, addr, token)
				if err != nil {
					slog.Error("wormhole client: relay connect", "err", err)
					return
				}

				if isWaiting {
					if err := waitForPeer(cctx, rawConn); err != nil {
						if cctx.Err() == nil {
							slog.Error("wormhole client: peer wait failed", "err", err)
						}
						rawConn.Close()
						return
					}
				}

				relayConn, err := wrapRelayConn(rawConn, token, "client")
				if err != nil {
					slog.Error("wormhole client: wrap relay conn", "err", err)
					rawConn.Close()
					return
				}
				defer relayConn.Close()

				slog.Debug("wormhole client: piping connection", "relay", addr, "local", lc.RemoteAddr())
				pipeConns(cctx, lc, relayConn)
			}(localConn)
		}
	}()

	return cl, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// pipeConns copies bidirectionally between a and b until one side closes or ctx is cancelled.
func pipeConns(ctx context.Context, a, b net.Conn) {
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(b, a) //nolint:errcheck
		b.Close()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(a, b) //nolint:errcheck
		a.Close()
		done <- struct{}{}
	}()

	select {
	case <-done:
	case <-ctx.Done():
		a.Close()
		b.Close()
	}
	<-done // wait for both goroutines
}

// pickFreePort asks the OS for a free TCP port on localhost and returns it.
// The port is not held open; callers should use it immediately.
func pickFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}
