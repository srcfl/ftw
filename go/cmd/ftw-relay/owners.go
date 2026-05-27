package main

import (
	"errors"
	"sync"
)

// OwnerRegistry maps a host's stable site_id to the host_id it polls
// under. A host registers on startup via POST /me/register and the
// public /me/<site_id>/... routes look up the matching host_id to
// enqueue tunneled requests.
//
// In-memory and ephemeral — a relay restart drops all registrations
// and hosts re-register on their next /tunnel/<host_id>/next loop.
type OwnerRegistry struct {
	mu      sync.Mutex
	bySite  map[string]string // site_id → host_id
}

var ErrSiteNotFound = errors.New("site not registered")

func NewOwnerRegistry() *OwnerRegistry {
	return &OwnerRegistry{bySite: make(map[string]string)}
}

// Register sets (or replaces) the host_id for a given site_id.
// Idempotent — a host re-registering after reconnect just refreshes
// the mapping.
func (r *OwnerRegistry) Register(siteID, hostID string) {
	r.mu.Lock()
	r.bySite[siteID] = hostID
	r.mu.Unlock()
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

// Unregister removes a site mapping. Idempotent.
func (r *OwnerRegistry) Unregister(siteID string) {
	r.mu.Lock()
	delete(r.bySite, siteID)
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
