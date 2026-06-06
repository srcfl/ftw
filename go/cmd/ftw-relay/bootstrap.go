package main

import (
	"crypto/subtle"
	"errors"
	"sync"
	"time"
)

// BootstrapStore holds, per site_id, the Pi's signed descriptor bytes during the
// brief first-enrollment window, keyed for claim by sha256(PIN). It is EPHEMERAL
// (in-memory, TTL'd — unlike the durable WalletBlobStore) and BLIND: the
// descriptor is Pi-signed cleartext the relay never trust-parses (site_id +
// pi_pubkey are already known to the relay from /me/register).
type BootstrapStore struct {
	mu       sync.Mutex
	m        map[string]*bootstrapEntry
	maxBytes int
	maxSites int
}

type bootstrapEntry struct {
	descriptor []byte
	pinHash    string
	expiresAt  time.Time
}

var (
	ErrBootstrapTooLarge = errors.New("bootstrap descriptor too large")
	ErrTooManyBootstraps = errors.New("too many bootstrap blobs")
)

func NewBootstrapStore(maxBytes, maxSites int) *BootstrapStore {
	return &BootstrapStore{m: make(map[string]*bootstrapEntry), maxBytes: maxBytes, maxSites: maxSites}
}

func (s *BootstrapStore) Put(siteID string, descriptor []byte, pinHash string, ttl time.Duration) error {
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
		pinHash:    pinHash,
		expiresAt:  time.Now().Add(ttl),
	}
	return nil
}

func (s *BootstrapStore) Claim(pinHash string) (descriptor []byte, siteID string, ok bool) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for site, e := range s.m {
		if now.After(e.expiresAt) {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(e.pinHash), []byte(pinHash)) == 1 {
			return append([]byte(nil), e.descriptor...), site, true
		}
	}
	return nil, "", false
}

func (s *BootstrapStore) Live(siteID, pinHash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[siteID]
	if !ok || time.Now().After(e.expiresAt) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(e.pinHash), []byte(pinHash)) == 1
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
