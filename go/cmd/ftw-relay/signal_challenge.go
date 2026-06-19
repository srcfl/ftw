package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"sync"
	"time"
)

// signal_challenge.go — the device-key proof challenge for the signaling offer
// (C2). Before the relay forwards a browser's WebRTC offer to the Pi, the browser
// must prove possession of a device key the Pi published (C1) by signing a
// single-use, short-TTL nonce the relay issued for that site:
//
//	GET  /signal/{site}/challenge          -> {"nonce","exp_ms"}
//	POST /signal/{site}/offer  + device_pubkey,nonce,sig
//	      sig over "ftw-signal:v1:<site>:<nonce>"  (raw r||s, base64url)
//
// The nonce is the relay's anti-replay token: the browser can't precompute a
// signature without it, and consuming it on use stops a captured (device_pubkey,
// nonce, sig) triple from being replayed to forward a second offer. This is what
// gates the relay→Pi forward on a key the Pi trusts, so a stranger who merely
// reached the home host can't make the relay contact the Pi at all.
//
// In-memory + ephemeral: a relay restart drops outstanding nonces and the browser
// simply re-fetches one. The store is per-site and bounded so a flood of
// challenge requests for many site_ids (or one) can't grow relay memory.

const (
	// signalChallengeTTL is how long an issued nonce stays valid. ~60s is ample
	// for a browser to sign + post its offer, tight enough that a leaked nonce is
	// useless almost immediately.
	signalChallengeTTL = 60 * time.Second
	// maxSignalChallengeSites bounds the number of distinct site_ids the nonce
	// store tracks, so an unauthenticated GET /signal/<random>/challenge flood
	// can't grow relay memory without limit. Expired sites are reclaimed lazily +
	// by GC, so this is a hard ceiling on live sites only.
	maxSignalChallengeSites = 4096
	// maxSignalNoncesPerSite bounds outstanding (un-consumed) nonces for one
	// site, so a single site_id can't be used to grow the store. A legit browser
	// holds ~1 in-flight challenge; this tolerates a few concurrent tabs/retries.
	// Past this the oldest nonce for the site is evicted.
	maxSignalNoncesPerSite = 16
)

// signalChallenge is one issued nonce with its absolute expiry.
type signalChallenge struct {
	exp time.Time
}

// SignalChallenges issues + consumes single-use signaling-offer nonces, keyed by
// (site_id, nonce). Safe for concurrent use; in-memory + ephemeral.
type SignalChallenges struct {
	mu     sync.Mutex
	bySite map[string]map[string]signalChallenge // site_id → nonce → challenge
}

func NewSignalChallenges() *SignalChallenges {
	return &SignalChallenges{bySite: make(map[string]map[string]signalChallenge)}
}

// Issue mints a fresh single-use nonce for siteID and returns it with its
// absolute expiry (ms since epoch). Returns ok=false only if the store is at its
// site ceiling for a brand-new site (surfaced as 503). The nonce is 32 bytes of
// CSPRNG entropy, base64url — opaque + unguessable, so it can't be precomputed.
func (c *SignalChallenges) Issue(siteID string) (nonce string, expMs int64, ok bool) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", 0, false
	}
	nonce = base64.RawURLEncoding.EncodeToString(b)
	now := time.Now()
	exp := now.Add(signalChallengeTTL)

	c.mu.Lock()
	defer c.mu.Unlock()
	site := c.bySite[siteID]
	if site == nil {
		if len(c.bySite) >= maxSignalChallengeSites {
			c.gcLocked(now) // try to make room by reaping expired sites first
			if len(c.bySite) >= maxSignalChallengeSites {
				return "", 0, false // still at capacity — refuse a new site
			}
		}
		site = make(map[string]signalChallenge)
		c.bySite[siteID] = site
	}
	// Bound per-site outstanding nonces: drop the soonest-to-expire (effectively
	// the oldest) if we're over the cap, so one site can't grow the store.
	if len(site) >= maxSignalNoncesPerSite {
		evictOldestChallengeLocked(site)
	}
	site[nonce] = signalChallenge{exp: exp}
	return nonce, exp.UnixMilli(), true
}

// Consume verifies (siteID, nonce) is a known, unexpired nonce and removes it
// (single-use). Returns false if unknown, expired, or already consumed — in every
// case the offer is refused and the Pi is never contacted. The lookup +
// constant-time match avoids leaking which of "unknown" vs "wrong" via timing.
func (c *SignalChallenges) Consume(siteID, nonce string) bool {
	if nonce == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	site := c.bySite[siteID]
	if site == nil {
		return false
	}
	// Constant-time membership check: iterate so a present-but-wrong nonce and an
	// absent one take the same path. The map key lookup below is the fast path;
	// the loop only guards against trivially distinguishing existence by timing.
	ch, present := site[nonce]
	match := 0
	for k := range site {
		if subtle.ConstantTimeCompare([]byte(k), []byte(nonce)) == 1 {
			match = 1
		}
	}
	if !present || match != 1 {
		return false
	}
	// Single-use: remove regardless of freshness so a captured nonce can never be
	// replayed even within its TTL.
	delete(site, nonce)
	if len(site) == 0 {
		delete(c.bySite, siteID)
	}
	if time.Now().After(ch.exp) {
		return false // expired → refuse (already removed above)
	}
	return true
}

// evictOldestChallengeLocked drops the soonest-to-expire nonce from a site map.
// Caller holds c.mu.
func evictOldestChallengeLocked(site map[string]signalChallenge) {
	var oldestKey string
	var oldestExp time.Time
	for k, ch := range site {
		if oldestKey == "" || ch.exp.Before(oldestExp) {
			oldestKey, oldestExp = k, ch.exp
		}
	}
	if oldestKey != "" {
		delete(site, oldestKey)
	}
}

// GC reaps expired nonces (and now-empty sites). Returns how many nonces were
// removed. Wired into the relay janitor so the store self-heals when sites go
// quiet. maxAge is ignored — a nonce's own absolute expiry is the only clock —
// but the parameter mirrors the other GC signatures for the janitor.
func (c *SignalChallenges) GC(_ time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gcLocked(time.Now())
}

// gcLocked removes every expired nonce and empty site. Caller holds c.mu.
func (c *SignalChallenges) gcLocked(now time.Time) int {
	removed := 0
	for site, nonces := range c.bySite {
		for n, ch := range nonces {
			if now.After(ch.exp) {
				delete(nonces, n)
				removed++
			}
		}
		if len(nonces) == 0 {
			delete(c.bySite, site)
		}
	}
	return removed
}
