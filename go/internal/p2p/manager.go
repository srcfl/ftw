package p2p

// manager.go is the production Pi side of the browser P2P path. The browser is
// always the offerer + DataChannel creator; the Pi answers. Manager.Answer
// turns a browser SDP offer into the Pi's answer by standing up a
// PeerConnection whose inbound DataChannel is served by a Bridge over the local
// API mux, then it tracks the connection so dead peers are reaped. Signaling
// (the offer/answer exchange) rides the existing authenticated owner tunnel via
// POST /api/p2p/offer — no relay changes; see
// docs/superpowers/specs/2026-06-03-home-route-phase5-transport-design.md.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
	"github.com/pion/webrtc/v4"
)

// FingerprintSigner signs a canonical string with the Pi's ES256 identity key
// (nova.Identity satisfies it via SignRawHex). Injected as an interface so the
// p2p package needn't import nova.
type FingerprintSigner interface {
	SignRawHex(msg string) (string, error)
}

const (
	// defaultMaxOpen caps simultaneous PeerConnections so a misbehaving or
	// hostile authenticated client can't exhaust the Pi with half-open peers.
	defaultMaxOpen = 16
	// handshakeTimeout bounds how long Answer waits for ICE gathering before
	// giving up — the browser falls back to the relay if we don't answer.
	handshakeTimeout = 12 * time.Second
	// sessionMaxAge is a backstop GC for connections whose state never
	// transitioned to closed/failed (e.g. a browser tab killed mid-session).
	sessionMaxAge = 6 * time.Hour
)

// Manager owns the Pi side of the browser P2P path and the lifecycle of the
// PeerConnections it answers. It is safe for concurrent use.
type Manager struct {
	log  *slog.Logger
	stun []string

	mu       sync.Mutex
	local    http.Handler          // ungated API mux; set via SetLocalAPI
	sessions map[string]*pcSession // active connections by session id
	maxOpen  int
	siteID   string                // for the fingerprint signing string
	signer   FingerprintSigner     // signs the answer DTLS fingerprint; set via SetSigner
}

type pcSession struct {
	pc      *webrtc.PeerConnection
	created time.Time
}

// NewManager builds a Manager. Pass DefaultSTUNServers for real NAT traversal,
// or nil for host-candidate-only (e.g. an in-process / LAN-only test).
func NewManager(log *slog.Logger, stun []string) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		log:      log,
		stun:     stun,
		sessions: make(map[string]*pcSession),
		maxOpen:  defaultMaxOpen,
	}
}

// SetLocalAPI injects the handler that DataChannel-delivered requests replay
// against — the ungated api.Server mux. Until it is set, Answer rejects offers.
// The DataChannel is post-auth (the offer that created it passed the owner
// gate), so replaying against the ungated mux is the authentication boundary.
func (m *Manager) SetLocalAPI(h http.Handler) {
	m.mu.Lock()
	m.local = h
	m.mu.Unlock()
}

// SetSigner wires the identity that signs the DTLS fingerprint of every answer.
// The browser, having pinned this Pi's public key at first connect, verifies the
// signature — so a key-less relay that swaps the relayed SDP/fingerprint cannot
// produce a valid signature and the browser aborts. siteID binds the signature to
// this site (it is part of the signed string).
func (m *Manager) SetSigner(siteID string, signer FingerprintSigner) {
	m.mu.Lock()
	m.siteID = siteID
	m.signer = signer
	m.mu.Unlock()
}

// SignFingerprint extracts the SHA-256 DTLS fingerprint from an answer SDP and
// returns a detached ES256 signature over tunnel.DtlsFingerprintSigningString,
// plus the millisecond timestamp it bound. Returns ("", 0) when no signer is set
// (e.g. unit tests) or the SDP carries no fingerprint — the caller then omits the
// signature and a verifying browser treats the answer as unauthenticated.
func (m *Manager) SignFingerprint(answerSDP string) (sig string, tsMs int64) {
	m.mu.Lock()
	signer, siteID := m.signer, m.siteID
	m.mu.Unlock()
	if signer == nil {
		return "", 0
	}
	fp, ok := extractSha256Fingerprint(answerSDP)
	if !ok {
		return "", 0
	}
	tsMs = time.Now().UnixMilli()
	s, err := signer.SignRawHex(tunnel.DtlsFingerprintSigningString(siteID, tunnel.NormalizeDtlsFingerprint(fp), tsMs))
	if err != nil {
		m.log.Warn("p2p: sign fingerprint", "err", err)
		return "", 0
	}
	return s, tsMs
}

