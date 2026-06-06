# Multi-tenant Onboarding Bootstrap (Approach A) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a first-time user seed their encrypted directory — a one-time 6-digit LAN PIN couriers the Pi's signed descriptor to `home.*`, where the browser verifies it, enrolls through one hardened PIN-gated relay-forward, and seeds the directory.

**Architecture:** Spec `docs/superpowers/specs/2026-06-06-multi-tenant-onboarding-bootstrap-design.md`. The relay gains a blind, TTL'd, `site_id`-keyed `BootstrapStore` + `PUT /bootstrap/{site_id}` (Pi self-publish, identity-key-authed) + `POST /bootstrap/claim` (browser, PIN→descriptor) + a narrow `enroll/start+finish` forward gated by a live matching bootstrap blob. The Pi self-publishes its `instanceDescriptorSigningString`-signed descriptor while an enroll-PIN is live and zero devices exist. The web adds a LAN PIN display + a `home.*` claim→verify→enroll→seed flow.

**Tech Stack:** Go 1.26 (relay `go/cmd/ftw-relay`, Pi `go/internal/api`, tunnel `go/internal/tunnel`), vanilla ES-module web (`web/owner-access`), WebCrypto, WebAuthn + PRF.

**Worktree:** `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5`. Run all `go` commands from its `go/` dir, web tests from its `web/` dir.

---

## File structure

| File | Responsibility | New/Mod |
|---|---|---|
| `go/cmd/ftw-relay/bootstrap.go` | `BootstrapStore`: site-keyed, TTL'd, bounded, pin-hash-gated descriptor store | NEW |
| `go/cmd/ftw-relay/bootstrap_test.go` | store unit tests | NEW |
| `go/cmd/ftw-relay/bootstrap_http.go` | `walletBlobPut`-style handlers: `PUT /bootstrap/{site_id}`, `POST /bootstrap/claim`, the enroll-forward | NEW |
| `go/cmd/ftw-relay/bootstrap_http_test.go` | handler + enroll-forward hardening tests | NEW |
| `go/cmd/ftw-relay/handlers.go` | `Relay.Bootstrap` field; register the routes under `-multi-tenant`; the CORS + boot-rule fixes (already in worktree) | MOD |
| `go/cmd/ftw-relay/main.go` | construct `BootstrapStore`; janitor GC; (boot-rule fix already in worktree) | MOD |
| `go/internal/api/api_owner_access.go` | Pi self-publish the descriptor on PIN mint while zero devices | MOD |
| `go/internal/api/bootstrap_publish.go` | the self-publish helper (build descriptor + sign + PUT to relay) | NEW |
| `web/owner-access/index.html` | LAN "Set up remote access" → show the 6-digit PIN | MOD |
| `web/owner-access/enroll.html` | `home.*` claim-by-PIN → verifyEntry → enroll(forwarded) → seed | MOD |
| `web/next-app.js` | landing: "set up on your home network first" when no directory | MOD |

**Task 1 first lands the 3 pending worktree fixes** (CORS on `/wallet`, `requireHomePin` skip under `-multi-tenant`, enroll seed→`https://<rp.id>`) so the branch is clean before the new work.

---

## Task 1: Land the pending relay fixes (already implemented in the worktree)

**Files (already modified, uncommitted):** `go/cmd/ftw-relay/handlers.go` (CORS + `walletBlobCORS`/`walletBlobOptions`), `go/cmd/ftw-relay/main.go` (`if !*multiTenant { requireHomePin… }`), `go/cmd/ftw-relay/walletblob_http_test.go` (`TestWalletBlobCORS`), `web/owner-access/enroll.html` (relayBase = `https://<rp.id>`).

- [ ] **Step 1: Verify they're green.**
  Run: `cd <worktree>/go && go test ./cmd/ftw-relay/...` and `cd <worktree>/web && node --test`. Expected: all pass (CORS test + the existing suite).
- [ ] **Step 2: Commit them on a fresh branch off origin/master.**
```bash
cd <worktree> && git fetch origin
git stash                       # stash the worktree edits
git checkout -b feat/multi-tenant-onboarding origin/master
git stash pop                   # reapply (CORS/boot/seed + this plan + the spec)
git add go/cmd/ftw-relay/handlers.go go/cmd/ftw-relay/main.go go/cmd/ftw-relay/walletblob_http_test.go web/owner-access/enroll.html \
        docs/superpowers/specs/2026-06-06-multi-tenant-onboarding-bootstrap-design.md docs/superpowers/plans/2026-06-06-multi-tenant-onboarding-bootstrap.md
git commit -m "fix(relay): CORS on /wallet + skip requireHomePin under -multi-tenant; enroll seed targets the relay base"
```
  (If the stash/branch dance conflicts, just commit on the current `feat/multi-tenant-web` branch — the goal is only that these fixes are committed before Task 2.)

---

## Task 2: `BootstrapStore` — the blind, TTL'd, site-keyed descriptor store

