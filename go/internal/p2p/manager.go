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
	// defaultMaxUnauth is a SEPARATE, lower cap on peers that have not yet
	// captured a login-over-channel session (FIX-4b). The signaling rendezvous is
	// unauthenticated, so without this an offer flood — each draining a slot
	// before any auth — fills maxOpen and denies the owner. Authenticated peers
	// are counted against maxOpen, not this; an attacker who never logs in can
	// pin at most defaultMaxUnauth slots, and the unauth reaps free those fast.
	//
	// FIX-C: bumped from 6 to 12 (still < maxOpen) and PAIRED with a much faster
	// unauth reap (unauthConnectGrace / unauthReapAfter below). The owner's FIRST
	// login is itself unauthenticated until the WebAuthn session is captured, so a
	// transient attacker flood must not be able to fill every unauth slot for long.
	// A wider unauth cap leaves headroom for the real owner to land while the fast
	// reap continuously frees attacker slots; the relay per-IP limit (FIX-C) is the
	// primary bound on flood volume, so this only needs to survive a brief burst.
	//
	// RESIDUAL: a DISTRIBUTED flood (many source IPs, each within the relay per-IP
	// limit) is a higher-bar residual — it can still momentarily occupy unauth
	// slots. The fast reap caps how long each is held; a true sustained botnet
	// flood belongs to the CDN/WAF in front of the relay, not this in-process cap.
	defaultMaxUnauth = 12
	// handshakeTimeout bounds how long Answer waits for ICE gathering before
	// giving up — the browser falls back to the relay if we don't answer.
	handshakeTimeout = 12 * time.Second
	// unauthConnectGrace is the SHORT window a brand-new un-authed peer gets to
	// reach a live DTLS connection (FIX-C). A peer still NOT in the Connected state
	// past this — an offer that drained a slot but never completed the handshake,
	// the most common cheap flood — is reaped promptly so it can't pin an unauth
	// slot. A genuine browser completes the handshake in well under a second on a
	// reachable path.
	unauthConnectGrace = 5 * time.Second
	// unauthReapAfter is the grace an un-authed but CONNECTED peer gets to log in
	// over the channel. The legitimate WebAuthn-over-DataChannel ceremony completes
	// in a few seconds; a connected peer that never captures a session past this is
	// reaped. Shortened from 30s (FIX-C) so a connect-but-never-login flood frees
	// its slots quickly without starving a slow-but-real owner login.
	unauthReapAfter = 12 * time.Second
	// sessionMaxAge is a backstop GC for connections whose state never
	// transitioned to closed/failed (e.g. a browser tab killed mid-session).
	sessionMaxAge = 6 * time.Hour
)

// Manager owns the Pi side of the browser P2P path and the lifecycle of the
// PeerConnections it answers. It is safe for concurrent use.
type Manager struct {
	log  *slog.Logger
	stun []string

	mu        sync.Mutex
	local     http.Handler          // ungated API mux; set via SetLocalAPI
	sessions  map[string]*pcSession // active connections by session id
	maxOpen   int
	maxUnauth int               // separate cap on not-yet-authenticated peers (FIX-4b)
	siteID    string            // for the fingerprint signing string
	signer    FingerprintSigner // signs the answer DTLS fingerprint; set via SetSigner
}

type pcSession struct {
	pc      *webrtc.PeerConnection
	created time.Time
	// authed flips true once the browser captures a login-over-channel session on
	// this peer's Bridge (FIX-4b). An un-authed peer is subject to the lower
	// maxUnauth cap and the unauthReapAfter grace; once authed it is a real owner
	// session counted only against maxOpen.
	authed bool
}

// NewManager builds a Manager. Pass DefaultSTUNServers for real NAT traversal,
// or nil for host-candidate-only (e.g. an in-process / LAN-only test).
func NewManager(log *slog.Logger, stun []string) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		log:       log,
		stun:      stun,
		sessions:  make(map[string]*pcSession),
		maxOpen:   defaultMaxOpen,
		maxUnauth: defaultMaxUnauth,
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

	// Reserve the slot atomically with the cap check: reap dead/stale peers, then
	// insert the live pc under the lock BEFORE the (up to handshakeTimeout) ICE
	// gather, so concurrent Answers can't over-admit during the half-open window.
	// Every error path below removes it (which closes the pc).
	//
	// FIX-4b: a NEW peer starts un-authenticated, so it is admitted against the
	// LOWER maxUnauth cap (separate from maxOpen). The signaling rendezvous is
	// unauthenticated; without this a flood of offers — each draining a slot
	// before any login — fills maxOpen and denies the owner. An attacker who never
	// logs in can hold at most maxUnauth slots, and the reaper frees those after
	// unauthReapAfter. Authenticated peers (session captured) are counted only
	// against maxOpen, so a logged-in owner is never blocked by the unauth flood.
	m.reap()
	m.mu.Lock()
	if len(m.sessions) >= m.maxOpen {
		m.mu.Unlock()
		_ = pc.Close()
		return "", fmt.Errorf("p2p: too many active sessions (%d)", m.maxOpen)
	}
	if m.unauthCountLocked() >= m.maxUnauth {
		m.mu.Unlock()
		_ = pc.Close()
		return "", fmt.Errorf("p2p: too many un-authenticated sessions (%d)", m.maxUnauth)
	}
	m.sessions[id] = &pcSession{pc: pc, created: time.Now()}
	m.mu.Unlock()

	// Schedule the deterministic unauth reaps so slots are freed even if no further
	// offers arrive to drive reap() (FIX-4b + FIX-C):
	//   - a FAST pre-connect reap (unauthConnectGrace): an offer that drained a slot
	//     but never reached a live DTLS connection is closed in seconds.
	//   - the login-window reap (unauthReapAfter): a connected-but-never-logged-in
	//     peer is closed after the short ceremony window.
	time.AfterFunc(unauthConnectGrace, func() { m.reapIfUnconnected(id) })
	time.AfterFunc(unauthReapAfter, func() { m.reapIfUnauthed(id) })

	// The browser creates the channel; the Pi serves whatever it is handed,
	// stamping the trusted auth context on each replayed request. When the browser
	// logs in over THIS channel, the Bridge captures the session and fires
	// onSession, which marks the peer authenticated so it is no longer subject to
	// the unauth cap / grace reaping (FIX-4b).
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		m.log.Info("p2p: data channel open", "session", id, "label", dc.Label())
		br := NewBridge(dc, local, replayHeaders, m.log)
		br.SetOnSession(func() { m.markAuthed(id) })
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

