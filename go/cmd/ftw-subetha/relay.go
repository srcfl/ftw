// Package main — relay server for ftw-pair.
//
// Protocol (v0x02):
//
//  1. Each peer connects via TCP and sends the handshake:
//
//        [1 byte] version = 0x02
//        [1 byte] role    = 0x00 (host) | 0x01 (client)
//        [1 byte] tokenLen N
//        [N bytes] token (UTF-8)
//
//  2. Hosts (role 0x00) are queued in pendingHosts[token]. The relay acks
//     them with 0x01 ("waiting") and blocks until a client pops them or
//     the idle timeout fires.
//
//  3. Clients (role 0x01) pop the head of pendingHosts[token] and the relay
//     acks both peers with 0x00 ("matched"). If no host is queued the
//     client is acked with 0x04 ("no host waiting") and disconnected — the
//     client is expected to retry. The host pool on the sidecar side keeps
//     several hosts pre-warmed so clients usually pop immediately.
//
//  4. After matching, the relay splices the two connections with bidirectional
//     io.Copy. Either side closing tears down the pair.
//
//  Role-tagging fixes a bug in v0x01 where two hosts spawned by the worker
//  pool would match with each other, leaving real clients with no peer.
//
// The relay is byte-transparent — it never inspects or modifies the payload.
// AEAD encryption is handled end-to-end by the ftw-pair / ftw-connect clients.
package main

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	// relayProtoVersion is the expected first byte of the handshake.
	relayProtoVersion = 0x02

	// Roles.
	roleHost   byte = 0x00
	roleClient byte = 0x01

	// Ack codes.
	ackMatched      byte = 0x00
	ackWaiting      byte = 0x01
	ackError        byte = 0x02
	ackNoHostReady  byte = 0x04

	// idleTimeout is the maximum time an unmatched host is held.
	idleTimeout = 60 * time.Second

	// maxTokenBytes is the maximum token length we accept.
	maxTokenBytes = 255

	// rateLimitWindow is the sliding window for per-IP rate limiting.
	rateLimitWindow = time.Minute

	// rateLimitMax is the maximum new connections per IP per window.
	rateLimitMax = 60
)

// Relay is the token match table. Safe for concurrent use.
type Relay struct {
	mu           sync.Mutex
	pendingHosts map[string][]*pendingConn // token → FIFO queue of host conns

	rateMu sync.Mutex
	ipHits map[string][]time.Time // IP → connection timestamps
}

type pendingConn struct {
	conn      net.Conn
	token     string
	arrivedAt time.Time
	// matched is closed when a client arrives (or timeout fires).
	matched chan struct{}
}

// NewRelay returns a Relay with empty state. Call Serve(ln) to start.
func NewRelay() *Relay {
	return &Relay{
		pendingHosts: make(map[string][]*pendingConn),
		ipHits:       make(map[string][]time.Time),
	}
}

// StartReaper kicks off the idle-host reaper goroutine. Call once at startup.
func (r *Relay) StartReaper() {
	go r.idleReaper()
}