**Files:** Create `go/cmd/ftw-relay/bootstrap.go` + `go/cmd/ftw-relay/bootstrap_test.go`.

- [ ] **Step 1: Write the failing test.** Create `bootstrap_test.go`:
```go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func pinHash(pin string) string { h := sha256.Sum256([]byte(pin)); return hex.EncodeToString(h[:]) }

func TestBootstrapStore_PutClaimBurn(t *testing.T) {
	s := NewBootstrapStore(64, 2048)
	desc := []byte(`{"site_id":"site:A","pi_pubkey":"…","label":"Home","sig":"…"}`)
	if err := s.Put("site:A", desc, pinHash("123456"), time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Claim with the right PIN returns the descriptor + site_id.
	got, site, ok := s.Claim(pinHash("123456"))
	if !ok || site != "site:A" || string(got) != string(desc) {
		t.Fatalf("Claim = %q,%q,%v want the descriptor for site:A", got, site, ok)
	}
	// Wrong PIN → miss.
	if _, _, ok := s.Claim(pinHash("000000")); ok {
		t.Fatal("wrong pin must miss")
	}
	// Burn removes it (single-use enrollment).
	s.Burn("site:A")
	if _, _, ok := s.Claim(pinHash("123456")); ok {
		t.Fatal("burned blob must be gone")
	}
}

func TestBootstrapStore_TTLAndGC(t *testing.T) {
	s := NewBootstrapStore(64, 2048)
	_ = s.Put("site:T", []byte("d"), pinHash("1"), -time.Second) // already expired
	if _, _, ok := s.Claim(pinHash("1")); ok {
		t.Fatal("expired blob must not claim")
	}
	if n := s.GC(); n != 1 {
		t.Fatalf("GC removed %d, want 1", n)
	}
}

func TestBootstrapStore_Caps(t *testing.T) {
	s := NewBootstrapStore(4, 2) // 4-byte desc cap, 2-site cap
	if err := s.Put("site:A", []byte("toolong!"), pinHash("1"), time.Minute); err != ErrBootstrapTooLarge {
		t.Fatalf("oversize: %v want ErrBootstrapTooLarge", err)
	}
	_ = s.Put("a", []byte("d"), pinHash("1"), time.Minute)
	_ = s.Put("b", []byte("d"), pinHash("2"), time.Minute)
	if err := s.Put("c", []byte("d"), pinHash("3"), time.Minute); err != ErrTooManyBootstraps {
		t.Fatalf("over cap: %v want ErrTooManyBootstraps", err)
	}
	// Re-publishing an existing site is allowed (the Pi refreshes on re-mint).
	if err := s.Put("a", []byte("e"), pinHash("9"), time.Minute); err != nil {
		t.Fatalf("refresh existing: %v", err)
	}
	_ = strings.TrimSpace
}
```
- [ ] **Step 2: Run it, see it FAIL.** `go test ./cmd/ftw-relay/ -run TestBootstrap` → `undefined: NewBootstrapStore`.
- [ ] **Step 3: Implement `bootstrap.go`:**
```go
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
	mu        sync.Mutex
	m         map[string]*bootstrapEntry
	maxBytes  int
	maxSites  int
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

// Put publishes (or refreshes) a site's bootstrap descriptor. Caller has already
// verified the request was authenticated by the site's pinned Pi key.
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

// Claim returns the descriptor + site_id for the (live) entry whose pinHash
// matches, or ok=false. Constant-time pin compare; expired entries never match.
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

// Live reports whether a non-expired bootstrap exists for site whose pinHash
// matches — the gate the enroll-forward checks (a forged site_id with no live
// bootstrap is refused).
func (s *BootstrapStore) Live(siteID, pinHash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[siteID]
	if !ok || time.Now().After(e.expiresAt) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(e.pinHash), []byte(pinHash)) == 1
}

// Burn removes a site's bootstrap (single-use, called after a successful enroll).
func (s *BootstrapStore) Burn(siteID string) {
	s.mu.Lock()
	delete(s.m, siteID)
	s.mu.Unlock()
}

// GC drops expired entries; returns how many. Wired into the relay janitor.
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
```
- [ ] **Step 4: Run it, see it PASS.** `go test ./cmd/ftw-relay/ -run TestBootstrap` → `ok`.
- [ ] **Step 5: Commit.** `git add go/cmd/ftw-relay/bootstrap.go go/cmd/ftw-relay/bootstrap_test.go && git commit -m "feat(relay): BootstrapStore — blind, TTL'd, site-keyed descriptor store"`

---

## Task 3: `PUT /bootstrap/{site_id}` + `POST /bootstrap/claim` handlers

**Files:** Create `go/cmd/ftw-relay/bootstrap_http.go`; add `Bootstrap *BootstrapStore` to the `Relay` struct in `handlers.go`; register routes under `-multi-tenant` in `Handler()`.

