package main

import (
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
	return nil
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
