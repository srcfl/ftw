package main

import (
	"crypto/subtle"
	"errors"
	"sync"
	"time"
)

// BootstrapStore holds, per site_id, the Pi's signed descriptor bytes during the
// brief first-enrollment window, keyed for claim by sha256(bootstrap_id). It is
// EPHEMERAL (in-memory, TTL'd — unlike the durable WalletBlobStore) and BLIND:
// the descriptor is Pi-signed cleartext the relay never trust-parses (site_id +
// pi_pubkey are already known to the relay from /me/register).
//
// claimKey is hex(sha256(bootstrap_id)): a 256-bit unguessable handle. The relay
// never sees the raw bootstrap_id — the browser derives claimKey from the URL
// fragment — and it never sees the 6-digit PIN, which the Pi validates on the
// forwarded enroll.
type BootstrapStore struct {
	mu       sync.Mutex
	m        map[string]*bootstrapEntry
	maxBytes int
	maxSites int
}

type bootstrapEntry struct {
	descriptor []byte
	claimKey   string
	expiresAt  time.Time
}

var (
	ErrBootstrapTooLarge = errors.New("bootstrap descriptor too large")
	ErrTooManyBootstraps = errors.New("too many bootstrap blobs")
)

func NewBootstrapStore(maxBytes, maxSites int) *BootstrapStore {
	return &BootstrapStore{m: make(map[string]*bootstrapEntry), maxBytes: maxBytes, maxSites: maxSites}
}

func (s *BootstrapStore) Put(siteID string, descriptor []byte, claimKey string, ttl time.Duration) error {
	if s.maxBytes > 0 && len(descriptor) > s.maxBytes {
		return ErrBootstrapTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[siteID]; !ok && len(s.m) >= s.maxSites {
		return ErrTooManyBootstraps
	}
	s.m[siteID] = &bootstrapEntry{
		descriptor: append([]byte(nil), descriptor...),
		claimKey:   claimKey,
		expiresAt:  time.Now().Add(ttl),
	}
	return nil
}

func (s *BootstrapStore) Claim(claimKey string) (descriptor []byte, siteID string, ok bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for site, e := range s.m {
		if now.After(e.expiresAt) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(e.claimKey), []byte(claimKey)) == 1 {
			return append([]byte(nil), e.descriptor...), site, true
		}
	}
	return nil, "", false
}

func (s *BootstrapStore) Live(siteID, claimKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[siteID]
	if !ok || time.Now().After(e.expiresAt) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(e.claimKey), []byte(claimKey)) == 1
}

// Consume verifies the live (non-expired) entry for siteID has a matching
// claimKey (constant-time) then deletes-and-returns it atomically under the
// lock, so two concurrent enroll/finish forwards can never both succeed.
func (s *BootstrapStore) Consume(siteID, claimKey string) (descriptor []byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, present := s.m[siteID]
	if !present || time.Now().After(e.expiresAt) {
		return nil, false
	}
	if subtle.ConstantTimeCompare([]byte(e.claimKey), []byte(claimKey)) != 1 {
		return nil, false
	}
	delete(s.m, siteID)
	return append([]byte(nil), e.descriptor...), true
}

func (s *BootstrapStore) Burn(siteID string) {
	s.mu.Lock()
	delete(s.m, siteID)
	s.mu.Unlock()
}

func (s *BootstrapStore) GC() int {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for site, e := range s.m {
		if now.After(e.expiresAt) {
			delete(s.m, site)
			n++
		}
	}
	return n
}