- [ ] **Step 1: Write the failing test** (`bootstrap_http_test.go`): build a multi-tenant relay (extend `newMultiTenantRelay` to set `Bootstrap: NewBootstrapStore(65536, 4096)`); register `site:A` with a test identity so `Owners.PublicKeyForSite("site:A")` returns its key; `PUT /bootstrap/site:A` signed by that identity over the canonical publish string → 200; an unsigned/mis-signed PUT → 401; `POST /bootstrap/claim {pin}` → returns the descriptor for a matching pin, 404 for a miss. (Mirror the `walletblob_http_test.go` style — `httptest.NewServer(relay.Handler())`.)
- [ ] **Step 2: Run it, see it FAIL** (`undefined: bootstrapPublishSigningString` / routes 404).
- [ ] **Step 3: Implement `bootstrap_http.go`:**
```go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

const bootstrapTTL = 10 * time.Minute

// bootstrapPublishSigningString is what the Pi signs to PUT /bootstrap/{site_id}.
// Reuses the same ES256 identity the relay already pins via /me/register, and
// binds site_id + pin_hash + the descriptor hash so the relay can verify
// authenticity without trust-parsing the descriptor.
func bootstrapPublishSigningString(siteID, pinHash string, descriptor []byte) string {
	dh := sha256.Sum256(descriptor)
	return "ftw-bootstrap:v1:" + siteID + ":" + pinHash + ":" + hex.EncodeToString(dh[:])
}

type bootstrapPublishIO struct {
	Descriptor string `json:"descriptor"` // std-base64 of the Pi-signed descriptor JSON
	PinHash    string `json:"pin_hash"`   // sha256(PIN) hex
	Sig        string `json:"sig"`        // ES256 raw r||s, base64url, over bootstrapPublishSigningString
}

// bootstrapPut: the Pi self-publishes its descriptor. Authenticated by the site's
// pinned identity key (same trust as /me/register). Multi-tenant only.
func (r *Relay) bootstrapPut(w http.ResponseWriter, req *http.Request) {
	siteID := req.PathValue("site_id")
	pub, ok := r.Owners.PublicKeyForSite(siteID)
	if siteID == "" || !ok || r.Bootstrap == nil {
		http.Error(w, "unknown site", http.StatusNotFound)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxControlBodyBytes)
	var in bootstrapPublishIO
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	desc, err := stdB64Decode(in.Descriptor) // helper already in the package (base64.StdEncoding)
	if err != nil {
		http.Error(w, "descriptor not base64", http.StatusBadRequest)
		return
	}
	msg := bootstrapPublishSigningString(siteID, in.PinHash, desc)
	if !verifyES256Hex(pub, msg, in.Sig) { // same verifier /me/register uses
		http.Error(w, "signature does not verify against the site key", http.StatusUnauthorized)
		return
	}
	if err := r.Bootstrap.Put(siteID, desc, in.PinHash, bootstrapTTL); err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// bootstrapClaim: the browser at home.* exchanges the PIN for the Pi-signed
// descriptor. Rate-limited per IP (reuse OfferLimit). The relay learns only
// sha256(pin)+site_id, never userHandle.
func (r *Relay) bootstrapClaim(w http.ResponseWriter, req *http.Request) {
	walletBlobCORS(w) // same-origin in practice, but harmless + consistent
	if r.Bootstrap == nil {
		http.Error(w, "not configured", http.StatusServiceUnavailable)
		return
	}
	if r.OfferLimit != nil && !r.OfferLimit.Allow(clientIP(req)) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxControlBodyBytes)
	var in struct{ Pin string `json:"pin"` }
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil || in.Pin == "" {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	h := sha256.Sum256([]byte(in.Pin))
	desc, site, ok := r.Bootstrap.Claim(hex.EncodeToString(h[:]))
	if !ok {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"site_id": site, "descriptor": string(desc)})
	_ = strconv.Itoa
}
```
  NOTE: reuse the package's existing base64/ES256 helpers — grep for how `meRegister` verifies (`verifyES256Hex`-equivalent) and how `walletBlobPut` base64-decodes; use those exact helpers rather than the placeholder names above. Add `clientIP(req)` if not present (the offer limiter already extracts it).
- [ ] **Step 4: Register routes** in `handlers.go` inside the `if r.MultiTenant {` host-less block AND the HomeHost block:
```go
mux.HandleFunc("PUT /bootstrap/{site_id}", r.bootstrapPut)
mux.HandleFunc("POST /bootstrap/claim", r.bootstrapClaim)
mux.HandleFunc("OPTIONS /bootstrap/claim", r.walletBlobOptions)
```
  and the `r.HomeHost+`-prefixed equivalents.
- [ ] **Step 5: Wire `main.go`.** Add the `Bootstrap *BootstrapStore` field to the `Relay` struct (handlers.go, next to `WalletBlobs`). In `main.go`, under the existing `if *multiTenant {` block, construct it and set it on the `Relay{}` literal:
```go
bootstrap := NewBootstrapStore(65536, 4096) // 64 KiB descriptor cap, 4096 concurrent onboards
// … Relay{ …, Bootstrap: bootstrap }
```
  and in the janitor ticker loop add (next to the wallet-blob note — bootstrap IS GC'd, it's ephemeral):
