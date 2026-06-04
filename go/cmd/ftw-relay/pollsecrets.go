package main

import (
	"crypto/subtle"
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
// In-memory and ephemeral. Tokens are GC'd after they go unused (the host
// re-registers / re-polls periodically, refreshing the seen timestamp); a relay
// restart drops them and hosts re-register to get fresh ones.
type PollSecrets struct {
	mu     sync.Mutex
	secret map[string]string
	seen   map[string]time.Time
}

func NewPollSecrets() *PollSecrets {
	return &PollSecrets{
		secret: make(map[string]string),
		seen:   make(map[string]time.Time),
	}
}

// Issue returns the stable poll token for hostID, minting one on first use (so
// re-registration returns the same token) and refreshing its seen timestamp.
func (p *PollSecrets) Issue(hostID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.secret[hostID]
	if !ok {
		s = mintGrant() // 32-byte CSPRNG base64url
		p.secret[hostID] = s
	}
	p.seen[hostID] = time.Now()
	return s
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
			delete(p.seen, h)
			removed++
		}
	}
	return removed
}
