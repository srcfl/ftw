package main

import (
	"crypto/subtle"
	"errors"
	"sync"
	"time"
)

// OwnerRegistry maps a host's stable site_id to the host_id it polls
// under. A host registers on startup via POST /me/register and the
// public /me/<site_id>/... routes look up the matching host_id to
// enqueue tunneled requests.
//
// Each site_id is pinned to the ES256 public key that first registered it
// (trust-on-first-use), or to an operator-provisioned key seeded at startup via
// Pin (used for the internet-exposed home site). Once pinned, only a request
// signed by that key may change the site's host_id — so an unauthenticated
// caller can never repoint a site's tunnel mapping to a host it controls.
//
// In-memory and ephemeral — a relay restart drops all registrations (and TOFU
// pins) and hosts re-register on their next loop. Seed the home site's key with
// Pin so its pin survives restarts and is never first-come-first-served.
type OwnerRegistry struct {
	mu         sync.Mutex
	bySite     map[string]string    // site_id → host_id
	keyBySite  map[string]string    // site_id → pinned ES256 public key (hex X||Y)
	seenBySite map[string]time.Time // site_id → last successful registration
}

var (
	ErrSiteNotFound = errors.New("site not registered")
	// ErrKeyMismatch is returned when a registration presents a public key
	// that differs from the one already pinned for the site.
	ErrKeyMismatch = errors.New("site is pinned to a different key")
)

func NewOwnerRegistry() *OwnerRegistry {
	return &OwnerRegistry{
		bySite:     make(map[string]string),
		keyBySite:  make(map[string]string),
		seenBySite: make(map[string]time.Time),
	}
}

// Pin pre-pins a public key for a site, so it is authoritative from boot and
// never subject to first-come TOFU. Used at startup for the operator-
// provisioned home-site key. Idempotent; intended to be called once.
func (r *OwnerRegistry) Pin(siteID, pubKeyHex string) {
	r.mu.Lock()
	r.keyBySite[siteID] = pubKeyHex
	r.mu.Unlock()
}

// Register binds host_id to site_id, enforcing the pinned public key. The first
// registration for a site TOFU-pins pubKeyHex; later registrations must present
// the same key (constant-time compare) or are refused with ErrKeyMismatch. The
// caller is responsible for having verified the request's signature against
// pubKeyHex first — Register only enforces key continuity, never identity.
func (r *OwnerRegistry) Register(siteID, hostID, pubKeyHex string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pinned, ok := r.keyBySite[siteID]; ok {
		if subtle.ConstantTimeCompare([]byte(pinned), []byte(pubKeyHex)) != 1 {
			return ErrKeyMismatch
		}
	} else {
		r.keyBySite[siteID] = pubKeyHex // trust-on-first-use
	}
	r.bySite[siteID] = hostID
	r.seenBySite[siteID] = time.Now()
	return nil
}

// Lookup returns the host_id for a site_id, or ErrSiteNotFound.
func (r *OwnerRegistry) Lookup(siteID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hostID, ok := r.bySite[siteID]
	if !ok {
		return "", ErrSiteNotFound
	}
	return hostID, nil
}

// Active reports the host_id for a site and whether its registration is recent
// (last seen within maxAge). A host re-registers periodically, so a mapping
// that has gone stale means the Pi is offline — the caller can serve an
// "offline" page immediately instead of hanging on a dead tunnel. registered is
// false when the site was never seen at all.
func (r *OwnerRegistry) Active(siteID string, maxAge time.Duration) (hostID string, registered, fresh bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hostID, registered = r.bySite[siteID]
	if !registered {
		return "", false, false
	}
	fresh = time.Since(r.seenBySite[siteID]) <= maxAge
	return hostID, true, fresh
}

// Unregister removes a site mapping. Idempotent. The pinned key is retained so
// a later re-registration must still present the same key.
func (r *OwnerRegistry) Unregister(siteID string) {
	r.mu.Lock()
	delete(r.bySite, siteID)
	delete(r.seenBySite, siteID)
	r.mu.Unlock()
}

// List returns a snapshot of all registered (site_id, host_id) pairs.
// Used by health/observability endpoints.
func (r *OwnerRegistry) List() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.bySite))
	for k, v := range r.bySite {
		out[k] = v
	}
	return out
}