```go
if r.Bootstrap != nil {
	if n := r.Bootstrap.GC(); n > 0 {
		slog.Info("ftw-relay: bootstrap GC", "removed", n)
	}
}
```
- [ ] **Step 6: Run + PASS, then commit.** `go test ./cmd/ftw-relay/ -run TestBootstrap` → `ok`; `go build ./cmd/ftw-relay/` clean. `git add … && git commit -m "feat(relay): /bootstrap publish + claim endpoints (multi-tenant)"`

---

## Task 4: The hardened enroll-forward

**Files:** add the handler to `bootstrap_http.go`; register `POST {HomeHost}/api/owner-access/enroll/{start,finish}` under `-multi-tenant`.

This is the single most security-sensitive new surface. It forwards ONLY `enroll/start`+`enroll/finish` to the Pi, ONLY with a `pin` matching a live bootstrap for the resolved `site_id`, single-use, rate-limited.

- [ ] **Step 1: Write the failing hardening test** (`bootstrap_http_test.go`): with a registered `site:A` + a live bootstrap blob (pin "123456") + a stub Pi draining the tunnel (use the existing `Queue` test helper / a goroutine that `Poll`s and `PostResponse`s a 200 for enroll/start, then for enroll/finish): (a) `POST {home}/api/owner-access/enroll/start?pin=123456` → forwarded → 200; (b) the SAME with a wrong pin → 403 (no live bootstrap); (c) after a successful `enroll/finish`, the bootstrap is burned → a second `enroll/start?pin=123456` → 403; (d) a non-enroll path e.g. `…/whoami` is NOT forwarded by this handler (404/refused); (e) a request over the loopback/friend path is refused. Assert `r.Bootstrap.Live("site:A", …)` is false after finish.
- [ ] **Step 2: Run it, see it FAIL.**
- [ ] **Step 3: Implement `bootstrapEnrollForward`:**
```go
// bootstrapEnrollForward is the ONE narrow pre-device-key path to the Pi. It
// forwards enroll/start + enroll/finish over the tunnel ONLY when a live bootstrap
// blob exists for the resolved site whose pin_hash matches the request's ?pin.
// Single-use: a successful enroll/finish burns the bootstrap. The Pi independently
// re-checks zero-devices + PIN in enrollAllowed (defence in depth).
func (r *Relay) bootstrapEnrollForward(which string) http.HandlerFunc { // which = "start" | "finish"
	return func(w http.ResponseWriter, req *http.Request) {
		if r.Bootstrap == nil {
			http.Error(w, "not configured", http.StatusServiceUnavailable)
			return
		}
		if r.OfferLimit != nil && !r.OfferLimit.Allow(clientIP(req)) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		pin := req.URL.Query().Get("pin")
		if pin == "" {
			http.Error(w, "bootstrap enroll requires ?pin", http.StatusForbidden)
			return
		}
		h := sha256.Sum256([]byte(pin))
		ph := hex.EncodeToString(h[:])
		// Resolve the site from the live bootstrap (the pin authorizes exactly one site).
		_, site, ok := r.Bootstrap.Claim(ph) // Claim is a read here; do NOT burn yet
		if !ok || !r.Bootstrap.Live(site, ph) {
			http.Error(w, "no live bootstrap for this pin", http.StatusForbidden)
			return
		}
		hostID, registered, fresh := r.Owners.Active(site, homeStaleAfter)
		if !registered || !fresh {
			http.Error(w, "home offline", http.StatusServiceUnavailable)
			return
		}
		body, err := readBodyLimited(req.Body, maxControlBodyBytes)
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		inner := "/api/owner-access/enroll/" + which + "?pin=" + pin
		resp, err := r.enqueue(req, hostID, inner, body)
		if err != nil {
			http.Error(w, "home did not respond", http.StatusBadGateway)
			return
		}
		// Strip any Set-Cookie the Pi returned — the ftw_owner session never
		// traverses the relay (base-design invariant).
		writeTunneledResponseNoCookie(w, resp) // reuse homeStaticForward's cookie-stripping writer
		if which == "finish" && resp.Status == http.StatusOK {
			r.Bootstrap.Burn(site) // single-use: enrollment done
		}
	}
}
```
  NOTE: `r.enqueue`, `r.Owners.Active`, `homeStaleAfter`, `readBodyLimited`, and the cookie-stripping response writer all already exist (grep `homeStaticForward` for the Set-Cookie strip + `enqueue` at handlers.go:1134). Use them verbatim; the names above mirror them.
- [ ] **Step 4: Register** in `handlers.go` (HomeHost block, under `if r.MultiTenant`):
```go
mux.HandleFunc("POST "+r.HomeHost+"/api/owner-access/enroll/start", r.bootstrapEnrollForward("start"))
mux.HandleFunc("POST "+r.HomeHost+"/api/owner-access/enroll/finish", r.bootstrapEnrollForward("finish"))
```
  Confirm `homeStaticForward`'s `/api/*` 403 does NOT shadow these (more specific patterns win in Go ServeMux; add a test asserting the forward fires, not the 403).