// unauthCountLocked returns how many tracked peers have NOT yet captured a
// session. Caller holds m.mu.
func (m *Manager) unauthCountLocked() int {
	n := 0
	for _, s := range m.sessions {
		if !s.authed {
			n++
		}
	}
	return n
}

// markAuthed flips a peer to authenticated once the browser captures a
// login-over-channel session on its Bridge (FIX-4b), so it is no longer subject
// to the unauth cap / grace reaping.
func (m *Manager) markAuthed(id string) {
	m.mu.Lock()
	if s := m.sessions[id]; s != nil {
		s.authed = true
	}
	m.mu.Unlock()
}

// reapIfUnauthed closes a session iff it is still un-authenticated when its grace
// timer fires (FIX-4b). An authed session (logged in over the channel) is kept.
//
// FIX-D (TOCTOU): the authed check and the removal happen under the SAME lock
// hold. The earlier version checked !authed, dropped the lock, then removed in a
// second lock hold — markAuthed could win the gap and a peer that authenticated a
// microsecond before its grace timer fired would still be closed. Deciding and
// deleting atomically closes that window; only pc.Close() (which must not run
// under the lock) is deferred to after the unlock.
func (m *Manager) reapIfUnauthed(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil || s.authed {
		// Already gone, or it authenticated before the timer fired (possibly in the
		// race window) — keep it. Re-checked under the lock that also deletes, so
		// markAuthed can't slip in between the decision and the removal.
		m.mu.Unlock()
		return
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	m.log.Debug("p2p: reaping un-authenticated peer past grace", "session", id)
	_ = s.pc.Close()
}

// reapIfUnconnected closes a session iff, when its short connect-grace timer
// fires, it is still un-authenticated AND has not reached a live DTLS connection
// (FIX-C). This is the cheap-flood case: an offer that drained an unauth slot but
// never completed the handshake. An already-connected (still logging in) or authed
// peer is kept — the login-window reaper handles those. Same same-lock discipline
// as FIX-D: the decision and the delete happen under one lock hold so a peer that
// connects/auths in the gap is never wrongly closed.
func (m *Manager) reapIfUnconnected(id string) {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil || s.authed {
		m.mu.Unlock()
		return
	}
	st := s.pc.ConnectionState()
	if st == webrtc.PeerConnectionStateConnected {
		// Connected and logging in — the login-window reaper owns it from here.
		m.mu.Unlock()
		return
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	m.log.Debug("p2p: reaping un-connected peer past connect grace", "session", id, "state", st.String())
	_ = s.pc.Close()
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

// reap closes connections that are no longer live, have aged out, or are
// un-authenticated past the short grace window (FIX-4b). The unauth-grace reap is
// what frees slots an offer flood pinned without ever logging in: a peer that
// connected but never captured a session — or never finished the handshake — is
// closed after unauthReapAfter. A logged-in (authed) peer is exempt from the
// grace reap and only ages out at sessionMaxAge.
func (m *Manager) reap() {
	now := time.Now()
	m.mu.Lock()
	// Decide AND delete under the same lock so markAuthed can't race the unauth
	// reap (FIX-D): a peer that authenticates between "flagged dead" and "removed"
	// must survive. Collect the closed PeerConnections and Close() them after the
	// unlock (pc.Close blocks; never hold m.mu across it).
	var closing []*webrtc.PeerConnection
	for id, s := range m.sessions {
		st := s.pc.ConnectionState()
		switch {
		case st == webrtc.PeerConnectionStateClosed,
			st == webrtc.PeerConnectionStateFailed,
			now.Sub(s.created) > sessionMaxAge:
			// Terminal / aged-out — reap regardless of auth state.
			delete(m.sessions, id)
			closing = append(closing, s.pc)
		case !s.authed && st != webrtc.PeerConnectionStateConnected &&
			now.Sub(s.created) > unauthConnectGrace:
			// FIX-C fast pre-connect reap: an un-authed peer that never reached a live
			// DTLS connection past the short connect grace — the cheap-flood case (an
			// offer drained a slot but the handshake never completed). Free it fast so
			// it can't pin an unauth slot.
			delete(m.sessions, id)
			closing = append(closing, s.pc)
		case !s.authed && now.Sub(s.created) > unauthReapAfter:
			// Connected-but-never-authenticated past the login grace — reap so a
			// connect-but-never-login flood can't hold slots. The !authed read and the
			// delete are in the same lock hold, so a concurrent markAuthed either ran
			// before (s.authed true → kept) or runs after (no-op on a removed
			// session); a just-authed peer is never closed (FIX-D).
			delete(m.sessions, id)
			closing = append(closing, s.pc)
		}
	}
	m.mu.Unlock()
	for _, pc := range closing {
		_ = pc.Close()
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
