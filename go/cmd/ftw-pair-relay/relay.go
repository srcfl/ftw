// Package main — relay.go
//
// Token match table and per-connection handling for the ftw-pair relay server.
//
// Each incoming connection:
//  1. Sends the relay handshake (version + token).
//  2. The relay looks up the token in the pending table.
//  3. First peer with a given token: stored in pending, ack 0x01 sent, goroutine
//     holds the connection until the matching peer arrives (or idle timeout fires).
//  4. Second peer with same token: both conns popped from pending, acked 0x00,
//     and spliced with io.Copy in both directions.
//
// Rate limiting: max 10 new connection attempts per minute per source IP.
// Idle timeout: 60 s for unmatched connections; no limit once matched.
//
// The relay is byte-transparent — it never inspects or modifies the payload.
// AEAD encryption is handled end-to-end by the ftw-pair / ftw-connect clients.
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	// relayProtoVersion is the expected first byte of the handshake.
	relayProtoVersion = 0x01

	// idleTimeout is the maximum time an unmatched connection is held.
	idleTimeout = 60 * time.Second

	// maxTokenBytes is the maximum token length we accept.
	maxTokenBytes = 255

	// rateLimitWindow is the sliding window for per-IP rate limiting.
	rateLimitWindow = time.Minute

	// rateLimitMax is the maximum new connections per IP per window.
	rateLimitMax = 10
)

// Relay is the token match table. Safe for concurrent use.
type Relay struct {
	mu      sync.Mutex
	pending map[string]*pendingConn // token → first peer waiting

	rateMu  sync.Mutex
	ipHits  map[string][]time.Time // IP → connection timestamps
}

type pendingConn struct {
	conn      net.Conn
	token     string
	arrivedAt time.Time
	// matched is closed when the second peer arrives (or timeout fires).
	matched chan struct{}
}

// NewRelay creates an empty relay.
func NewRelay() *Relay {
	r := &Relay{
		pending: make(map[string]*pendingConn),
		ipHits:  make(map[string][]time.Time),
	}
	go r.idleReaper()
	return r
}

// Handle handles a new inbound connection through the relay protocol.
func (r *Relay) Handle(conn net.Conn) {
	addr := conn.RemoteAddr().String()
	ip, _, _ := net.SplitHostPort(addr)

	if !r.allowIP(ip) {
		slog.Warn("relay: rate-limited connection dropped", "ip", ip)
		conn.Write([]byte{0x02}) //nolint:errcheck
		conn.Close()
		return
	}

	// Read handshake: version(1) + tokenLen(1) + token(N).
	conn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		slog.Debug("relay: handshake read error", "ip", ip, "err", err)
		conn.Close()
		return
	}
	if header[0] != relayProtoVersion {
		slog.Debug("relay: unknown protocol version", "ip", ip, "version", header[0])
		conn.Write([]byte{0x02}) //nolint:errcheck
		conn.Close()
		return
	}
	tokenLen := int(header[1])
	if tokenLen == 0 || tokenLen > maxTokenBytes {
		slog.Debug("relay: invalid token length", "ip", ip, "len", tokenLen)
		conn.Write([]byte{0x02}) //nolint:errcheck
		conn.Close()
		return
	}
	tokenBuf := make([]byte, tokenLen)
	if _, err := io.ReadFull(conn, tokenBuf); err != nil {
		slog.Debug("relay: token read error", "ip", ip, "err", err)
		conn.Close()
		return
	}
	conn.SetDeadline(time.Time{}) //nolint:errcheck
	token := string(tokenBuf)

	slog.Debug("relay: connection", "ip", ip, "token_prefix", tokenPrefix(token))

	r.mu.Lock()
	pc, ok := r.pending[token]
	if ok {
		// Second peer: pop the pending entry and match.
		delete(r.pending, token)
	} else {
		// First peer: register as pending.
		pc = &pendingConn{
			conn:      conn,
			token:     token,
			arrivedAt: time.Now(),
			matched:   make(chan struct{}),
		}
		r.pending[token] = pc
	}
	r.mu.Unlock()

	if !ok {
		// This is the first peer. Ack "waiting" and hold until matched or timeout.
		if _, err := conn.Write([]byte{0x01}); err != nil {
			slog.Debug("relay: first-peer ack write error", "ip", ip, "err", err)
			conn.Close()
			r.mu.Lock()
			delete(r.pending, token)
			r.mu.Unlock()
			return
		}
		// Block until matched (or idle timeout kicks us out via idleReaper).
		select {
		case <-pc.matched:
			// Matched by second peer; the second-peer goroutine handles piping.
		case <-time.After(idleTimeout + 5*time.Second):
			// Should have been reaped by idleReaper before this fires; defensive.
			conn.Close()
		}
		return
	}

	// This is the second peer. Ack both peers and splice.
	peerConn := pc.conn

	// Ack the second peer (us).
	if _, err := conn.Write([]byte{0x00}); err != nil {
		slog.Debug("relay: second-peer ack write error", "ip", ip, "err", err)
		conn.Close()
		close(pc.matched)
		peerConn.Close()
		return
	}
	// Ack the first peer.
	if _, err := peerConn.Write([]byte{0x00}); err != nil {
		slog.Debug("relay: first-peer matched-ack write error", "ip", ip, "err", err)
		conn.Close()
		close(pc.matched)
		peerConn.Close()
		return
	}
	// Signal the first-peer goroutine that it was matched (so it can exit cleanly).
	close(pc.matched)

	slog.Info("relay: matched pair", "token_prefix", tokenPrefix(token))
	splice(peerConn, conn)
	slog.Info("relay: pair disconnected", "token_prefix", tokenPrefix(token))
}

// splice copies bidirectionally between a and b until both sides close.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(a, b) //nolint:errcheck
		a.Close()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(b, a) //nolint:errcheck
		b.Close()
		done <- struct{}{}
	}()
	<-done
	<-done
}

// idleReaper runs periodically and closes/removes unmatched connections that
// have been idle for longer than idleTimeout.
func (r *Relay) idleReaper() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		r.mu.Lock()
		now := time.Now()
		for token, pc := range r.pending {
			if now.Sub(pc.arrivedAt) > idleTimeout {
				delete(r.pending, token)
				pc.conn.Close()
				slog.Debug("relay: idle timeout — closed unmatched connection", "token_prefix", tokenPrefix(token))
			}
		}
		r.mu.Unlock()
	}
}

// allowIP returns true if the source IP is within rate limits.
func (r *Relay) allowIP(ip string) bool {
	r.rateMu.Lock()
	defer r.rateMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rateLimitWindow)

	hits := r.ipHits[ip]
	// Filter out timestamps older than the window.
	recent := hits[:0]
	for _, t := range hits {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	recent = append(recent, now)
	r.ipHits[ip] = recent

	return len(recent) <= rateLimitMax
}

// tokenPrefix returns the first word of a token (for log scrubbing).
func tokenPrefix(token string) string {
	for i, c := range token {
		if c == '-' {
			return token[:i] + "-…"
		}
	}
	return fmt.Sprintf("%.8s…", token)
}