- [ ] **Step 5: Run + PASS, commit.** `git commit -m "feat(relay): hardened bootstrap enroll-forward (pin-gated, single-use, rate-limited)"`

---

## Task 5: Pi self-publishes the descriptor on PIN mint (zero devices)

**Files:** Create `go/internal/api/bootstrap_publish.go`; call it from `handleOwnerEnrollPin` in `api_owner_access.go`.

- [ ] **Step 1: Write the failing test** (`go/internal/api/bootstrap_publish_test.go`): a fake relay `httptest.Server` capturing `PUT /bootstrap/{site_id}`; call the publish helper with a known identity + pin; assert the captured body's `sig` verifies against the identity over `bootstrapPublishSigningString` AND the descriptor inside is the same one `verifyEntry` would accept (reuse the descriptor builder from `api_owner_instance_descriptor.go`). Assert publish is SKIPPED when `LoadTrustedDevices()` is non-empty.
- [ ] **Step 2: Run, FAIL.**
- [ ] **Step 3: Implement** `publishBootstrapDescriptor(deps, relayBase, pin)`:
  - guard: `len(LoadTrustedDevices()) == 0` else return nil (no publish once enrolled).
  - build the descriptor exactly as `handleOwnerInstanceDescriptor` does (reuse `instanceDescriptorSigningString` + the ES256 identity; produce `{site_id, pi_pubkey, label, sig}` JSON).
  - compute `pin_hash = hex(sha256(pin))`; sign `bootstrapPublishSigningString(site_id, pin_hash, descriptorJSON)` with the identity; `PUT {relayBase}/bootstrap/{site_id}` `{descriptor: b64(descriptorJSON), pin_hash, sig}`. Best-effort (log on failure; never block the PIN response).
- [ ] **Step 4: Call it** at the end of `handleOwnerEnrollPin` (after a successful mint), in a goroutine, using the configured relay base (the same `FTW_RELAY_URL`/home origin the Pi already knows; if absent, skip).
- [ ] **Step 5: Run + PASS, commit.** `go test ./internal/api/... -run Bootstrap` → ok. `git commit -m "feat(pi): self-publish the signed descriptor to /bootstrap while a PIN is live + zero devices"`

---

## Task 6: Web — LAN PIN display + `home.*` claim→verify→enroll→seed

**Files:** `web/owner-access/index.html` (LAN), `web/owner-access/enroll.html` (home.*), `web/next-app.js` (landing).

- [ ] **Step 1 (LAN PIN):** in `index.html`, on a genuine-LAN origin, add a "Set up remote access" affordance that GETs the enroll-PIN endpoint and shows the 6-digit PIN big + amber + a live countdown + copy (reuse the existing PIN display markup already in `enroll.html`). Plain-HTTP display only — no WebAuthn here. Node test: `web/lan-pin.test.mjs` asserts the PIN render + countdown logic.
- [ ] **Step 2 (home.* claim):** in `enroll.html`, BEFORE the WebAuthn create, when a PIN was entered: `POST {https://<rp.id>}/bootstrap/claim {pin}` → `{site_id, descriptor}` → `JSON.parse` + `verifyEntry(descriptor)` (import from `instance-sync.js`) → ABORT with a clear error if the Pi signature fails. Store the verified `{site_id, pi_pubkey, label}` for the seed. Node test (`enroll-bootstrap.test.mjs`): a mock relay returns a Pi-signed descriptor → claim+verify passes; a tampered descriptor → rejected.
- [ ] **Step 3 (enroll via forward):** the existing `enroll/start`+`enroll/finish` calls already go through `ownerFetch`; on `home.*` they hit the relay forward (Task 4) with `?pin=`. Ensure the `pin` rides on those requests. After `enroll/finish` succeeds, run the seed (Task already in `enroll.html`): `saveDirectory(W, encKey, "https://"+rpId, {instances:[verifiedDescriptor], version:0, writeKey:null})`.
- [ ] **Step 4 (landing guidance):** in `next-app.js`, when on the public route with no decryptable directory AND no PIN entered, show "Set up on your home network first" with a short how-to (open your Pi on Wi-Fi → get the code), instead of letting sign-in synth-503. Node test (`landing-setup-hint.test.mjs`).
- [ ] **Step 5: Run + PASS, commit.** `cd web && node --test` → all green. `git commit -m "feat(web): LAN PIN setup + home.* claim/verify/enroll/seed bootstrap"`

---

## Task 7: e2e + interop + docs

