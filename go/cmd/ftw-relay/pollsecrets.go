package main

import (
	"crypto/subtle"
	"errors"
	"sync"
	"time"
)

// PollSecrets maps a registered host_id to the poll token it must present on
// /tunnel/{host_id}/next and /tunnel/{host_id}/response/{req_id}. The token is
// minted on registration (returned to the host, which proved it owns the
// host_id by an ES256-signed /me/register or by registering its own pair token)
// and required on every poll, so a caller that merely learned the 48-bit
// host_id can't race the real host for tunneled traffic.
//
// Each host_id's secret is BOUND to the principal that first minted it (the
// verified site key for owners via /me/register, the pair token for friends via
// /tunnel/register). A later Issue with a DIFFERENT principal is refused — so a
// caller that did not mint a host_id's secret can never retrieve it. This closes
// the disclosure where an unauthenticated /tunnel/register with an existing
// owner-* host_id would otherwise be handed back the owner's poll secret and
// then poll/inject the owner's /signal rendezvous.
//
// In-memory and ephemeral. Tokens are GC'd after they go unused (the host
// re-registers / re-polls periodically, refreshing the seen timestamp); a relay
// restart drops them and hosts re-register to get fresh ones.
type PollSecrets struct {
	mu        sync.Mutex
	secret    map[string]string
	principal map[string]string // host_id → principal that minted the secret
	seen      map[string]time.Time
}

// ErrPrincipalMismatch is returned by Issue when an existing secret for a
// host_id was minted by a DIFFERENT principal — the caller is not the owner of
// this host_id's secret and must not be handed it.
var ErrPrincipalMismatch = errors.New("poll secret bound to a different principal")

func NewPollSecrets() *PollSecrets {
	return &PollSecrets{
		secret:    make(map[string]string),
		principal: make(map[string]string),
		seen:      make(map[string]time.Time),
	}
}

// Issue returns the stable poll token for hostID, minting one on first use (so
// re-registration returns the same token) and refreshing its seen timestamp.
// The secret is bound to principal on first mint; a later Issue with a different
// principal is refused with ErrPrincipalMismatch (the caller did not mint this
// secret and must never receive it). Pass a stable, caller-specific principal:
// the verified site public key for owners (/me/register), the registered pair
// token for friends (/tunnel/register). An empty principal is rejected so a
// caller can't bypass the binding.
func (p *PollSecrets) Issue(hostID, principal string) (string, error) {
	if principal == "" {
		return "", ErrPrincipalMismatch
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.secret[hostID]
	if !ok {
		s = mintGrant() // 32-byte CSPRNG base64url
		p.secret[hostID] = s
		p.principal[hostID] = principal
	} else if subtle.ConstantTimeCompare([]byte(p.principal[hostID]), []byte(principal)) != 1 {
		// A secret already exists, minted by a different principal. Refuse — the
		// caller must not learn a secret it did not mint. (Constant-time so the
		// refusal doesn't leak the stored principal byte-by-byte.)
		return "", ErrPrincipalMismatch
	}
	p.seen[hostID] = time.Now()
	return s, nil
}

// Check reports whether token matches hostID's minted token (constant-time),
// refreshing the seen timestamp on success. Unknown host / empty token → false.
func (p *PollSecrets) Check(hostID, token string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	want, ok := p.secret[hostID]
	if !ok || want == "" || token == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1 {
		p.seen[hostID] = time.Now()
		return true
	}
	return false
}

// GC drops tokens unused for longer than maxAge. Returns how many were removed.
func (p *PollSecrets) GC(maxAge time.Duration) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	removed := 0
	for h, t := range p.seen {
		if now.Sub(t) > maxAge {
			delete(p.secret, h)
			delete(p.principal, h)
			delete(p.seen, h)
			removed++
		}
	}
	return removed
}
