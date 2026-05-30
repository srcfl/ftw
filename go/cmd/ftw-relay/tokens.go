package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

// MaxApprovalAttempts gates the 4-digit code matching. After this many
// wrong attempts on a single token, even the correct code is rejected
// for the rest of the TTL. 10000 codes / 5 attempts = 0.05% spam
// success rate before the operator notices something is off.
const MaxApprovalAttempts = 5

var (
	ErrTokenExists     = errors.New("token already registered")
	ErrTokenNotFound   = errors.New("token not found")
	ErrTokenNotPending = errors.New("token not in pending state")
	ErrBadApprovalCode = errors.New("bad approval code")
	ErrApprovalLocked  = errors.New("approval locked after too many bad attempts")
)

type TokenState int

const (
	TokenPending TokenState = iota
	TokenActive
	TokenExpired
	TokenRevoked
)

func (s TokenState) String() string {
	switch s {
	case TokenPending:
		return "pending"
	case TokenActive:
		return "active"
	case TokenExpired:
		return "expired"
	case TokenRevoked:
		return "revoked"
	}
	return "unknown"
}

// TokenRegistration is the host-supplied data when registering a new
// pair session with the relay.
type TokenRegistration struct {
	HostID       string
	Token        string
	TTL          time.Duration
	ApprovalCode string
	Intent       string
	As           string
}

// Token is the relay-side per-session record.
type Token struct {
	mu               sync.Mutex
	hostID           string
	token            string
	approvalCode     string
	intent, as       string
	createdAt        time.Time
	expiresAt        time.Time
	state            TokenState
	approvalAttempts int
	lastActivity     time.Time
	// pendingApprovals tracks landing-page hits that haven't been
	// matched yet by an /approve POST. Used by the host dashboard's
	// "friend opened the URL, code shown" indicator.
	pendingApprovals int
	// grant is the high-entropy session secret minted when the 4-digit
	// code is accepted. It — not the URL token — is what authorizes
	// /h/<token>/{mcp,web} requests after activation, so a leaked-but-
	// already-activated URL is useless without it. Empty until approval.
	// See docs/goals/relay-subdomain-sessions.md (grant-exchange model).
	grant string
}

// State returns the current state, lazily transitioning to TokenExpired
// when the TTL has passed.
func (t *Token) State() TokenState {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state == TokenPending || t.state == TokenActive {
		if time.Now().After(t.expiresAt) {
			t.state = TokenExpired
		}
	}
	return t.state
}

func (t *Token) HostID() string       { return t.hostID }
func (t *Token) Token() string        { return t.token }
func (t *Token) ApprovalCode() string { return t.approvalCode }
func (t *Token) Intent() string       { return t.intent }
func (t *Token) As() string           { return t.as }
func (t *Token) ExpiresAt() time.Time { return t.expiresAt }

// Grant returns the session grant minted at approval (empty before).
func (t *Token) Grant() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.grant
}

// mintGrant returns 32 bytes of CSPRNG entropy as base64url — the session
// secret handed to the friend once (cookie for the browser, Bearer header
// for MCP).
func mintGrant() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// TokenRegistry is the in-memory token store for one relay process.
type TokenRegistry struct {
	mu     sync.Mutex
	tokens map[string]*Token
}

func NewTokenRegistry() *TokenRegistry {
	return &TokenRegistry{tokens: make(map[string]*Token)}
}

func (r *TokenRegistry) Register(reg TokenRegistration) (*Token, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tokens[reg.Token]; ok {
		return nil, ErrTokenExists
	}
	t := &Token{
		hostID:       reg.HostID,
		token:        reg.Token,
		approvalCode: reg.ApprovalCode,
		intent:       reg.Intent,
		as:           reg.As,
		createdAt:    time.Now(),
		expiresAt:    time.Now().Add(reg.TTL),
		state:        TokenPending,
	}
	r.tokens[reg.Token] = t
	return t, nil
}

func (r *TokenRegistry) Get(token string) (*Token, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tokens[token]
	if !ok {
		return nil, ErrTokenNotFound
	}
	return t, nil
}

// Approve flips a pending token to active when the code matches.
// Returns ErrBadApprovalCode (and increments the attempt counter) when
// it doesn't, ErrApprovalLocked once the attempt cap is hit, and
// ErrTokenNotPending when the token has already moved past pending.
func (r *TokenRegistry) Approve(token, code string) error {
	t, err := r.Get(token)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != TokenPending {
		return ErrTokenNotPending
	}
	if t.approvalAttempts >= MaxApprovalAttempts {
		return ErrApprovalLocked
	}
	if code != t.approvalCode {
		t.approvalAttempts++
		return ErrBadApprovalCode
	}
	t.state = TokenActive
	t.grant = mintGrant()
	return nil
}

// CheckGrant reports whether grant matches the session's minted grant and
// the session is currently active. Constant-time compare so a timing
// oracle can't recover the grant byte-by-byte. Empty grants never match.
func (r *TokenRegistry) CheckGrant(token, grant string) bool {
	t, err := r.Get(token)
	if err != nil {
		return false
	}
	if t.State() != TokenActive { // lazy-expires under its own lock
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.grant == "" || grant == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(t.grant), []byte(grant)) == 1
}

// Revoke unconditionally marks a token as revoked. Idempotent.
func (r *TokenRegistry) Revoke(token string) {
	t, err := r.Get(token)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = TokenRevoked
}

// TouchActivity bumps the last-activity timestamp. Called by the
// public-tunnel handlers every time a friend request lands on this
// token. The dashboard's session-info polling surfaces it as
// "friend last active N seconds ago".
func (r *TokenRegistry) TouchActivity(token string) {
	t, err := r.Get(token)
	if err != nil {
		return
	}
	t.mu.Lock()
	t.lastActivity = time.Now()
	t.mu.Unlock()
}

// MarkPendingHit increments the count of landing-page hits for this
// token. Used by the dashboard to surface "friend opened the URL"
// before they've called in with the code.
func (r *TokenRegistry) MarkPendingHit(token string) {
	t, err := r.Get(token)
	if err != nil {
		return
	}
	t.mu.Lock()
	if t.state == TokenPending {
		t.pendingApprovals++
	}
	t.mu.Unlock()
}

// SessionInfo is the snapshot the host polls via /sessions/<token>/info.
type SessionInfo struct {
	State            string `json:"state"`
	PendingApprovals int    `json:"pending_approvals"`
	LastActivityMs   int64  `json:"last_activity_ms"`
	ExpiresAtMs      int64  `json:"expires_at_ms"`
}

func (t *Token) Snapshot() SessionInfo {
	state := t.State() // takes lock internally (lazy-expires)
	t.mu.Lock()
	defer t.mu.Unlock()
	si := SessionInfo{
		State:            state.String(),
		PendingApprovals: t.pendingApprovals,
		ExpiresAtMs:      t.expiresAt.UnixMilli(),
	}
	if !t.lastActivity.IsZero() {
		si.LastActivityMs = t.lastActivity.UnixMilli()
	}
	return si
}
