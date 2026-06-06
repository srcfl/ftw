package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// bootstrapPublishMaxSkewMs bounds how far in the past or future a publish's ts_ms
// may sit (symmetric). The Pi mints ts_ms at PUT time; the relay rejects anything
// outside this window so a captured publish body can't be replayed once it lapses.
const bootstrapPublishMaxSkewMs = 30000

// isLowerHex64 reports whether s is exactly 64 lowercase hex chars — the shape of a
// hex(sha256(...)) digest. The claim_key MUST be a sha256 digest (the browser has
// already hashed the high-entropy bootstrap_id), so a malformed handle is a 400
// before any store lookup or crypto.
func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// bootstrapTTL bounds how long a Pi-published first-enrollment descriptor stays
// claimable. Short on purpose: the browser claims it within the onboarding flow,
// the Pi re-publishes if the window lapses, and the janitor GCs anything stale so
// an abandoned bootstrap never lingers in relay memory.
const bootstrapTTL = 10 * time.Minute

// bootstrapPublishSigningString is the canonical message the Pi signs (ES256, raw
// r||s hex via nova.Identity.SignRawHex) to authorise parking its descriptor under
// a site, and the relay reconstructs to verify against the site's PINNED key. It is
// versioned and binds (site_id, claim_key, ts_ms, sha256(descriptor)) so a captured
// signature can't be lifted to another site, re-keyed to a different claim_key,
// replayed after its window, or swapped onto a tampered descriptor. claim_key is
// hex(sha256(bootstrap_id)) — the relay never sees the raw bootstrap secret. The
// descriptor itself is hashed (not inlined) so the signing string stays bounded
// regardless of descriptor size. MUST be byte-identical to the Pi's reconstruction
// in go/internal/api/bootstrap_publish.go.
func bootstrapPublishSigningString(siteID, claimKey string, tsMs int64, descriptor []byte) string {
	dh := sha256.Sum256(descriptor)
	return "ftw-bootstrap:v1:" + siteID + ":" + claimKey + ":" + strconv.FormatInt(tsMs, 10) + ":" + hex.EncodeToString(dh[:])
}