// Handle reads the protocol handshake from conn and either queues the peer
// (host) or matches it with a queued host (client). Spawn as a goroutine.
func (r *Relay) Handle(conn net.Conn) {
	ip := conn.RemoteAddr().String()
	if h, _, err := net.SplitHostPort(ip); err == nil {
		ip = h
	}

	if !r.allow(ip) {
		slog.Warn("relay: rate-limit", "ip", ip)
		conn.Write([]byte{ackError}) //nolint:errcheck
		conn.Close()
		return
	}

	// Read handshake: version(1) + role(1) + tokenLen(1) + token(N).
	conn.SetDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck
	header := make([]byte, 3)
	if _, err := io.ReadFull(conn, header); err != nil {
		slog.Debug("relay: handshake read error", "ip", ip, "err", err)
		conn.Close()
		return
	}
	if header[0] != relayProtoVersion {
		slog.Debug("relay: unknown protocol version", "ip", ip, "version", header[0])
		conn.Write([]byte{ackError}) //nolint:errcheck
		conn.Close()
		return
	}
	role := header[1]
	if role != roleHost && role != roleClient {
		slog.Debug("relay: unknown role", "ip", ip, "role", role)
		conn.Write([]byte{ackError}) //nolint:errcheck
		conn.Close()
		return
	}
	tokenLen := int(header[2])
	if tokenLen == 0 || tokenLen > maxTokenBytes {
		slog.Debug("relay: invalid token length", "ip", ip, "len", tokenLen)
		conn.Write([]byte{ackError}) //nolint:errcheck
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

	if role == roleHost {
		r.handleHost(conn, ip, token)
	} else {
		r.handleClient(conn, ip, token)
	}
}

func (r *Relay) handleHost(conn net.Conn, ip, token string) {
	pc := &pendingConn{
		conn:      conn,
		token:     token,
		arrivedAt: time.Now(),
		matched:   make(chan struct{}),
	}
	r.mu.Lock()
	r.pendingHosts[token] = append(r.pendingHosts[token], pc)
	r.mu.Unlock()

	if _, err := conn.Write([]byte{ackWaiting}); err != nil {
		slog.Debug("relay: host waiting-ack write error", "ip", ip, "err", err)
		r.removeHost(token, pc)
		conn.Close()
		return
	}

	select {
	case <-pc.matched:
		// Matched by handleClient; splicing happens there.
	case <-time.After(idleTimeout + 5*time.Second):
		// Should have been reaped before this fires; defensive.
		r.removeHost(token, pc)
		conn.Close()
	}
}

func (r *Relay) handleClient(conn net.Conn, ip, token string) {
	r.mu.Lock()
	queue := r.pendingHosts[token]
	if len(queue) == 0 {
		r.mu.Unlock()
		slog.Debug("relay: client arrived but no host queued", "ip", ip, "token_prefix", tokenPrefix(token))
		conn.Write([]byte{ackNoHostReady}) //nolint:errcheck
		conn.Close()
		return
	}
	host := queue[0]
	r.pendingHosts[token] = queue[1:]
	if len(r.pendingHosts[token]) == 0 {
		delete(r.pendingHosts, token)
	}
	r.mu.Unlock()

	if _, err := conn.Write([]byte{ackMatched}); err != nil {
		slog.Debug("relay: client matched-ack write error", "ip", ip, "err", err)
		conn.Close()
		close(host.matched)
		host.conn.Close()
		return
	}
	if _, err := host.conn.Write([]byte{ackMatched}); err != nil {
		slog.Debug("relay: host matched-ack write error", "ip", ip, "err", err)
		conn.Close()
		close(host.matched)
		host.conn.Close()
		return
	}

	close(host.matched)
	slog.Info("relay: matched pair", "token_prefix", tokenPrefix(token))

	// Splice — bidirectional io.Copy with half-close handling.
	//
	// As soon as EITHER direction reaches EOF, close BOTH conns so the other
	// io.Copy unblocks. Without this, a short-lived client (e.g. one-shot curl)
	// that closes its side after the response would leave the splice deadlocked:
	// the client→host Copy returns on EOF, but the host→client Copy stays
	// blocked reading from a keep-alive HTTP conn on the host's MCP server,
	// holding the host's relay socket open forever. The host's worker pool then
	// fills up after a few requests and every new client gets "no host ready".
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(host.conn, conn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, host.conn)
		done <- struct{}{}
	}()

	<-done
	conn.Close()
	host.conn.Close()
	<-done // drain the second goroutine — its io.Copy returns once the close above lands

	slog.Info("relay: pair disconnected", "token_prefix", tokenPrefix(token))
}

// removeHost drops a specific pending host from the queue (used on error or
// reap). Safe if the host is already gone.
func (r *Relay) removeHost(token string, pc *pendingConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.pendingHosts[token]
	for i, p := range q {
		if p == pc {
			r.pendingHosts[token] = append(q[:i], q[i+1:]...)
			if len(r.pendingHosts[token]) == 0 {
				delete(r.pendingHosts, token)
			}
			return
		}
	}
}

// idleReaper runs periodically and closes hosts that have been queued longer
// than idleTimeout.
func (r *Relay) idleReaper() {
	t := time.NewTicker(idleTimeout / 2)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		r.mu.Lock()
		for token, queue := range r.pendingHosts {
			fresh := queue[:0]
			for _, pc := range queue {
				if now.Sub(pc.arrivedAt) > idleTimeout {
					slog.Debug("relay: idle reap", "token_prefix", tokenPrefix(token))
					pc.conn.Close()
					continue
				}
				fresh = append(fresh, pc)
			}
			if len(fresh) == 0 {
				delete(r.pendingHosts, token)
			} else {
				r.pendingHosts[token] = fresh
			}
		}
		r.mu.Unlock()
	}
}

// allow returns true if `ip` is below the rate-limit threshold.
func (r *Relay) allow(ip string) bool {
	now := time.Now()
	cutoff := now.Add(-rateLimitWindow)
	r.rateMu.Lock()
	defer r.rateMu.Unlock()
	hits := r.ipHits[ip]
	fresh := hits[:0]
	for _, t := range hits {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= rateLimitMax {
		r.ipHits[ip] = fresh
		return false
	}
	fresh = append(fresh, now)
	r.ipHits[ip] = fresh
	return true
}

func tokenPrefix(t string) string {
	if i := indexByte(t, '-'); i > 0 {
		return t[:i] + "-…"
	}
	if len(t) > 6 {
		return t[:6] + "…"
	}
	return t
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