// extractSha256Fingerprint pulls the hex token from the first
// "a=fingerprint:sha-256 <token>" line of an SDP. All m-lines share one cert, so
// the first is authoritative.
func extractSha256Fingerprint(sdp string) (string, bool) {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(line)
		const pfx = "a=fingerprint:sha-256 "
		if strings.HasPrefix(line, pfx) {
			return strings.TrimSpace(line[len(pfx):]), true
		}
	}
	return "", false
}

// ActiveCount reports the number of tracked PeerConnections.
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Answer turns a browser SDP offer into the Pi's SDP answer. It creates a
// PeerConnection (STUN-configured), attaches a Bridge to the inbound "ftw"
// DataChannel, performs non-trickle ICE (gather, then return the answer with
// candidates embedded), and tracks the connection for lifecycle cleanup. The
// caller must have already authenticated the requester (the offer endpoint is
// owner-gated).
// replayHeaders are the trusted offer-time auth context (forwarded Cookie +
// X-FTW-Tunnel when the offer was remote). The Bridge stamps them on every
// DataChannel request so a replayed request carries the SAME trust tier the
// owner had at offer time — remote-owner over the relay, local-console only on
// the LAN. Without this, ungated replays would be silently treated as a
// physically-present console and could mint enroll PINs etc.
func (m *Manager) Answer(ctx context.Context, offerSDP string, replayHeaders http.Header) (string, error) {
	m.mu.Lock()
	local := m.local
	m.mu.Unlock()
	if local == nil {
		return "", fmt.Errorf("p2p: local API not wired")
	}

	pc, err := NewPeer(m.stun)
	if err != nil {
		return "", fmt.Errorf("p2p: new peer: %w", err)
	}
	id := newSessionID()

	// Reserve the slot atomically with the cap check: reap dead peers, then
	// insert the live pc under the lock BEFORE the (up to handshakeTimeout) ICE
	// gather, so concurrent Answers can't over-admit past maxOpen during the
	// half-open window. Every error path below removes it (which closes the pc).
	m.reap()
	m.mu.Lock()
	if len(m.sessions) >= m.maxOpen {
		m.mu.Unlock()
		_ = pc.Close()
		return "", fmt.Errorf("p2p: too many active sessions (%d)", m.maxOpen)
	}
	m.sessions[id] = &pcSession{pc: pc, created: time.Now()}
	m.mu.Unlock()

	// The browser creates the channel; the Pi serves whatever it is handed,
	// stamping the trusted auth context on each replayed request.
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		m.log.Info("p2p: data channel open", "session", id, "label", dc.Label())
		NewBridge(dc, local, replayHeaders, m.log)
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		m.log.Debug("p2p: connection state", "session", id, "state", s.String())
		// Reap only on terminal states. Disconnected often recovers (ICE
		// restart); pion advances a truly-dead peer to Failed, caught here.
		switch s {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			m.remove(id)
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offerSDP,
	}); err != nil {
		m.remove(id)
		return "", fmt.Errorf("p2p: set remote description: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		m.remove(id)
		return "", fmt.Errorf("p2p: create answer: %w", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		m.remove(id)
		return "", fmt.Errorf("p2p: set local description: %w", err)
	}
	// Non-trickle: wait for gathering so the answer SDP carries ICE candidates.
	select {
	case <-gather:
	case <-ctx.Done():
		m.remove(id)
		return "", ctx.Err()
	case <-time.After(handshakeTimeout):
		m.remove(id)
		return "", fmt.Errorf("p2p: ICE gather timeout")
	}

	final := pc.LocalDescription()
	if final == nil {
		m.remove(id)
		return "", fmt.Errorf("p2p: no local description after gather")
	}
	return final.SDP, nil
}

// remove closes and forgets one session.
func (m *Manager) remove(id string) {
	m.mu.Lock()
	sess := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if sess != nil {
		_ = sess.pc.Close()
	}
}

// reap closes connections that are no longer live or have aged out.
func (m *Manager) reap() {
	m.mu.Lock()
	var dead []string
	for id, s := range m.sessions {
		st := s.pc.ConnectionState()
		if st == webrtc.PeerConnectionStateClosed ||
			st == webrtc.PeerConnectionStateFailed ||
			time.Since(s.created) > sessionMaxAge {
			dead = append(dead, id)
		}
	}
	m.mu.Unlock()
	for _, id := range dead {
		m.remove(id)
	}
}

// Close tears down all active PeerConnections (call on shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = make(map[string]*pcSession)
	m.mu.Unlock()
	for _, s := range sessions {
		_ = s.pc.Close()
	}
}

func newSessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