// bootstrapPublishIO is the PUT /bootstrap/{site_id} body. descriptor is the
// Pi-signed cleartext directory descriptor (standard-base64, never parsed by the
// relay); claim_key is hex(sha256(bootstrap_id)) — the 256-bit unguessable claim
// handle (NEVER the 6-digit PIN); ts_ms is the Pi's mint time for the replay guard;
// sig is the ES256 raw r||s hex signature over bootstrapPublishSigningString, the
// SAME wire format the Pi's /me/register uses (verified with verifyES256Hex against
// the site's pinned key).
type bootstrapPublishIO struct {
	Descriptor string `json:"descriptor"`
	ClaimKey   string `json:"claim_key"`
	TsMs       int64  `json:"ts_ms"`
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
// it) keyed for claim by claim_key = hex(sha256(bootstrap_id)). Statuses:
//   - 404  unknown site (no pinned key) OR bootstrap store not configured
//   - 400  malformed JSON / non-base64 descriptor / claim_key not 64-char hex / stale ts_ms
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
	// claim_key must be a sha256 hex digest (the browser hashed the high-entropy
	// bootstrap_id; the relay never sees the secret). Reject any other shape before
	// touching crypto so a malformed handle can never become a store key.
	if !isLowerHex64(in.ClaimKey) {
		http.Error(w, "claim_key must be 64-char lowercase hex", http.StatusBadRequest)
		return
	}
	desc, err := base64.StdEncoding.DecodeString(in.Descriptor)
	if err != nil {
		http.Error(w, "descriptor is not valid base64", http.StatusBadRequest)
		return
	}
	// Order: verify-sig-THEN-skew. The signature binds ts_ms, so a failed sig means
	// the ts_ms is unauthenticated noise — there is nothing to replay-guard. Only an
	// authentic publish reaches the skew window check.
	//
	// Verify the Pi signature binds (site_id, claim_key, ts_ms, descriptor) to the
	// site's pinned key — exactly the /me/register wire format (raw r||s hex over
	// SHA-256(msg)). A mismatch is 401 and nothing is parked.
	if !verifyES256Hex(pub, bootstrapPublishSigningString(siteID, in.ClaimKey, in.TsMs, desc), in.Sig) {
		http.Error(w, "invalid bootstrap signature", http.StatusUnauthorized)
		return
	}
	// Replay guard: a captured (but authentic) publish body must not be re-parkable
	// once its window lapses. now() is read once; the skew window is symmetric.
	now := time.Now().UnixMilli()
	if d := now - in.TsMs; d > bootstrapPublishMaxSkewMs || d < -bootstrapPublishMaxSkewMs {
		http.Error(w, "stale bootstrap publish", http.StatusBadRequest)
		return
	}
	if err := r.Bootstrap.Put(siteID, desc, in.ClaimKey, bootstrapTTL); err != nil {
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

// bootstrapClaim handles POST /bootstrap/claim: a fresh browser that holds the
// claim_key (hex(sha256(bootstrap_id)), derived from the URL fragment) pulls the
// Pi-parked descriptor. Unauthenticated (the claim_key is the bearer secret — a
// 256-bit unguessable handle, NOT the 6-digit PIN) but rate-limited PER SOURCE IP
// so a flood can't grow memory. The claim is a DIRECT lookup by claim_key; the
// relay does NOT hash anything (the browser already hashed bootstrap_id). A miss is
// a clean 404 (the browser learns nothing about whether the site exists). Statuses:
//   - 503  bootstrap store not configured
//   - 429  too many claim attempts from this address
//   - 400  malformed JSON / claim_key not 64-char hex
//   - 404  no parked descriptor for this claim_key
//   - 200  {site_id, descriptor}
func (r *Relay) bootstrapClaim(w http.ResponseWriter, req *http.Request) {
	walletBlobCORS(w)
	if r.Bootstrap == nil {
		http.Error(w, "bootstrap store not configured", http.StatusServiceUnavailable)
		return
	}
	// Per-source-IP throttle on the un-spoofable client IP (CF-aware via
	// offerClientIP) bounds a memory-flood; the claim_key itself is unguessable.
	if r.OfferLimit != nil && !r.OfferLimit.Allow(r.offerClientIP(req)) {
		http.Error(w, "too many claim attempts from your address", http.StatusTooManyRequests)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxControlBodyBytes)
	var in struct {
		ClaimKey string `json:"claim_key"`
	}
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		http.Error(w, "malformed claim body", http.StatusBadRequest)
		return
	}
	if !isLowerHex64(in.ClaimKey) {
		http.Error(w, "claim_key must be 64-char lowercase hex", http.StatusBadRequest)
		return
	}
	// Direct lookup — the browser already hashed bootstrap_id, so the relay holds
	// only the digest and never the raw secret.
	desc, siteID, ok := r.Bootstrap.Claim(in.ClaimKey)
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

// bootstrapEnrollForward is the ONE narrow exception to the P2P-only home route:
// the single most security-sensitive new surface of multi-tenant onboarding.
//
// Under -multi-tenant the relay forces -require-device-key ON, so signalBrowserOffer
// hard-fails any WebRTC offer lacking a published device-key proof. But a FIRST-TIME
// user has no device key yet, so they can't open the P2P channel to enroll one. This
// handler bridges exactly that gap and NOTHING else: it forwards ONLY the two enroll
// RPCs (/api/owner-access/enroll/start and /enroll/finish) to the Pi over the tunnel,
// and ONLY while a live bootstrap blob exists for the resolved site — a blob the Pi
// publishes ONLY while a live LAN PIN is showing on its console. The forward is
// single-use: a successful enroll/finish burns the blob, closing the window.
//
// Every other owner-API path stays strictly P2P (homeStaticForward 403s /api/* under
// multi-tenant). `which` is "start" or "finish".
//
// TWO secrets, TWO checkers: the relay GATE is the high-entropy ?claim_key (=
// hex(sha256(bootstrap_id))) — a 256-bit unguessable handle the relay can resolve a
// live blob by. The 6-digit ?pin is forwarded UNTOUCHED to the Pi, which validates
// it (ownerAccessState.validateEnrollPin, 5-try burn). The relay NEVER inspects the
// PIN — so a leaked store key never reveals a guessable PIN, and the relay can't
// ride the enroll forward without the PIN the Pi still demands. Gates, in order:
//   - 503  bootstrap store not configured
//   - 429  per-source-IP rate limit (memory-flood backstop)
//   - 403  no claim_key query param
//   - 403  claim_key resolves to no LIVE bootstrap blob (the resolved site has no open window)
//   - 503  the resolved site's Pi is offline (not registered / stale)
//   - 413  request body over the control-body cap
//   - 502  the tunnel RPC to the Pi failed
//   - else the Pi's status, with Set-Cookie STRIPPED (the owner session cookie the Pi
//     mints on a successful enroll must never traverse the relay)
//
// The 403 for a missing-or-dead claim_key is identical to the no-claim_key 403 on
// purpose: an anonymous caller learns nothing about which sites have an open window.
func (r *Relay) bootstrapEnrollForward(which string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if r.Bootstrap == nil {
			http.Error(w, "bootstrap store not configured", http.StatusServiceUnavailable)
			return
		}
		// Bound a memory-flood on the un-spoofable source IP, the same throttle the
		// unauthenticated claim endpoint uses.
		if r.OfferLimit != nil && !r.OfferLimit.Allow(r.offerClientIP(req)) {
			http.Error(w, "too many enroll attempts from your address", http.StatusTooManyRequests)
			return
		}
		// The relay gates on claim_key (high-entropy handle); the pin rides through to
		// the Pi unread (the relay NEVER inspects it — see the forward below). A
		// missing claim_key is the same 403 as a dead one (§ doc).
		claimKey := req.URL.Query().Get("claim_key")
		if claimKey == "" {
			http.Error(w, "claim_key required", http.StatusForbidden)
			return
		}
		// Resolve the site WITHOUT burning: Claim is a read here. The window only
		// closes on a successful finish (single-use), never on a probe.
		_, site, ok := r.Bootstrap.Claim(claimKey)
		if !ok || !r.Bootstrap.Live(site, claimKey) {
			http.Error(w, "no live bootstrap for this claim_key", http.StatusForbidden)
			return
		}
		// Resolve the Pi's host_id. A site with an open enrollment window whose Pi
		// has gone offline can't be enrolled against → 503.
		hostID, registered, fresh := r.Owners.Active(site, homeStaleAfter)
		if !registered || !fresh {
			http.Error(w, "home offline", http.StatusServiceUnavailable)
			return
		}
		body, err := readBodyLimited(req.Body, maxControlBodyBytes)
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Preserve the browser's full query when forwarding, dropping ONLY the
		// relay-private claim_key (the Pi never sees it). The finish ceremony carries
		// ceremony_token + name in the query string (not the body); hardcoding
		// "?pin=" here would silently drop them and the Pi's enroll/finish would 400
		// "ceremony_token required", so a real multi-tenant enroll could never
		// complete. url.Values.Encode also percent-escapes every value, so the pin
		// (and any other param) round-trips cleanly through the tunnel host's URL
		// re-parse instead of a raw string concat injecting stray '&'/'#'/'%'/space
		// into the inner query.
		q := req.URL.Query()
		q.Del("claim_key")
		inner := "/api/owner-access/enroll/" + which + "?" + q.Encode()
		resp, err := r.enqueue(req, hostID, inner, body)
		if err != nil {
			http.Error(w, "home did not answer", http.StatusBadGateway)
			return
		}
		// Copy the Pi's response with the owner cookie stripped — the same chokepoint
		// homeStaticForward uses, so ftw_owner can never traverse the relay.
		writeTunneledNoCookie(w, resp)
		// Single-use: a completed enrollment closes the window. Only consume on a
		// finish that the Pi accepted (200) — a failed finish leaves the window open
		// so the user can retry without the Pi re-publishing. Consume verifies the
		// claim_key still matches and deletes the entry ATOMICALLY under the store
		// lock, so two concurrent in-flight finishes that both passed the live-gate
		// above can never both close the window (read-then-Burn would have raced).
		if which == "finish" && resp.Status == http.StatusOK {
			r.Bootstrap.Consume(site, claimKey)
		}
	}
}