- [ ] **Step 1 (e2e):** extend `go/test/e2e` — a full bootstrap on the docker harness: mint a PIN on the (simulated-LAN) Pi → Pi self-publishes → a client claims + enrolls via the forward + seeds → a fresh session loads the directory + routes. Assert a no-PIN offer is still refused (C2 fail-closed). Run: `make e2e`.
- [ ] **Step 2 (interop):** a Go→JS fixture lock — the Pi's bootstrap descriptor verifies in the browser `verifyEntry` (reuse the `instance-interop.test.mjs` pattern; the descriptor signing already has a fixture from the multi-tenant slice).
- [ ] **Step 3 (docs):** update `docs/relay-deploy.md` (the `/bootstrap` + enroll-forward surface, the `-multi-tenant` flags) and `docs/remote-access.md` (the new-user onboarding steps: LAN PIN → home.* → passkey).
- [ ] **Step 4: changeset + commit.** `npx changeset` (minor — new onboarding capability). Commit.

---

## REVISION (post-Codex-audit) — rework Tasks 2–6 to the high-entropy `bootstrap_id` model

Tasks 1–5 are committed + green but on the SUPERSEDED 6-digit-`pin_hash`-as-relay-key
model. A Codex audit found a BLOCKER (PIN is offline-brute-forceable by the relay)
+ 4 HIGH/MED (global PIN lookup, non-atomic single-use, publish replay, late
Set-Cookie strip, no zero-device enforcement). See the spec's REVISION section.
Fredrik chose the **QR + `#fragment` link** fix. Rework as follows (the committed
code is the starting point — transform it, don't rebuild from scratch).

**Contract changes (apply consistently):**
- The Pi mints a HIGH-ENTROPY `bootstrap_id` (≥32 B CSPRNG, base64url) when it mints
  the enroll-PIN. The relay keys the bootstrap store on `claimKey = sha256(bootstrap_id)`
  — NOT on the PIN. The browser sends `claimKey` (it computes `sha256(bootstrap_id)`
  from the `#fragment`) to `/bootstrap/claim` and to the enroll-forward, so the relay
  never holds the raw secret and `claimKey` is unguessable.
- `bootstrapPublishSigningString` becomes `"ftw-bootstrap:v1:" + site_id + ":" +
  claimKey + ":" + ts_ms + ":" + hex(sha256(descriptor))`; the relay rejects
  `|now - ts_ms| > 30s` (replay guard). Keep both sig encodings (inner descriptor
  sig base64url; outer publish sig hex).
- The 6-digit PIN stays, validated by the **Pi** (`validateEnrollPin`, 5-try burn) on
  the forwarded enroll — NOT by the relay.

### Task R1 — `BootstrapStore`: rename `pinHash`→`claimKey`; add atomic `Consume`
**Files:** `go/cmd/ftw-relay/bootstrap.go` + `bootstrap_test.go`.
- [ ] Rename the `pinHash` field/params to `claimKey` (mechanical; the value is now
  `sha256(bootstrap_id)`). Behaviour identical.
- [ ] Add `func (s *BootstrapStore) Consume(siteID, claimKey string) ([]byte, bool)` —
  under the lock, verify the live entry's `claimKey` matches (constant-time) then
  delete-and-return atomically (so two concurrent `enroll/finish` can't both pass).
- [ ] Test: `Consume` returns the descriptor + removes it; a second `Consume`/`Claim`
  misses; a non-matching key never consumes. Run `go test ./cmd/ftw-relay/ -run TestBootstrap` → green. Commit.

### Task R2 — `/bootstrap` publish + claim: `claimKey` + `ts_ms` replay guard
**Files:** `go/cmd/ftw-relay/bootstrap_http.go` + test.
- [ ] `bootstrapPublishIO` gains `ts_ms int64`; `bootstrapPublishSigningString` gains
  `claimKey` + `ts_ms` (as above). `bootstrapPut`: reject skew > 30 s; store keyed by
  the `claimKey` the PUT carries (verify the outer sig over the new string first).
- [ ] `bootstrapClaim` body becomes `{claim_key}` (the browser-computed
  `sha256(bootstrap_id)` hex); look up by `claimKey` (still per-site unique).
- [ ] Tests: a stale `ts_ms` PUT → rejected; claim by `claim_key` → descriptor; wrong
  key → 404. Commit.

### Task R3 — enroll-forward: gate on `claimKey`, single-use via `Consume`
**Files:** `bootstrap_http.go` + test.
- [ ] `bootstrapEnrollForward` reads `claim_key` (query or header), resolves the live
  bootstrap by it (`Claim`/`Live` read, no burn on `start`); on `enroll/finish` 200 use
  `Consume(site, claimKey)` instead of `Burn` (atomic single-use).
- [ ] Add the zero-device relay guard: in `meRegister`, when a site's `/me/register`
  publishes a non-empty `device_pubkeys` set (C1), call `r.Bootstrap.Burn(site)` — so a
  replayed/stale bootstrap can never reach an already-enrolled Pi.
- [ ] Tests: the existing 8 + (a) concurrent double-finish only one succeeds; (b) after
  a `/me/register` with device keys, the forward 403s. Commit.

### Task R4 — Pi: generate `bootstrap_id`, publish keyed by it, suppress the cookie
**Files:** `go/internal/api/bootstrap_publish.go`, `api_owner_access.go`, the enroll-finish handler.
- [ ] On enroll-PIN mint, generate a `bootstrap_id` (CSPRNG, base64url), stash it with
  the PIN (so the LAN page can show it). `publishBootstrapDescriptor` publishes keyed by
  `sha256(bootstrap_id)` with `ts_ms`.
- [ ] Return the `bootstrap_id` from the enroll-PIN endpoint (so the LAN page builds the
  QR + `#fragment` link). It NEVER goes to the relay raw — only its sha256 does, from the
  browser.
- [ ] On a bootstrap-forwarded enroll (the request arrived via the relay tunnel for the
  zero-device window), SUPPRESS the `ftw_owner` Set-Cookie (don't issue it — the seed is
  write-sig-authed; steady-state sign-in mints the session over P2P). Strip it Pi-side
  before the tunneled response is posted.
- [ ] Tests: publish keyed by `sha256(bootstrap_id)`; the cookie is absent on a
  tunnel-forwarded enroll-finish. Commit.

### Task R5 — web: QR + `#fragment` link (replaces the typed PIN as the courier)
**Files:** `web/owner-access/index.html` (LAN), `web/owner-access/enroll.html` (home.*).
- [ ] LAN page: on a genuine-LAN origin, GET the enroll-PIN endpoint → render a **QR**
  (vendor a tiny zero-dep QR module under `web/vendor/`, or draw on a canvas) encoding
  `https://<rp.id>/owner-access/enroll.html#b=<bootstrap_id>`, PLUS a clickable link with
  the same URL, PLUS the 6-digit PIN (shown for the optional manual factor). Display-only —
  no WebAuthn here.
- [ ] `home.*` `enroll.html`: read `bootstrap_id` from `location.hash` (`#b=`); compute
  `claim_key = hex(sha256(bootstrap_id))`; `POST /bootstrap/claim {claim_key}` → verify
  the descriptor (`verifyEntry`) → run enroll with `?claim_key=` on the forwarded
  start/finish (+ the optional PIN the Pi validates) → seed. Remove the typed-PIN-as-relay
  path. Clear the hash after reading (don't leave the secret in history).
- [ ] Node tests for the hash-parse + claim_key derivation + the verify-before-trust gate. Commit.

### Task R6 — e2e + interop + docs + changeset (as the original Task 7, updated for `bootstrap_id`)

## Before the flag is flipped in production (manual gates — NOT code tasks)

1. **Codex audit** of `bootstrap.go` + `bootstrap_http.go` (especially `bootstrapEnrollForward`): confirm the forward is unreachable without a live PIN, single-use, rate-limited, refused post-enrollment, and never reachable over the friend loopback tunnel.
2. **PRF determinism device test** on real synced devices (iPhone iCloud Keychain + Android Google Password Manager): a synced passkey yields an identical PRF output, so `deriveEncKey` reproduces `K_dir` on a fresh device. If it fails, ship browser-carried-only and surface "encrypted home sync unavailable".
3. Then: deploy the web bundle as `-home-web`, set `-multi-tenant` (+ forced `-require-device-key`) + `-wallet-blob-dir`, and do a live owner onboarding before announcing.

## REVISION 2 (post-audit-2) — fix tasks F1–F5

The second Codex audit (see the spec's REVISION 2) found a BLOCKER (single-use
consumed after the Pi's enroll side effects; `handleOwnerEnrollFinish` never
re-ran the zero-device check) + a HIGH (relay sees the reusable plaintext PIN →
a compromised relay can run its own enrollment) + INFO items. Fredrik chose the
**ceremony-bound possession proof** fix. New contract pieces (apply consistently):
`bootstrap_proof = hex(HMAC-SHA256(key=utf8(bootstrap_id), msg=utf8(ceremony_token)))`,
validated Pi-side at finish on the `isTunneled` path; relay `Reserve`s the
bootstrap before forwarding finish (`Burn` on 200, `Release` on non-200); Pi
re-checks zero-device at finish. Keep the two-sig contract unchanged; the proof
is a THIRD, separate HMAC.

### Task F1 — relay: reserved-flag single-use + `isLowerHex64` gate
**Files:** `go/cmd/ftw-relay/bootstrap.go` + `bootstrap_test.go`; `go/cmd/ftw-relay/bootstrap_http.go` + `bootstrap_http_test.go`.
- [ ] BootstrapStore: add a `reserved bool` to `bootstrapEntry` + `Reserve(siteID, claimKey string) (descriptor []byte, ok bool)` (atomic: live + constant-time claimKey match + not already reserved → set reserved, return descriptor copy + true; else false) and `Release(siteID, claimKey string)` (clear the flag iff claimKey matches). Keep `Burn`. (`Consume` may stay or be removed — the forward no longer uses it.)
- [ ] `bootstrapEnrollForward`: add `if !isLowerHex64(claimKey) { 403 }` before the store lookup (uniform 403). On `finish`: `Reserve` BEFORE `r.enqueue`; on Pi 200 → `Burn(site)`; on non-200 (or enqueue error) → `Release(site, claimKey)`. `start` stays a non-burning `Claim/Live` read.
- [ ] Tests: Reserve atomicity (N goroutines → exactly one ok); a reserved entry can't be claimed/reserved again; Release reopens it; second concurrent finish → 403; non-200 finish releases (retry works); non-hex claim_key → 403. Full `go test ./cmd/ftw-relay/...` green. Commit.

### Task F2 — Pi: validate the possession proof at finish + zero-device recheck + clear bootstrap_id
**Files:** `go/internal/api/api_owner_access.go` + tests (`api_owner_enroll_cookie_test.go` has a software authenticator to reuse).
- [ ] Add a helper `bootstrapEnrollProof(bootstrapID, ceremonyToken string) string` = `hex(HMAC-SHA256(key=[]byte(bootstrapID), msg=[]byte(ceremonyToken)))` (crypto/hmac + crypto/sha256).
- [ ] `handleOwnerEnrollFinish`, when `s.isTunneled(r)` (the bootstrap path): require `?bootstrap_proof`; constant-time-compare it against `bootstrapEnrollProof(oa.enrollBootstrapID, tok)` — mismatch/empty → 403, no save. Read `enrollBootstrapID` under `oa.mu`. ALSO re-check the zero-device window (LoadTrustedDevices empty) on this path before `SaveTrustedDevice` — non-empty → 403. (Untunneled LAN finish unchanged: no proof, no recheck.)
- [ ] Clear `oa.enrollBootstrapID` in `validateEnrollPin`'s burn/expiry branches (next to `oa.enrollPin = ""`) and after a successful tunneled enroll.
- [ ] Tests: tunneled finish with a valid proof + zero devices → 200; missing/wrong proof → 403; tunneled finish when a device already exists → 403; untunneled LAN finish needs no proof (still 200 + cookie). Reuse the software-authenticator ceremony. Full `go test ./internal/api/...` green. Commit.

### Task F3 — web: compute + send the possession proof at finish
**Files:** `web/owner-access/bootstrap-enroll.js` + `bootstrap-enroll.test.mjs`; `web/owner-access/enroll.html`.
- [ ] Keep `bootstrap_id` in memory through the ceremony (the hash is still cleared from the URL immediately). At finish: `bootstrap_proof = hex(HMAC-SHA256(key=bootstrap_id, msg=ceremony_token))` via `crypto.subtle.importKey('raw', utf8(bootstrap_id), {name:'HMAC',hash:'SHA-256'}, …)` + `sign`; send `?bootstrap_proof=<hex>` on `enroll/finish` (alongside the existing `?claim_key=`, `?pin=`, `?ceremony_token=`). `enroll/start` unchanged.
- [ ] Node tests: proof derivation matches a Go-computed vector for a fixed (bootstrap_id, ceremony_token); the finish request carries `bootstrap_proof`. `node --test` green. Commit.

### Task F4 — Pi production wiring: multi-tenant enroll-forward tunnel host
**Files:** `go/cmd/forty-two-watts/owner_relay_register.go` (+ test).
- [ ] Under `-multi-tenant`, drain the relay tunnel with a host that serves ONLY `POST /api/owner-access/enroll/start` + `/finish` (route to the real `api.Server` handlers), stamps `X-FTW-Tunnel=<tunnelMarker>` so `isTunneled` is true (gating the proof + cookie-suppression + PIN), and keeps Set-Cookie stripped on the response. Everything else stays on the static-asset host (still 403/405). Do NOT broaden the static host.
- [ ] Test: a POST enroll/start over this host reaches the handler with the marker stamped; a non-enroll path is still refused. `go test ./cmd/forty-two-watts/...` green (or the nearest existing test target). Commit.

### Task F5 — e2e + interop + docs + changeset update
**Files:** `go/test/e2e/bootstrap_onboarding_test.go`; docs; `.changeset/onboarding-bootstrap-id.md`.
- [ ] e2e: drive the REAL `main.go` enroll-forward host (F4) instead of the bespoke stand-in; complete a finish with a software-authenticator attestation + a valid `bootstrap_proof` → 200; assert a missing/wrong proof → 403, a second finish after a device exists → 403, and the relay reservation (concurrent double-finish → one 200 / one 403). Keep the C2 no-claim_key/no-PIN refusals.
- [ ] docs: `docs/relay-deploy.md` + `docs/remote-access.md` — document the `bootstrap_proof` (HMAC over ceremony_token), the relay Reserve/Burn/Release single-use, the Pi finish-time zero-device recheck, and the new enroll-forward host. Update the `.changeset` summary to note the proof + single-use-before-side-effects and that the relay↔Pi enroll path is now end-to-end (still `-multi-tenant` default OFF).
- [ ] `make verify` green. Commit.

### After F1–F5: re-audit (Codex) + the existing manual gates (PRF device test + Fredrik's validation) before any go-live. `home.fortytwowatts.com` stays 404 throughout.
