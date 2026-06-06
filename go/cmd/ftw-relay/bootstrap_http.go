package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// bootstrapTTL bounds how long a Pi-published first-enrollment descriptor stays
// claimable. Short on purpose: the browser claims it within the onboarding flow,
// the Pi re-publishes if the window lapses, and the janitor GCs anything stale so
// an abandoned bootstrap never lingers in relay memory.
const bootstrapTTL = 10 * time.Minute

// bootstrapPublishSigningString is the canonical message the Pi signs (ES256, raw
// r||s hex via nova.Identity.SignRawHex) to authorise parking its descriptor under
// a site, and the relay reconstructs to verify against the site's PINNED key. It is
// versioned and binds (site_id, pin_hash, sha256(descriptor)) so a captured
// signature can't be lifted to another site, re-keyed to a different PIN, or
// swapped onto a tampered descriptor. The descriptor itself is hashed (not inlined)
// so the signing string stays bounded regardless of descriptor size.
func bootstrapPublishSigningString(siteID, pinHash string, descriptor []byte) string {
	dh := sha256.Sum256(descriptor)
	return "ftw-bootstrap:v1:" + siteID + ":" + pinHash + ":" + hex.EncodeToString(dh[:])
}

// bootstrapPublishIO is the PUT /bootstrap/{site_id} body. descriptor is the
// Pi-signed cleartext directory descriptor (standard-base64, never parsed by the
// relay); pin_hash is hex(sha256(PIN)) — the claim key; sig is the ES256 raw r||s
// hex signature over bootstrapPublishSigningString, the SAME wire format the Pi's
// /me/register uses (verified with verifyES256Hex against the site's pinned key).
type bootstrapPublishIO struct {
	Descriptor string `json:"descriptor"`
	PinHash    string `json:"pin_hash"`
	Sig        string `json:"sig"`
}

// bootstrapClaimIO is the POST /bootstrap/claim response: the parked descriptor as
// an opaque string plus the site_id it belongs to. The relay never trust-parses
// the descriptor — the browser verifies the Pi signature inside it client-side.
type bootstrapClaimIO struct {
	SiteID     string `json:"site_id"`
	Descriptor string `json:"descriptor"`
}

// bootstrapPut handles PUT /bootstrap/{site_id}: the Pi parks its signed directory
// descriptor for the brief first-enrollment window. WRITER-AUTHENTICATED against the
// site's pinned ES256 key (the same key /me/register pinned), so only the owning Pi
// can publish under its site. The relay stores the descriptor BLIND (never parses
// it) keyed for claim by pin_hash. Statuses:
//   - 404  unknown site (no pinned key) OR bootstrap store not configured
//   - 400  malformed JSON / non-base64 descriptor / missing pin_hash
//   - 401  signature does not verify against the site's pinned key
//   - 413  descriptor over the per-blob byte cap
//   - 503  too many concurrent bootstraps (store cap)
//   - 200  parked
func (r *Relay) bootstrapPut(w http.ResponseWriter, req *http.Request) {
	siteID := req.PathValue("site_id")
	if siteID == "" || r.Bootstrap == nil || r.Owners == nil {
		http.NotFound(w, req)
		return
	}
	// Resolve the site's PINNED public key. An unknown site is a 404 — an anonymous
	// caller learns nothing about which sites exist, and a Pi must have registered
	// (pinned its key) before it can bootstrap.
	pub, ok := r.Owners.PublicKeyForSite(siteID)
	if !ok {
		http.NotFound(w, req)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxControlBodyBytes)
	var in bootstrapPublishIO
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		http.Error(w, "malformed bootstrap body", http.StatusBadRequest)
		return
	}
	if in.PinHash == "" {
		http.Error(w, "pin_hash required", http.StatusBadRequest)
		return
	}
	desc, err := base64.StdEncoding.DecodeString(in.Descriptor)
	if err != nil {
		http.Error(w, "descriptor is not valid base64", http.StatusBadRequest)
		return
	}
	// Verify the Pi signature binds (site_id, pin_hash, descriptor) to the site's
	// pinned key — exactly the /me/register wire format (raw r||s hex over
	// SHA-256(msg)). A mismatch is 401 and nothing is parked.
	if !verifyES256Hex(pub, bootstrapPublishSigningString(siteID, in.PinHash, desc), in.Sig) {
		http.Error(w, "invalid bootstrap signature", http.StatusUnauthorized)
		return
	}
	if err := r.Bootstrap.Put(siteID, desc, in.PinHash, bootstrapTTL); err != nil {
		switch {
		case errors.Is(err, ErrBootstrapTooLarge):
			http.Error(w, "bootstrap descriptor too large", http.StatusRequestEntityTooLarge)
		case errors.Is(err, ErrTooManyBootstraps):
			http.Error(w, "relay at capacity", http.StatusServiceUnavailable)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
}

// bootstrapClaim handles POST /bootstrap/claim: a fresh browser that knows the PIN
// pulls the Pi-parked descriptor. Unauthenticated (the PIN is the bearer secret)
// but rate-limited PER SOURCE IP so a flood can't brute-force PINs or grow memory.
// The claim is by hex(sha256(pin)); a miss is a clean 404 (the browser learns
// nothing about whether the site exists or the PIN was merely wrong). Statuses:
//   - 503  bootstrap store not configured
//   - 429  too many claim attempts from this address
//   - 400  malformed JSON / missing pin
//   - 404  no parked descriptor for this PIN
//   - 200  {site_id, descriptor}
func (r *Relay) bootstrapClaim(w http.ResponseWriter, req *http.Request) {
	walletBlobCORS(w)
	if r.Bootstrap == nil {
		http.Error(w, "bootstrap store not configured", http.StatusServiceUnavailable)
		return
	}
	// Reuse the per-source-IP offer throttle: a brute-force of the 6-digit PIN space
	// is bounded here on the un-spoofable client IP (CF-aware via offerClientIP).
	if r.OfferLimit != nil && !r.OfferLimit.Allow(r.offerClientIP(req)) {
		http.Error(w, "too many claim attempts from your address", http.StatusTooManyRequests)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxControlBodyBytes)
	var in struct {
		Pin string `json:"pin"`
	}
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		http.Error(w, "malformed claim body", http.StatusBadRequest)
		return
	}
	if in.Pin == "" {
		http.Error(w, "pin required", http.StatusBadRequest)
		return
	}
	h := sha256.Sum256([]byte(in.Pin))
	desc, siteID, ok := r.Bootstrap.Claim(hex.EncodeToString(h[:]))
	if !ok {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(bootstrapClaimIO{
		SiteID:     siteID,
		Descriptor: string(desc),
	})
}
