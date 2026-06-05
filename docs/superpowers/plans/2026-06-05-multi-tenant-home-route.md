# Multi-tenant Home Route — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Make `home.fortytwowatts.com` a public multi-tenant front door (anonymous -> landing; signed-in wallet -> its own Pi), with a hybrid/encrypted directory (relay-held PRF-encrypted blob + browser-carried source of truth).

**Spec:** `docs/superpowers/specs/2026-06-05-multi-tenant-home-route-design.md`.

**Status (2026-06-05):**
- DONE Task Group 1 (WalletBlobStore) - SHIPPED in v0.118.0 (`go/cmd/ftw-relay/walletblob.go`).
- DONE Task Group 2 (relay multi-tenant mode/routing/endpoints/flags) - SHIPPED in v0.118.0 behind `-multi-tenant` (default OFF). Codex-reviewed.
- TODO Task Groups 3-7 (below) - NEXT slice (Pi descriptor + web prf.js/instance-sync.js/landing + e2e). NOT yet built.

**Hard blockers before flipping `-multi-tenant` ON in production:**
1. WebAuthn-PRF determinism device test on real synced iPhone/Android (manual, needs hardware).
2. Write-authentication on `PUT /wallet/{user_handle}/blob` (Codex HIGH - per-wallet PRF-derived write MAC).

**Cleanup carried into Group 3+ (touches handlers.go anyway):**
- Gate the host-less `/wallet/*` + `/signal/{site_id}/identity` route registration behind `r.MultiTenant` so they return 404 (not 503/pubkey) when the flag is off. Currently inert+safe but live.

---

# TASK GROUP 3 — Pi signed instance descriptor

Branch: `feat/multi-tenant-home-route`
Worktree root: `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5`
Go module: `github.com/frahlg/forty-two-watts/go` (run all `go` commands from `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/go`).

## Goal

Add an owner-authed endpoint served over the P2P/DTLS channel:

```
GET /api/owner-access/instance-descriptor
 -> 200 {"site_id":"site:…","pi_pubkey":"<hex X||Y, 128 chars>","label":"…","sig":"<b64url>"}
```

`sig` is the Pi ES256 signature (raw r||s, base64url **no padding**) over the **exact** UTF-8 string from the CONTRACT:

```
"ftw-instance:v1:" + site_id + ":" + pi_pubkey + ":" + label
```

The browser (TASK GROUP 5, `instance-sync.js` `verifyEntry`) verifies this signature against `pi_pubkey` before trusting a directory entry. This is the trust anchor for the encrypted directory blob.

## What the real code already gives us (verified)

- `*nova.Identity` (`go/internal/nova/identity.go`):
  - `PublicKeyHex() string` → uncompressed P-256 `X||Y`, 128 lowercase hex chars (line 80).
  - `SignRawHex(msg string) (string, error)` → SHA-256(msg), ES256, returns raw `r||s` 64-byte sig as **128-char hex** (line 107). **NOTE: it returns hex, NOT base64url.** The CONTRACT requires `sig` as **b64url**. The handler must re-encode hex→bytes→`base64.RawURLEncoding`. (Verified against the relay's `verifyDevicePoPSig` in `api_owner_device_pop.go`, which decodes a `base64.RawURLEncoding` raw r||s.)
  - Domain-separation note (lines 99–106) requires every caller prefix a unique `ftw-<purpose>:v1:` tag. Our string starts with `ftw-instance:v1:` — compliant and distinct from `ftw-device-pop:v1:` and `ftw-signal:v1:`.
- `api.Deps` (`go/internal/api/api.go` ~line 181) already carries `SiteIdentityPubHex string` (line 185) and `SiteID string` (line 190). It does **not** yet carry a signer, so we add one.
- `main.go` (`go/cmd/forty-two-watts/main.go`) already holds `siteIdentity` (`*nova.Identity`, line 1451) and builds `deps` at line 1481 with `SiteIdentityPubHex` + `SiteID`. We add one field assignment.
- Owner auth: `s.authorizeOwner(r)` (`api_owner_access.go` line 387) returns `(credentialID, ok)`. The P2P-channel handlers (e.g. `handleOwnerDevicePoP`) are reachable because the relay→Pi forward is already owner-authenticated by the channel; for a SESSION-gated owner endpoint we gate with `authorizeOwner` exactly like `handleOwnerWhoami` (line 930).
- Route registration: `s.handle("GET  /api/owner-access/whoami", s.handleOwnerWhoami)` block at `api.go` line 355–367. We append one line.
- `writeJSON(w, status, v)` helper (`api.go` line 391).
- Test helper `minDeps(t)` (`api_owner_access_test.go` line 22) sets `OwnerAccessLANBypass: true`, `Cfg.Site.Name = "test-site"`; `contains(h, n)` (line 513).

### No import cycle / matched pattern

`internal/api` does not import `internal/nova` and `internal/nova` does not import `internal/api` (verified). To keep it that way AND mirror the existing `relaySigner` interface pattern in `cmd/forty-two-watts/owner_relay_register.go` (line 24), we define a tiny **interface** in the api package instead of importing nova. `*nova.Identity` satisfies it structurally.

### Label source

There is no `Site.Label` field in `internal/config` (verified — `Site` struct has `Name`, no `Label`). The human label is `Cfg.Site.Name`. The descriptor `label` = `s.deps.Cfg.Site.Name`. `site_id` stays `s.deps.SiteID` (already `"site:"+Name`).

---

## Task 3.1 — add `InstanceSigner` to `Deps` + the descriptor handler (write failing test first)

### Step 3.1.a — Write the failing test

Create new file `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/go/internal/api/api_owner_instance_descriptor_test.go`:

```go
package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"testing"
)

// fakeInstanceSigner is an in-test ES256 signer mirroring *nova.Identity's
// SignRawHex/PublicKeyHex wire contract (raw r||s as 128-char hex; pubkey as
// uncompressed X||Y, 128 lowercase hex chars). It lets the api package test the
// descriptor handler without importing internal/nova (no cycle, matches the
// relaySigner interface pattern in cmd/forty-two-watts/owner_relay_register.go).
type fakeInstanceSigner struct{ priv *ecdsa.PrivateKey }

func newFakeInstanceSigner(t *testing.T) *fakeInstanceSigner {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	return &fakeInstanceSigner{priv: priv}
}

func (f *fakeInstanceSigner) PublicKeyHex() string {
	x := f.priv.PublicKey.X.Bytes()
	y := f.priv.PublicKey.Y.Bytes()
	buf := make([]byte, 64)
	copy(buf[32-len(x):32], x)
	copy(buf[64-len(y):64], y)
	return hex.EncodeToString(buf)
}

func (f *fakeInstanceSigner) SignRawHex(msg string) (string, error) {
	h := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, f.priv, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	rb, sb := r.Bytes(), s.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):64], sb)
	return hex.EncodeToString(sig), nil
}

// verifyInstanceSig checks a base64url (no padding) raw r||s ECDSA-P256 sig of
// msg against an uncompressed X||Y device public key (128 hex chars). Mirrors
// the browser-side verifyEntry the directory blob relies on.
func verifyInstanceSig(t *testing.T, pubKeyHex, msg, sigB64URL string) bool {
	t.Helper()
	pb, err := hex.DecodeString(pubKeyHex)
	if err != nil || len(pb) != 64 {
		return false
	}
	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(pb[:32]),
		Y:     new(big.Int).SetBytes(pb[32:]),
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64URL)
	if err != nil || len(sig) != 64 {
		return false
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	h := sha256.Sum256([]byte(msg))
	return ecdsa.Verify(pub, h[:], r, s)
}

// The descriptor is owner-authed and signs the EXACT CONTRACT string; the
// signature must verify against pi_pubkey and the JSON fields must match.
func TestInstanceDescriptorSignsContractString(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true // gate inactive in minDeps (TunnelMarker empty), authorizeOwner returns lan-bypass
	signer := newFakeInstanceSigner(t)
	d.InstanceSigner = signer
	d.SiteIdentityPubHex = signer.PublicKeyHex()
	d.SiteID = "site:test-site"
	d.Cfg.Site.Name = "test-site"
	srv := New(d)

	req := httptest.NewRequest("GET", "/api/owner-access/instance-descriptor", nil)
	req.Host = "1.2.3.4"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}

	var out struct {
		SiteID   string `json:"site_id"`
		PiPubkey string `json:"pi_pubkey"`
		Label    string `json:"label"`
		Sig      string `json:"sig"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v body=%q", err, rec.Body.String())
	}
	if out.SiteID != "site:test-site" {
		t.Fatalf("site_id=%q want site:test-site", out.SiteID)
	}
	if out.PiPubkey != signer.PublicKeyHex() || len(out.PiPubkey) != 128 {
		t.Fatalf("pi_pubkey=%q (len %d) want %q", out.PiPubkey, len(out.PiPubkey), signer.PublicKeyHex())
	}
	if out.Label != "test-site" {
		t.Fatalf("label=%q want test-site", out.Label)
	}

	// The signed string MUST be byte-for-byte the CONTRACT format.
	want := "ftw-instance:v1:" + out.SiteID + ":" + out.PiPubkey + ":" + out.Label
	if !verifyInstanceSig(t, out.PiPubkey, want, out.Sig) {
		t.Fatalf("signature does not verify over %q (sig=%q)", want, out.Sig)
	}
	// And sig is base64url (no padding) of a 64-byte raw r||s, never hex.
	if _, err := base64.RawURLEncoding.DecodeString(out.Sig); err != nil {
		t.Fatalf("sig not base64url-no-pad: %q (%v)", out.Sig, err)
	}
}

// Without an owner session AND without LAN-bypass, a remote (tunnelled) caller
// is refused — the descriptor carries the Pi pubkey + a signature and must not
// be an anonymous read over the relay.
func TestInstanceDescriptorRequiresOwner(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = false
	d.TunnelMarker = "marker" // gate active
	signer := newFakeInstanceSigner(t)
	d.InstanceSigner = signer
	d.SiteIdentityPubHex = signer.PublicKeyHex()
	d.SiteID = "site:test-site"
	srv := New(d)

	req := httptest.NewRequest("GET", "/api/owner-access/instance-descriptor", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("X-FTW-Tunnel", "marker") // tunnelled → no LAN bypass, no session → reject
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status=%d want 401 body=%q", rec.Code, rec.Body.String())
	}
}

// When no identity/signer is wired (e.g. identity load failed on boot), the
// endpoint reports 503 rather than serving an unsigned descriptor.
func TestInstanceDescriptor503WhenNoSigner(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.InstanceSigner = nil
	d.SiteIdentityPubHex = ""
	srv := New(d)

	req := httptest.NewRequest("GET", "/api/owner-access/instance-descriptor", nil)
	req.Host = "1.2.3.4"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status=%d want 503 body=%q", rec.Code, rec.Body.String())
	}
}
```

### Step 3.1.b — Run it & see it FAIL with the exact message

Run:

```
go test ./internal/api/ -run TestInstanceDescriptor -count=1
```

Expected output: a **compile failure** (the test names the not-yet-existing `Deps.InstanceSigner` field and route), e.g.:

```
# github.com/frahlg/forty-two-watts/go/internal/api [github.com/frahlg/forty-two-watts/go/internal/api.test]
./api_owner_instance_descriptor_test.go:108:4: d.InstanceSigner undefined (type *Deps has no field or method InstanceSigner)
FAIL	github.com/frahlg/forty-two-watts/go/internal/api [build failed]
```

This is the expected RED state (build failure counts as a failing test in TDD here — the symbol doesn't exist yet).

### Step 3.1.c — Minimal implementation

**(1)** Add the signer interface + `Deps` field. In `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/go/internal/api/api.go`, immediately AFTER the `SiteID` field block (currently ends at line 190, just before the `// P2P is the Pi-side WebRTC manager` comment at line 192), insert:

```go
	// InstanceSigner is the Pi's self-sovereign ES256 identity used to sign the
	// owner-access instance descriptor (GET /api/owner-access/instance-descriptor)
	// over the P2P channel. Satisfied by *nova.Identity — declared as an interface
	// here so internal/api does not import internal/nova (matches the relaySigner
	// pattern in cmd/forty-two-watts/owner_relay_register.go). Nil when identity
	// load failed on boot; the descriptor endpoint then returns 503.
	InstanceSigner InstanceSigner
```

Then, near the top of the same file with the other type declarations — directly ABOVE the `// Deps holds...`/`type Deps struct` declaration — add the interface type. (Place it just before `type Deps struct`; the exact preceding line is the doc comment for `Deps`.) Add:

```go
// InstanceSigner signs the owner-access instance descriptor with the Pi's
// self-sovereign ES256 identity. *nova.Identity satisfies it. PublicKeyHex
// returns the uncompressed P-256 public key (X||Y, 128 lowercase hex chars);
// SignRawHex returns the raw r||s 64-byte signature as a 128-char hex string
// (the handler re-encodes it to base64url for the wire).
type InstanceSigner interface {
	PublicKeyHex() string
	SignRawHex(msg string) (string, error)
}
```

**(2)** Create the handler file `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/go/internal/api/api_owner_instance_descriptor.go`:

```go
// api_owner_instance_descriptor.go
//
// GET /api/owner-access/instance-descriptor — the Pi-signed instance descriptor
// the multi-tenant home route relies on. Served over the owner-authenticated P2P
// (DTLS) channel; NOT an open path. The browser stores the {site_id, pi_pubkey,
// label, sig} tuple inside its encrypted directory blob and verifies sig against
// pi_pubkey (web/owner-access/instance-sync.js verifyEntry) before trusting an
// entry — so even a tampering relay that stores the blob cannot inject a fake
// instance. The signing string is domain-separated ("ftw-instance:v1:") and bound
// to site_id + pi_pubkey + label so a signature minted here can never be replayed
// for another purpose (cf. SignRawHex's domain-separation note in
// internal/nova/identity.go).
package api

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
)

// instanceDescriptorSigningString is the canonical message the Pi signs for the
// instance descriptor. Both ends (Pi here, browser in instance-sync.js) MUST
// build it identically — pinning the format in one place is the entire point.
func instanceDescriptorSigningString(siteID, piPubkey, label string) string {
	return "ftw-instance:v1:" + siteID + ":" + piPubkey + ":" + label
}

// handleOwnerInstanceDescriptor returns the Pi-signed instance descriptor. Owner
// auth required (same posture as whoami): the descriptor is served only over the
// already-owner-authenticated P2P channel, never anonymously over the relay.
func (s *Server) handleOwnerInstanceDescriptor(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeOwner(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.deps.InstanceSigner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "site identity unavailable"})
		return
	}
	piPubkey := s.deps.InstanceSigner.PublicKeyHex()
	siteID := s.deps.SiteID
	label := ""
	if s.deps.Cfg != nil {
		label = s.deps.Cfg.Site.Name
	}
	msg := instanceDescriptorSigningString(siteID, piPubkey, label)
	sigHex, err := s.deps.InstanceSigner.SignRawHex(msg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sign descriptor"})
		return
	}
	// SignRawHex returns raw r||s as hex; the CONTRACT wire form is base64url
	// (no padding) so the browser can verify with WebCrypto. Re-encode.
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "encode descriptor"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site_id":   siteID,
		"pi_pubkey": piPubkey,
		"label":     label,
		"sig":       base64.RawURLEncoding.EncodeToString(sigBytes),
	})
}
```

**(3)** Register the route. In `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/go/internal/api/api.go`, in the `// ---- Owner remote access (Phase 3, WebAuthn passkey) ----` block, after the existing line (line 367):

```go
	s.handle("POST /api/owner-access/device-pop", s.handleOwnerDevicePoP)
```

add:

```go
	// Multi-tenant home route: Pi-signed instance descriptor, owner-authed,
	// served over the P2P channel (see api_owner_instance_descriptor.go).
	s.handle("GET  /api/owner-access/instance-descriptor", s.handleOwnerInstanceDescriptor)
```

### Step 3.1.d — Run it & see it PASS

Run:

```
go test ./internal/api/ -run TestInstanceDescriptor -count=1 -v
```

Expected output:

```
=== RUN   TestInstanceDescriptorSignsContractString
--- PASS: TestInstanceDescriptorSignsContractString (0.00s)
=== RUN   TestInstanceDescriptorRequiresOwner
--- PASS: TestInstanceDescriptorRequiresOwner (0.00s)
=== RUN   TestInstanceDescriptor503WhenNoSigner
--- PASS: TestInstanceDescriptor503WhenNoSigner (0.00s)
PASS
ok  	github.com/frahlg/forty-two-watts/go/internal/api	0.40s
```

Then confirm nothing else in the package regressed:

```
go test ./internal/api/ -count=1
```

Expected: `ok  github.com/frahlg/forty-two-watts/go/internal/api`.

### Step 3.1.e — Commit

```
git add go/internal/api/api.go go/internal/api/api_owner_instance_descriptor.go go/internal/api/api_owner_instance_descriptor_test.go
git commit -m "feat(api): Pi-signed instance descriptor for multi-tenant home route

Add GET /api/owner-access/instance-descriptor (owner-authed, P2P channel) that
returns {site_id, pi_pubkey, label, sig}, sig = Pi ES256 over
\"ftw-instance:v1:\"+site_id+\":\"+pi_pubkey+\":\"+label as base64url raw r||s.
The browser verifies each directory-blob entry against pi_pubkey before trusting
it, so a tampering relay can't inject a fake instance.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3.2 — wire the signer in `main.go` (no new test; covered by `go build` + existing build/vet)

The handler is dormant until `main.go` passes the real `*nova.Identity` into `Deps.InstanceSigner`. This is a one-line wiring change; it has no independent unit test (the api-package tests above already prove behaviour with a fake signer). Verification is `go build` + `make verify`.

### Step 3.2.a — Implement

In `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/go/cmd/forty-two-watts/main.go`, in the `deps = &api.Deps{...}` literal, find the two lines (currently 1527–1528):

```go
		SiteIdentityPubHex:   siteIdentityPubHex,
		SiteID:               "site:" + cfg.Site.Name,
```

Replace with:

```go
		SiteIdentityPubHex:   siteIdentityPubHex,
		SiteID:               "site:" + cfg.Site.Name,
		// InstanceSigner signs the owner-access instance descriptor with the same
		// self-sovereign ES256 key. nil-safe: if identity load failed above,
		// siteIdentity is nil and the descriptor endpoint returns 503.
		InstanceSigner:       instanceSignerOrNil(siteIdentity),
```

`siteIdentity` is a typed `*nova.Identity`; assigning a nil typed pointer directly into the interface field would make the interface non-nil (the classic typed-nil trap), defeating the handler's `== nil` 503 guard. So add a tiny helper at the bottom of `main.go` (after the last function — place it next to other small `func envOr`-style helpers if you prefer):

```go
// instanceSignerOrNil returns id as an api.InstanceSigner, or a genuine nil
// interface when id is nil — so api's `InstanceSigner == nil` 503 guard fires
// (a typed-nil *nova.Identity boxed into the interface would be non-nil).
func instanceSignerOrNil(id *nova.Identity) api.InstanceSigner {
	if id == nil {
		return nil
	}
	return id
}
```

(`nova` and `api` are already imported in main.go — verified via `nova.LoadOrCreateIdentity` at line 1451 and `api.Deps` at line 1481.)

### Step 3.2.b — Verify it compiles and vets

Run:

```
go build ./cmd/forty-two-watts/ && go vet ./cmd/forty-two-watts/ ./internal/api/
```

Expected output: no output (clean build + vet), exit 0.

Then run the full package test suite once more to confirm no regression in either package:

```
go test ./internal/api/ ./cmd/forty-two-watts/ -count=1
```

Expected:

```
ok  	github.com/frahlg/forty-two-watts/go/internal/api
ok  	github.com/frahlg/forty-two-watts/go/cmd/forty-two-watts
```

### Step 3.2.c — Commit

```
git add go/cmd/forty-two-watts/main.go
git commit -m "feat(main): wire site identity into instance-descriptor endpoint

Pass *nova.Identity into api.Deps.InstanceSigner (typed-nil-safe) so
GET /api/owner-access/instance-descriptor serves a real Pi-signed descriptor.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Changeset (required — user-visible new API endpoint)

Per the repo's Changesets workflow (project `CLAUDE.md`), a new API endpoint is a **minor** bump. From the repo root `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5`:

```
npx changeset
```

Pick `minor`, summary e.g. _"Pi-signed instance descriptor (`GET /api/owner-access/instance-descriptor`) for the multi-tenant home route"_. Commit the generated `.changeset/<name>.md` with Task 3.1's commit (or its own commit). Note this group touches Go only (no `web/` change), so the auto-exempt doc paths do not apply — the changeset is mandatory.

---

## Contract conformance checklist (self-audit before marking done)

- [x] Route path EXACTLY `/api/owner-access/instance-descriptor`, method GET.
- [x] Response JSON keys EXACTLY `site_id`, `pi_pubkey`, `label`, `sig`.
- [x] `pi_pubkey` = uncompressed `X||Y`, 128 hex chars (from `PublicKeyHex()`), matches `SiteIdentityPubHex`.
- [x] `sig` over EXACTLY `"ftw-instance:v1:" + site_id + ":" + pi_pubkey + ":" + label` (single source: `instanceDescriptorSigningString`).
- [x] `sig` is base64url **no padding** of the raw 64-byte r||s — re-encoded from `SignRawHex`'s hex output (the one subtle gotcha this group fixes).
- [x] Owner-authed (not open path) — gated by `authorizeOwner`, mirrors `handleOwnerWhoami`; tunnelled-without-session → 401.
- [x] No `internal/api` → `internal/nova` import (interface + typed-nil-safe wiring).
- [x] Test asserts the signature verifies against the pubkey and the string format is byte-for-byte the CONTRACT.

## Notes for downstream task groups

- **TASK GROUP 5** (`web/owner-access/instance-sync.js` `verifyEntry`) MUST rebuild the signing string as `"ftw-instance:v1:" + entry.site_id + ":" + entry.pi_pubkey + ":" + entry.label`, base64url-decode `entry.sig` to 64 raw bytes (r||s), and verify with WebCrypto `subtle.verify("ECDSA",{hash:"SHA-256"}, importedKey, sig, encoder.encode(str))` where the key is imported as a raw P-256 point from the 128-hex `pi_pubkey` (`crypto.subtle.importKey("raw", 0x04||X||Y, {name:"ECDSA",namedCurve:"P-256"}, ...)`). The Pi emits `r||s` (not DER) and base64url — both match WebCrypto's native ECDSA format, so no DER conversion is needed.
- The `pi_pubkey` byte form is lowercase hex from `nova.PublicKeyHex()`; the blob plaintext field name is `pi_pubkey` (matches the CONTRACT BLOB PLAINTEXT schema).

---

# TASK GROUP 4 — `web/owner-access/prf.js` (WebAuthn PRF → HKDF → AES-GCM K_dir)

Branch: `feat/multi-tenant-home-route` (already checked out in the worktree).
All paths are absolute under `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5`.

## Pre-flight facts I verified against the real tree (do not re-derive — use them)

- **Test runner** (from root `package.json`): `node --test 'web/**/*.test.mjs'`. To run one file: `node --test web/owner-access/prf.test.mjs`. Node in this env is **v26.0.0**; `globalThis.crypto.subtle` supports HKDF + AES-GCM natively (confirmed by running it).
- **Existing patterns this module must mirror**:
  - `web/owner-access/device-key.js` — pure ESM, uses `crypto.subtle` directly, exports named functions, has a classic-script bridge (`window.ftwDeviceKey = {...}`) guarded by `try { if (typeof window !== "undefined") … } catch (_) {}`. **prf.js will use the same window-bridge pattern** (`window.ftwPrf`).
  - `web/owner-access/webauthn.js` — already exports `bufToB64url` / `b64urlToBuf`. **Do not** re-implement base64 in prf.js; nothing in prf.js needs it (it deals in ArrayBuffers + CryptoKey only).
  - `web/owner-access/device-key.test.mjs` — sets `globalThis.location` / `globalThis.window` / `globalThis.indexedDB` in a `before()`, imports the module with a cache-busting query (`./device-key.js?fresh=` + `Date.now()`), uses `node:test` (`describe/it/before`) + `node:assert/strict`, and verifies crypto by **re-importing keys and round-tripping** (it imports a pubkey and `crypto.subtle.verify`s). **prf.test.mjs follows this exact style.**
- **CONTRACT (locked — do NOT rename or reshape):**
  - `prfRequested()` → the WebAuthn extension **input** object `{ prf: { eval: { first: FIXED_SALT_32 } } }`.
  - `derivePrfKey(assertion)` reads `assertion.getClientExtensionResults().prf.results.first` (an `ArrayBuffer`, 32 bytes). If missing/absent → **return `null`** (PRF unsupported). Else: `HKDF-SHA256(ikm=prfOut, salt=FIXED_SALT_32, info="ftw-instance-blob:aes-gcm:v1", len=32)` → `importKey` as a **non-extractable AES-GCM-256** `CryptoKey` and return it.
  - `FIXED_SALT_32` — exported `Uint8Array`, length 32.
- **The salt I chose** (a fixed, documented constant; lives in this module as the single source of truth): ASCII `"ftw-instance-blob-salt/v1"` zero-padded to 32 bytes →
  hex `6674772d696e7374616e63652d626c6f622d73616c742f763100000000000000`.
- **Known-Answer Vector I computed with the real WebCrypto** (use verbatim in the test): with `ikm = bytes 0x00..0x1f` (32 bytes), `salt = FIXED_SALT_32`, `info = "ftw-instance-blob:aes-gcm:v1"`, `HKDF-SHA256` 256-bit output =
  `7d3354f97c1470c020412dc320ed52b72ce52c44b9a5dd7a67701c21c7837efc`.
- **KAT verification technique** (the derived key is non-extractable, so we can't read its bytes): the test builds a **reference** AES-GCM key from the KAT hex and proves equality by **cross-decrypt** — encrypt with the key from `derivePrfKey`, decrypt with the reference key, assert plaintext matches (and the reverse). I verified both directions succeed.

---

## Task 4.1 — `prfRequested()` + `FIXED_SALT_32` (the extension request)

### Step 4.1.a — Write the failing test (first two `it`s)

Create `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/owner-access/prf.test.mjs` with the FULL contents below. (This file also contains the Task 4.2 / 4.3 tests — write it once, complete; the early tasks will fail until each export lands.)

```js
// node --test web/owner-access/prf.test.mjs
//
// prf.js turns a WebAuthn login assertion's PRF output into the directory key
// K_dir = HKDF-SHA256(prfOut, FIXED_SALT_32, "ftw-instance-blob:aes-gcm:v1") as a
// NON-EXTRACTABLE AES-GCM-256 key. These tests run the REAL WebCrypto path
// (node's globalThis.crypto.subtle) and lock in:
//   - prfRequested() returns the exact extension-input shape the assertion needs;
//   - FIXED_SALT_32 is a 32-byte Uint8Array (HKDF salt + PRF eval salt);
//   - derivePrfKey() returns a usable AES-GCM key matching a HKDF known-answer
//     vector (proved by cross-decrypt, since the key is non-extractable);
//   - derivePrfKey() returns null when the assertion has no PRF results
//     (PRF unsupported, e.g. Firefox) — the documented degrade-to-local path.

import { describe, it } from "node:test";
import assert from "node:assert/strict";

// The KAT was computed once with this same WebCrypto (node v26) — see prf.js
// header. ikm = bytes 0x00..0x1f, salt = FIXED_SALT_32, info as below.
const KAT_KEY_HEX =
  "7d3354f97c1470c020412dc320ed52b72ce52c44b9a5dd7a67701c21c7837efc";

function hexToBytes(h) {
  const a = new Uint8Array(h.length >> 1);
  for (let i = 0; i < a.length; i++) a[i] = parseInt(h.substr(i * 2, 2), 16);
  return a;
}

// A stand-in for a real PublicKeyCredential assertion: only the surface prf.js
// touches — getClientExtensionResults().prf.results.first (an ArrayBuffer).
function mockAssertion(prfFirstArrayBuffer) {
  return {
    getClientExtensionResults() {
      if (prfFirstArrayBuffer === undefined) return {}; // PRF entirely absent
      if (prfFirstArrayBuffer === null) return { prf: {} }; // present, no results
      return { prf: { results: { first: prfFirstArrayBuffer } } };
    },
  };
}

const prf = await import("./prf.js?fresh=" + Date.now());

describe("prfRequested() + FIXED_SALT_32 (the assertion extension request)", () => {
  it("FIXED_SALT_32 is a 32-byte Uint8Array", () => {
    assert.ok(prf.FIXED_SALT_32 instanceof Uint8Array, "must be a Uint8Array");
    assert.equal(prf.FIXED_SALT_32.length, 32, "PRF/HKDF salt is fixed 32 bytes");
  });

  it("prfRequested() returns the WebAuthn prf eval.first extension input", () => {
    const ext = prf.prfRequested();
    assert.ok(ext && ext.prf && ext.prf.eval, "shape { prf: { eval: { first } } }");
    assert.ok(ext.prf.eval.first instanceof Uint8Array, "eval.first is the salt bytes");
    assert.equal(ext.prf.eval.first.length, 32);
    // Must hand over the SAME fixed salt the deriver uses, not a fresh/random one.
    assert.deepEqual(
      new Uint8Array(ext.prf.eval.first),
      new Uint8Array(prf.FIXED_SALT_32),
      "the requested PRF salt must equal FIXED_SALT_32",
    );
  });
});

describe("derivePrfKey() — HKDF→AES-GCM-256, known-answer + null degrade", () => {
  it("returns null when the assertion has no PRF results (unsupported)", async () => {
    assert.equal(await prf.derivePrfKey(mockAssertion(undefined)), null,
      "no prf extension at all → null (degrade to local-only)");
    assert.equal(await prf.derivePrfKey(mockAssertion(null)), null,
      "prf present but results.first absent → null");
  });

  it("derives an AES-GCM-256 non-extractable key matching the HKDF KAT", async () => {
    // ikm = prfOut = bytes 0x00..0x1f (32 bytes), as an ArrayBuffer.
    const ikm = new Uint8Array(32);
    for (let i = 0; i < 32; i++) ikm[i] = i;
    const key = await prf.derivePrfKey(mockAssertion(ikm.buffer));
    assert.ok(key, "must derive a key for a present 32-byte PRF output");
    assert.equal(key.type, "secret");
    assert.equal(key.algorithm.name, "AES-GCM");
    assert.equal(key.algorithm.length, 256);
    assert.equal(key.extractable, false, "K_dir must be NON-extractable");

    // KAT proof by cross-decrypt: a reference key built straight from the KAT hex
    // must interoperate with derivePrfKey's key. (The key is non-extractable, so we
    // cannot read its bytes — equality is proven cryptographically.)
    const refKey = await crypto.subtle.importKey(
      "raw", hexToBytes(KAT_KEY_HEX),
      { name: "AES-GCM", length: 256 }, false, ["encrypt", "decrypt"],
    );
    const nonce = new Uint8Array(12).fill(5);
    const msg = new TextEncoder().encode("kat-proof");
    const ct = await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce }, key, msg);
    const back = await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce }, refKey, ct);
    assert.equal(new TextDecoder().decode(back), "kat-proof",
      "derived key must equal the HKDF known-answer key (cross-decrypt prod→ref)");
    // Reverse direction too — proves it is exactly the same key, not just compatible.
    const ct2 = await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce }, refKey, msg);
    const back2 = await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce }, key, ct2);
    assert.equal(new TextDecoder().decode(back2), "kat-proof");
  });

  it("accepts a Uint8Array PRF output (not only ArrayBuffer)", async () => {
    // Belt-and-suspenders: some authenticator polyfills hand back a typed array.
    const ikm = new Uint8Array(32);
    for (let i = 0; i < 32; i++) ikm[i] = i;
    const key = await prf.derivePrfKey(mockAssertion(ikm));
    assert.ok(key, "Uint8Array prfOut must also derive a key");
    assert.equal(key.algorithm.name, "AES-GCM");
  });

  it("rejects a wrong-length PRF output by returning null (defensive)", async () => {
    const short = new Uint8Array(16); // not 32 bytes
    assert.equal(await prf.derivePrfKey(mockAssertion(short.buffer)), null,
      "a non-32-byte PRF output is treated as unusable → null");
  });
});

describe("classic-script bridge", () => {
  it("exposes window.ftwPrf for non-module consumers", () => {
    assert.equal(typeof globalThis.window, "object");
    assert.equal(typeof globalThis.window.ftwPrf, "object");
    assert.equal(typeof globalThis.window.ftwPrf.derivePrfKey, "function");
    assert.equal(typeof globalThis.window.ftwPrf.prfRequested, "function");
    assert.ok(globalThis.window.ftwPrf.FIXED_SALT_32 instanceof Uint8Array);
  });
});
```

Note: the test sets nothing in a `before()` — prf.js touches no `location`/IndexedDB. But the bridge test reads `globalThis.window`. Add `globalThis.window = globalThis.window || {};` at the top of the test file (just after the imports, before `const prf = await import(...)`), so the module's window-bridge has somewhere to attach. Insert this line:

```js
globalThis.window = globalThis.window || {};
```

immediately above `const prf = await import("./prf.js?fresh=" + Date.now());`.

### Step 4.1.b — Run it, see it FAIL with the exact message

```
node --test web/owner-access/prf.test.mjs
```

Expected: the run aborts during module load because `./prf.js` does not exist. Output contains:

```
✖ failing tests:
…
Error [ERR_MODULE_NOT_FOUND]: Cannot find module '…/web/owner-access/prf.js'
```

(Node fails the whole file at the dynamic `import()`. That is the expected RED.)

### Step 4.1.c — Minimal implementation: create prf.js with the salt + request only

Create `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/owner-access/prf.js` with this content (full file — `derivePrfKey` is added in 4.2, but to make 4.1's two tests pass we ship `FIXED_SALT_32`, `prfRequested`, and a stub `derivePrfKey` that returns `null`, plus the bridge):

```js
// prf.js — WebAuthn PRF → directory key (K_dir) for the multi-tenant home route.
//
// WHAT THIS IS
// On the login assertion we request the WebAuthn `prf` extension, evaluated at a
// FIXED salt. If the authenticator (and its sync chain) supports PRF, it returns
// a stable 32-byte secret bound to the passkey. We stretch that secret with
// HKDF-SHA256 into a NON-EXTRACTABLE AES-GCM-256 key, K_dir, used to decrypt the
// per-wallet instance-directory blob the relay stores opaquely.
//
// WHY null-ON-MISSING
// PRF is unsupported on some browsers (Firefox today) and some sync paths. The
// design's source of truth is the browser-local directory copy; the relay blob is
// only a fresh-device bootstrap convenience. So when prf.results.first is absent
// we return null and the caller degrades to local-only — never an error, never a
// "lost your homes". (See docs/superpowers/specs/2026-06-05-multi-tenant-home-route-design.md §PRF de-risk.)
//
// CRYPTO CONTRACT (must match instance-sync.js + the spec byte-for-byte):
//   prfOut  = assertion.getClientExtensionResults().prf.results.first  (ArrayBuffer, 32B)
//   K_dir   = HKDF-SHA256( ikm=prfOut, salt=FIXED_SALT_32,
//                          info="ftw-instance-blob:aes-gcm:v1", len=32 )
//           → importKey AES-GCM-256, extractable=false, usages ["encrypt","decrypt"]
//   request = { prf: { eval: { first: FIXED_SALT_32 } } }  (handed to navigator.credentials.get)
//
// KNOWN-ANSWER (locked in prf.test.mjs): ikm=bytes 0x00..0x1f, salt=FIXED_SALT_32,
// info as above → 7d3354f97c1470c020412dc320ed52b72ce52c44b9a5dd7a67701c21c7837efc.
//
// Importable as a module (ESM) AND usable as a classic script: when loaded via
// <script src> it also assigns window.ftwPrf = { derivePrfKey, prfRequested,
// FIXED_SALT_32 }. The owner-access ESM pages import it directly.

// FIXED_SALT_32 — the single fixed salt used for BOTH the PRF eval input and the
// HKDF salt. ASCII "ftw-instance-blob-salt/v1" zero-padded to 32 bytes. It must
// never change without a versioned migration (it would re-key every blob).
export const FIXED_SALT_32 = (() => {
  const salt = new Uint8Array(32);
  const label = "ftw-instance-blob-salt/v1"; // 25 bytes; remaining 7 bytes stay 0x00
  for (let i = 0; i < label.length; i++) salt[i] = label.charCodeAt(i);
  return salt;
})();

const HKDF_INFO = new TextEncoder().encode("ftw-instance-blob:aes-gcm:v1");

// prfRequested() → the extension-input object to merge into the login assertion's
// publicKey.extensions. navigator.credentials.get(...) reads { prf: { eval: { first }}}
// and the authenticator returns the evaluated PRF in getClientExtensionResults().
export function prfRequested() {
  return { prf: { eval: { first: FIXED_SALT_32 } } };
}

// derivePrfKey(assertion) → CryptoKey (AES-GCM-256, non-extractable) | null.
// null means PRF is unavailable/unusable; the caller degrades to local-only.
export async function derivePrfKey(assertion) {
  // (filled in Task 4.2)
  return null;
}

// Classic-script bridge for non-module consumers (next-app.js / p2p.js IIFEs).
try {
  if (typeof window !== "undefined") {
    window.ftwPrf = { derivePrfKey, prfRequested, FIXED_SALT_32 };
  }
} catch (_) {
  /* non-browser (test harness) — ignore */
}
```

### Step 4.1.d — Run, see 4.1 tests PASS

```
node --test web/owner-access/prf.test.mjs
```

Expected: the two `prfRequested() + FIXED_SALT_32` tests **pass**; the bridge test passes. The four `derivePrfKey` tests under "HKDF→AES-GCM-256" still fail (the KAT test fails because the stub returns `null`; the null-degrade test happens to pass since the stub returns `null`). Confirm the two salt/request tests + the bridge test report `pass`. You will see something like:

```
✔ prfRequested() + FIXED_SALT_32 (the assertion extension request) > FIXED_SALT_32 is a 32-byte Uint8Array
✔ prfRequested() + FIXED_SALT_32 (the assertion extension request) > prfRequested() returns the WebAuthn prf eval.first extension input
✔ derivePrfKey() … > returns null when the assertion has no PRF results (unsupported)
✖ derivePrfKey() … > derives an AES-GCM-256 non-extractable key matching the HKDF KAT
✖ derivePrfKey() … > accepts a Uint8Array PRF output (not only ArrayBuffer)
✔ derivePrfKey() … > rejects a wrong-length PRF output by returning null (defensive)
✔ classic-script bridge > exposes window.ftwPrf for non-module consumers
```

### Step 4.1.e — Commit

```
git add web/owner-access/prf.js web/owner-access/prf.test.mjs
git commit -m "feat(web): prf.js scaffold — FIXED_SALT_32 + prfRequested() extension input

prfRequested() returns the WebAuthn { prf: { eval: { first } } } extension input
keyed to the fixed 32-byte salt; derivePrfKey is a null stub pending HKDF wiring.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4.2 — `derivePrfKey()` HKDF→AES-GCM, known-answer + Uint8Array input

### Step 4.2.a — Failing test

Already written in 4.1.a (the "derives an AES-GCM-256 non-extractable key matching the HKDF KAT" and "accepts a Uint8Array PRF output" tests). No new test file needed.

### Step 4.2.b — Run, see it FAIL with the exact message

```
node --test web/owner-access/prf.test.mjs
```

Expected RED on the KAT test. The assertion that trips is:

```
✖ derives an AES-GCM-256 non-extractable key matching the HKDF KAT
  AssertionError [ERR_ASSERTION]: must derive a key for a present 32-byte PRF output

  assert.ok(key, "must derive a key for a present 32-byte PRF output")
```

(`key` is `null` from the stub, so `assert.ok(key, …)` throws with that message. The "accepts a Uint8Array PRF output" test fails the same way.)

### Step 4.2.c — Minimal implementation: replace the `derivePrfKey` stub

Edit `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/owner-access/prf.js`. Replace exactly:

```js
// derivePrfKey(assertion) → CryptoKey (AES-GCM-256, non-extractable) | null.
// null means PRF is unavailable/unusable; the caller degrades to local-only.
export async function derivePrfKey(assertion) {
  // (filled in Task 4.2)
  return null;
}
```

with:

```js
// prfOutputOf pulls the 32-byte PRF result out of an assertion, or null if PRF
// was not evaluated (unsupported browser / no results). Accepts both ArrayBuffer
// and TypedArray (some polyfills hand back a Uint8Array).
function prfOutputOf(assertion) {
  let ext;
  try {
    ext = assertion && typeof assertion.getClientExtensionResults === "function"
      ? assertion.getClientExtensionResults()
      : null;
  } catch (_) {
    return null;
  }
  const first = ext && ext.prf && ext.prf.results ? ext.prf.results.first : undefined;
  if (first == null) return null;
  let bytes;
  if (first instanceof ArrayBuffer) bytes = new Uint8Array(first);
  else if (ArrayBuffer.isView(first)) bytes = new Uint8Array(first.buffer, first.byteOffset, first.byteLength);
  else return null;
  // WebAuthn PRF "first" is a 32-byte secret. A different length is off-contract
  // and unusable as our HKDF ikm — degrade to local-only rather than key off junk.
  if (bytes.length !== 32) return null;
  return bytes;
}

// derivePrfKey(assertion) → CryptoKey (AES-GCM-256, non-extractable) | null.
// null means PRF is unavailable/unusable; the caller degrades to local-only.
export async function derivePrfKey(assertion) {
  const prfOut = prfOutputOf(assertion);
  if (!prfOut) return null;
  // HKDF: import the PRF secret as ikm, derive 256 raw bits, then import those as
  // a non-extractable AES-GCM key. We go via deriveBits + importKey (rather than
  // deriveKey) so the result is plainly a raw-imported AES-GCM-256 secret key,
  // matching instance-sync.js's expectations and the KAT in prf.test.mjs.
  const ikm = await crypto.subtle.importKey("raw", prfOut, "HKDF", false, ["deriveBits"]);
  const rawKey = await crypto.subtle.deriveBits(
    { name: "HKDF", hash: "SHA-256", salt: FIXED_SALT_32, info: HKDF_INFO },
    ikm,
    256,
  );
  return crypto.subtle.importKey(
    "raw",
    rawKey,
    { name: "AES-GCM", length: 256 },
    false, // NON-extractable: K_dir bytes never leave the browser key store
    ["encrypt", "decrypt"],
  );
}
```

### Step 4.2.d — Run, see all tests PASS

```
node --test web/owner-access/prf.test.mjs
```

Expected: every test passes. Summary line:

```
# tests 8
# pass 8
# fail 0
```

(8 = 2 salt/request + 4 derivePrfKey + 1 bridge + … recount: FIXED_SALT_32, prfRequested, null-degrade, KAT, Uint8Array-input, wrong-length-null, bridge = 7 `it`s; `node --test` counts each `it`. Confirm `# fail 0` regardless of the exact total.)

### Step 4.2.e — Commit

```
git add web/owner-access/prf.js
git commit -m "feat(web): derivePrfKey — HKDF-SHA256(prfOut)→non-extractable AES-GCM-256 K_dir

Pulls the 32-byte WebAuthn PRF result, stretches it with HKDF (FIXED_SALT_32,
info ftw-instance-blob:aes-gcm:v1) and imports a non-extractable AES-GCM-256 key.
Returns null on absent/short PRF output so the caller degrades to local-only.
Locked against a known-answer vector via cross-decrypt in prf.test.mjs.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4.3 — Full-suite green + lint hygiene

### Step 4.3.a — Run the whole web suite (no regressions)

```
node --test 'web/**/*.test.mjs'
```

Expected: all web tests pass, including the new `prf.test.mjs`. Confirm `# fail 0` and that `prf.test.mjs` appears in the run. This is the same command the repo's `npm test` runs (root `package.json` → `"test": "node --test 'web/**/*.test.mjs'"`), so a green here is what CI sees.

### Step 4.3.b — Confirm no accidental cross-file coupling

The new module imports nothing from the rest of `web/` (it only uses `crypto.subtle` + `TextEncoder`). Verify there is no stray import:

```
grep -n "^import\|require(" web/owner-access/prf.js
```

Expected: **no output** (prf.js has zero imports — by design, so it loads in the classic-script bridge context too). If anything prints, remove it; prf.js must stay dependency-free like the early lines of `device-key.js` aside from its one `webauthn.js` import (prf.js needs none).

### Step 4.3.c — Commit (only if 4.3 surfaced a fix; otherwise skip)

No code change is expected in 4.3; it is a verification gate. If the grep or the full suite forced an edit, commit it with:

```
git add web/owner-access/prf.js web/owner-access/prf.test.mjs
git commit -m "test(web): prf.js — full web suite green, dependency-free module

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Hand-off notes for downstream task groups (Task Group consuming prf.js)

- **`instance-sync.js`** (separate task group) imports `derivePrfKey`, `prfRequested`, `FIXED_SALT_32` from `./prf.js`. It uses `derivePrfKey(assertion)` → `kDir` and passes `kDir` to `loadDirectory`/`saveDirectory`. prf.js owns the salt + info constants; instance-sync.js must NOT redefine them — it AES-GCM encrypts/decrypts with the `CryptoKey` prf.js returns (random 12-byte nonce per the contract).
- **The login ceremony** (`login.html` / `next-app.js`, separate group) must merge `prfRequested()` into the assertion's `publicKey.extensions` before `navigator.credentials.get`, then call `derivePrfKey(credential)` on the result. A `null` return is the documented "encrypted home sync isn't available on this browser" branch — surface it; do not treat it as an error.
- **`FIXED_SALT_32` is the same salt for two roles**: the PRF eval input AND the HKDF salt. That is intentional and load-bearing; changing it re-keys every stored blob.

---

# Task Group 5 — `web/owner-access/instance-sync.js`

Branch `feat/multi-tenant-home-route`. Worktree root: `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5`.

This module is the browser-side directory manager. The browser-carried IndexedDB copy is the **source of truth**; the relay holds only an opaque ciphertext mirror. Every step is strict TDD: write the failing test, run it and read the exact failure, write the minimal implementation, run it green, commit.

## Contracts this group implements (verbatim from SHARED CONTRACTS — do NOT deviate)

**Exports** (`web/owner-access/instance-sync.js`):
- `export async function loadDirectory(userHandleB64u, kDir, relayBase) -> instances[]`
- `export async function saveDirectory(userHandleB64u, kDir, relayBase, instances) -> void`
- `export async function verifyEntry(entry) -> bool`
- `export function getCachedInstances() -> instances[]`

**Relay endpoints** (`relayBase` is a URL prefix, e.g. `""` for the home host, or an absolute origin in tests):
- `GET  {relayBase}/wallet/{user_handle}/blob` → `200 {"ciphertext":"<b64std>","nonce":"<b64std>","version":<int>}` | `404`
- `PUT  {relayBase}/wallet/{user_handle}/blob` body `{"ciphertext":"<b64std>","nonce":"<b64std>","version":<int>}` → `200` | `409` (version<=stored) | `413` | `400`

> `ciphertext` and `nonce` are **base64 std** (`base64.StdEncoding`, matches p2p.js `b64encode`/`b64decode` at `web/p2p.js:178-188` — `btoa`/`atob`, no url-safe swap, with padding).

**Blob plaintext** (browser-only; relay NEVER parses it):
```json
{ "v":1, "instances":[ {"site_id":"site:…","pi_pubkey":"<hex X||Y>","label":"Home","sig":"<b64url>","added_ms":<int>} ] }
```
`instance.sig` = Pi ES256 (raw r||s, **b64url**) over the UTF-8 string:
`"ftw-instance:v1:" + site_id + ":" + pi_pubkey + ":" + label`

**Crypto**: `kDir` is a **non-extractable AES-GCM-256 `CryptoKey`** (produced by `prf.js`, Task Group 4). Encrypt with a **random 12-byte nonce**. Decrypt with the stored nonce. ES256 verification re-imports `pi_pubkey` (128-hex X||Y) into an `ECDSA P-256` verify key and checks raw r||s (64 bytes) over SHA-256 — exactly the inverse of `device-key.js` sign and mirroring `p2p.js` `importP256Pub` (`web/p2p.js:216-225`) + `verify` (`web/p2p.js:274`).

**v1 invariant**: directory is a LIST but holds exactly 1 entry (single-instance-per-wallet). Picker deferred.

**Degrade, never throw away**: a decrypt / PRF / network failure returns the cached/local copy. The IndexedDB copy is canonical.

**Merge rule (`saveDirectory` 409 retry)**: re-GET, decrypt, union-by-`site_id`, **newest-wins** (`added_ms` higher wins), re-PUT with `stored.version + 1`. Retry **once**.

## Patterns to follow (read these first, already read during planning)
- `web/owner-access/device-key.test.mjs` — the in-memory IndexedDB shim, `before`/`beforeeach`, `import("./mod.js?fresh=" + Date.now())` cache-busting, `globalThis.location`/`window`/`indexedDB` setup, the real `crypto.subtle` ECDSA round-trip. **Copy this shim verbatim.**
- `web/owner-access/device-key.js` — IndexedDB promise plumbing (`openDB`/`idbGet`/`idbPut`), `rawXYToHex`, the `window.ftw*` classic-script bridge at the bottom. **Match this module's style** (ESM with a `try { window... }` bridge, `slog`-free, errors returned/handled not thrown across the public surface where degrade applies).
- `web/p2p.js:178-188` (`b64encode`/`b64decode`), `web/p2p.js:204-225` (`hexToBytes`, `SPKI_P256_PREFIX`, `importP256Pub`), `web/p2p.js:262-280` (verify pattern). **Reuse these byte conventions exactly.**

## Run command (same as `package.json` `test` script, scoped to this file)
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && node --test 'web/owner-access/instance-sync.test.mjs'
```
Confirmed working: Node `v26.0.0`, `globalThis.crypto.subtle` is an object, the existing `device-key.test.mjs` passes 7/7 with the same runner.

---

## Step 0 — branch + scaffold the test file (RED: module does not exist)

Create the branch if not already on it:
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && git rev-parse --abbrev-ref HEAD
# if not feat/multi-tenant-home-route:
git checkout -b feat/multi-tenant-home-route 2>/dev/null || git checkout feat/multi-tenant-home-route
```

### 0a. Write `web/owner-access/instance-sync.test.mjs` — shared harness + first failing test

Create the file with the full harness (IndexedDB shim copied from `device-key.test.mjs`, a `mintPiIdentity` helper that produces a real ES256 keypair + a `makeEntry` that signs the canonical string with it, a `fetchStub` that simulates the relay blob store, and an `encryptDir` helper using real `crypto.subtle`). This is the complete file — every later step appends `describe` blocks to it, but write the whole thing now and add tests incrementally is fine; here we write the harness + the happy-path test.

```js
// node --test web/owner-access/instance-sync.test.mjs
//
// instance-sync.js manages the per-wallet directory of home instances. The
// IndexedDB copy is the SOURCE OF TRUTH; the relay holds an opaque AES-GCM
// ciphertext mirror keyed by userHandle. These tests exercise the REAL
// WebCrypto path (HKDF/AES-GCM via the kDir CryptoKey, ECDSA P-256 verify of
// each entry's Pi signature) against an in-memory IndexedDB shim and a fetch
// stub that simulates the relay /wallet/{W}/blob store.
//
// Contracts locked in here:
//   - blob plaintext { v:1, instances:[{site_id, pi_pubkey, label, sig, added_ms}] }
//   - sig = Pi ES256 raw r||s (b64url) over
//       "ftw-instance:v1:" + site_id + ":" + pi_pubkey + ":" + label
//   - relay blob ciphertext/nonce are base64 STD (matches Go base64.StdEncoding)
//   - degrade-not-discard: a decrypt/PRF/network failure returns the cached copy

import { describe, it, before, beforeEach } from "node:test";
import assert from "node:assert/strict";

// --- in-memory IndexedDB shim (verbatim from device-key.test.mjs) ----------
function makeIndexedDBShim() {
  const data = new Map();
  function reqFire(req, fn) {
    queueMicrotask(() => {
      try {
        req.result = fn();
        if (req.onsuccess) req.onsuccess();
      } catch (e) {
        req.error = e;
        if (req.onerror) req.onerror();
      }
    });
  }
  return {
    _data: data,
    open() {
      const db = {
        objectStoreNames: { contains: (n) => data.has(n) },
        createObjectStore: (n) => { if (!data.has(n)) data.set(n, new Map()); },
        transaction: (name) => ({
          objectStore: (n) => ({
            get(key) { const r = {}; reqFire(r, () => data.get(n)?.get(key) ?? undefined); return r; },
            put(value, key) { const r = {}; reqFire(r, () => { if (!data.has(n)) data.set(n, new Map()); data.get(n).set(key, value); return true; }); return r; },
          }),
        }),
      };
      const req = { result: db };
      queueMicrotask(() => {
        if (req.onupgradeneeded) req.onupgradeneeded();
        if (req.onsuccess) req.onsuccess();
      });
      return req;
    },
  };
}

// --- byte helpers (mirror webauthn.js / p2p.js byte conventions) -----------
function bufToB64url(buf) {
  const bytes = new Uint8Array(buf);
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
function b64stdEncode(buf) {
  const bytes = new Uint8Array(buf);
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}
function b64stdDecode(s) {
  const bin = atob(s), out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}
function rawXYToHex(rawBuf) {
  const b = new Uint8Array(rawBuf);
  let hex = "";
  for (let i = 1; i < b.length; i++) hex += (b[i] + 0x100).toString(16).slice(1);
  return hex;
}

// --- mint a real Pi ES256 identity + sign canonical entries ----------------
async function mintPiIdentity() {
  const kp = await crypto.subtle.generateKey(
    { name: "ECDSA", namedCurve: "P-256" }, true, ["sign", "verify"]);
  const raw = await crypto.subtle.exportKey("raw", kp.publicKey);
  return { kp, pubHex: rawXYToHex(raw) };
}
function instanceMsg(site_id, pi_pubkey, label) {
  return "ftw-instance:v1:" + site_id + ":" + pi_pubkey + ":" + label;
}
async function makeEntry(pi, site_id, label, added_ms) {
  const msg = instanceMsg(site_id, pi.pubHex, label);
  const sig = await crypto.subtle.sign(
    { name: "ECDSA", hash: "SHA-256" }, pi.kp.privateKey,
    new TextEncoder().encode(msg));
  return { site_id, pi_pubkey: pi.pubHex, label, sig: bufToB64url(sig), added_ms };
}

// --- AES-GCM helpers using a real non-extractable kDir ---------------------
async function makeKDir() {
  // Stand-in for prf.js's HKDF output: a real non-extractable AES-GCM-256 key.
  return crypto.subtle.generateKey({ name: "AES-GCM", length: 256 }, false, ["encrypt", "decrypt"]);
}
async function encryptDir(kDir, plaintextObj) {
  const nonce = crypto.getRandomValues(new Uint8Array(12));
  const pt = new TextEncoder().encode(JSON.stringify(plaintextObj));
  const ct = await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce }, kDir, pt);
  return { ciphertext: b64stdEncode(ct), nonce: b64stdEncode(nonce) };
}

// --- relay blob store fetch stub -------------------------------------------
// Simulates GET/PUT /wallet/{W}/blob with version-conflict (409) semantics.
function makeRelay() {
  const store = new Map(); // W -> {ciphertext, nonce, version}
  const calls = [];
  function handler(url, opts) {
    opts = opts || {};
    const m = /\/wallet\/([^/]+)\/blob$/.exec(url);
    const W = m ? decodeURIComponent(m[1]) : null;
    const method = (opts.method || "GET").toUpperCase();
    calls.push({ url, method, W, body: opts.body });
    if (method === "GET") {
      const rec = store.get(W);
      if (!rec) return Promise.resolve({ ok: false, status: 404, json: () => Promise.reject(new Error("no body")) });
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({ ...rec }) });
    }
    if (method === "PUT") {
      const body = JSON.parse(opts.body);
      const cur = store.get(W);
      if (cur && body.version <= cur.version) {
        return Promise.resolve({ ok: false, status: 409, json: () => Promise.resolve({ error: "version conflict" }) });
      }
      store.set(W, { ciphertext: body.ciphertext, nonce: body.nonce, version: body.version });
      return Promise.resolve({ ok: true, status: 200, json: () => Promise.resolve({}) });
    }
    return Promise.resolve({ ok: false, status: 405 });
  }
  return { store, calls, handler };
}

const W = "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVowMTIzNDU2Nzg5LV8"; // 51-char b64url, valid handle length

let sync;
let relay;

before(() => {
  globalThis.location = { origin: "https://home.fortytwowatts.com", pathname: "/" };
  globalThis.window = globalThis.window || {};
});

beforeEach(async () => {
  globalThis.indexedDB = makeIndexedDBShim();
  relay = makeRelay();
  globalThis.fetch = (url, opts) => relay.handler(url, opts);
  sync = await import("./instance-sync.js?fresh=" + Date.now());
});

describe("loadDirectory — decrypt + verify + cache happy path", () => {
  it("GETs the blob, decrypts with kDir, verifies the Pi signature, returns + caches the instance", async () => {
    const pi = await mintPiIdentity();
    const entry = await makeEntry(pi, "site:Home", "Home", 1000);
    const kDir = await makeKDir();
    const enc = await encryptDir(kDir, { v: 1, instances: [entry] });
    relay.store.set(W, { ciphertext: enc.ciphertext, nonce: enc.nonce, version: 3 });

    const got = await sync.loadDirectory(W, kDir, "");
    assert.equal(got.length, 1, "one verified instance");
    assert.equal(got[0].site_id, "site:Home");
    assert.equal(got[0].pi_pubkey, pi.pubHex);
    assert.equal(got[0].label, "Home");

    // cached in IndexedDB → getCachedInstances returns the same after a fresh import
    const fresh = await import("./instance-sync.js?fresh=" + Date.now() + "x");
    // getCachedInstances reads the in-memory cache; loadDirectory must have cached.
    assert.equal(sync.getCachedInstances().length, 1, "loadDirectory caches in memory");
  });
});
```

### 0b. Run it — expect RED (module not found)
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && node --test 'web/owner-access/instance-sync.test.mjs'
```
**Expected failure (exact):** an `ERR_MODULE_NOT_FOUND` thrown from the `import("./instance-sync.js?fresh=...")` in `beforeEach`, surfacing as:
```
Cannot find module '.../web/owner-access/instance-sync.js' imported from .../instance-sync.test.mjs
```
and `ℹ fail 1` (the happy-path test errors in setup). This is the correct RED — the module does not exist yet.

### 0c. Commit the failing test
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && git add web/owner-access/instance-sync.test.mjs && git commit -m "test(web): instance-sync loadDirectory happy-path (RED)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Step 1 — minimal `instance-sync.js`: `verifyEntry` + `loadDirectory` happy path + in-memory + IndexedDB cache (GREEN)

### 1a. Create `web/owner-access/instance-sync.js` with the complete implementation needed for Step 0's test

```js
// instance-sync.js — per-wallet home directory manager (multi-tenant home route).
//
// WHAT THIS IS
// The browser-carried directory of the owner's home instances. Each instance is
// {site_id, pi_pubkey, label, sig, added_ms}; the canonical copy lives in
// IndexedDB (SOURCE OF TRUTH). The relay mirrors it as an OPAQUE AES-GCM
// ciphertext keyed by userHandle — the relay never parses the plaintext.
//
// FLOW (login):
//   1. derive kDir (prf.js — passed in, a non-extractable AES-GCM-256 CryptoKey)
//   2. GET  {relayBase}/wallet/{W}/blob  -> {ciphertext, nonce, version} | 404
//   3. AES-GCM-decrypt with kDir -> { v:1, instances:[...] }
//   4. verify each entry's Pi ES256 signature over
//        "ftw-instance:v1:"+site_id+":"+pi_pubkey+":"+label
//   5. merge with the local copy (union by site_id, newest added_ms wins)
//   6. cache (memory + IndexedDB) and return the verified instances
//
// DEGRADE, NEVER DISCARD: a 404 / network error / decrypt failure / bad PRF
// returns the cached (or local) copy. A PRF failure means "can't bootstrap a
// brand-new browser," never "lost your homes."
//
// WIRE FORMATS (must match the relay + Pi byte-for-byte):
//   - ciphertext/nonce on the wire: base64 STD (Go base64.StdEncoding) — btoa/atob.
//   - entry.sig: Pi ES256 raw r||s (64 bytes), base64url, no padding.
//   - pi_pubkey: uncompressed P-256 X||Y, 128 lowercase hex chars, NO 0x04 prefix.

const DB_NAME = "ftw-instance-sync";
const STORE = "dir";
const RECORD_KEY = "directory"; // single canonical directory record

// ---- base64 std (matches Go base64.StdEncoding, same as p2p.js) -----------
function b64stdEncode(bytes) {
  let bin = "";
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i]);
  return btoa(bin);
}
function b64stdDecode(s) {
  const bin = atob(s), out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}
// ---- base64url -> bytes (entry.sig) ----------------------------------------
function b64urlToBytes(s) {
  const pad = "=".repeat((4 - (s.length % 4)) % 4);
  const b64 = (s + pad).replace(/-/g, "+").replace(/_/g, "/");
  const bin = atob(b64), out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}
function hexToBytes(h) {
  const a = new Uint8Array(h.length >> 1);
  for (let i = 0; i < a.length; i++) a[i] = parseInt(h.substr(i * 2, 2), 16);
  return a;
}

// ---- ES256 verify of a directory entry (mirror of p2p.js importP256Pub) ----
const SPKI_P256_PREFIX = new Uint8Array([
  0x30, 0x59, 0x30, 0x13, 0x06, 0x07, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x02, 0x01,
  0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x03, 0x01, 0x07, 0x03, 0x42, 0x00,
]);
function importP256Pub(xyHex) {
  const xy = hexToBytes(xyHex);
  if (xy.length !== 64) return Promise.reject(new Error("bad pubkey length"));
  const spki = new Uint8Array(SPKI_P256_PREFIX.length + 65);
  spki.set(SPKI_P256_PREFIX, 0);
  spki[SPKI_P256_PREFIX.length] = 0x04;
  spki.set(xy, SPKI_P256_PREFIX.length + 1);
  return crypto.subtle.importKey("spki", spki.buffer,
    { name: "ECDSA", namedCurve: "P-256" }, false, ["verify"]);
}

// verifyEntry returns true iff the entry's Pi ES256 signature is valid over the
// canonical string. Any malformed field / bad signature / crypto error -> false
// (never throws — a bad entry is dropped, not fatal).
export async function verifyEntry(entry) {
  try {
    if (!entry || typeof entry.site_id !== "string" ||
        typeof entry.pi_pubkey !== "string" || typeof entry.label !== "string" ||
        typeof entry.sig !== "string") return false;
    if (!/^[0-9a-f]{128}$/.test(entry.pi_pubkey)) return false;
    const sig = b64urlToBytes(entry.sig);
    if (sig.length !== 64) return false;
    const key = await importP256Pub(entry.pi_pubkey);
    const msg = "ftw-instance:v1:" + entry.site_id + ":" + entry.pi_pubkey + ":" + entry.label;
    return await crypto.subtle.verify(
      { name: "ECDSA", hash: "SHA-256" }, key, sig, new TextEncoder().encode(msg));
  } catch (_) {
    return false;
  }
}

// ---- IndexedDB plumbing (promise-wrapped; mirrors device-key.js) -----------
function openDB() {
  return new Promise((resolve, reject) => {
    let req;
    try { req = indexedDB.open(DB_NAME, 1); } catch (e) { reject(e); return; }
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains(STORE)) db.createObjectStore(STORE);
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error || new Error("indexedDB open failed"));
  });
}
function idbGet(key) {
  return openDB().then((db) => new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, "readonly");
    const req = tx.objectStore(STORE).get(key);
    req.onsuccess = () => resolve(req.result || null);
    req.onerror = () => reject(req.error || new Error("idb get failed"));
  }));
}
function idbPut(key, value) {
  return openDB().then((db) => new Promise((resolve, reject) => {
    const tx = db.transaction(STORE, "readwrite");
    const req = tx.objectStore(STORE).put(value, key);
    req.onsuccess = () => resolve(true);
    req.onerror = () => reject(req.error || new Error("idb put failed"));
  }));
}

// ---- in-memory cache (source-of-truth mirror for getCachedInstances) -------
let _cache = []; // last known good instances[]

// getCachedInstances returns the last verified directory held in memory. Sync,
// no I/O — the gate reads it to render immediately while a load is in flight.
export function getCachedInstances() {
  return _cache.slice();
}

async function readLocal() {
  try {
    const rec = await idbGet(RECORD_KEY);
    if (rec && Array.isArray(rec.instances)) return rec.instances;
  } catch (_) {}
  return [];
}
async function writeLocal(instances) {
  _cache = instances.slice();
  try { await idbPut(RECORD_KEY, { instances }); } catch (_) {}
}

// mergeUnion unions two instance lists by site_id, newest added_ms wins.
function mergeUnion(a, b) {
  const by = new Map();
  for (const e of a || []) by.set(e.site_id, e);
  for (const e of b || []) {
    const cur = by.get(e.site_id);
    if (!cur || (e.added_ms || 0) > (cur.added_ms || 0)) by.set(e.site_id, e);
  }
  return [...by.values()];
}

function walletBlobURL(relayBase, W) {
  return (relayBase || "") + "/wallet/" + encodeURIComponent(W) + "/blob";
}

// fetchBlob GETs the relay ciphertext. Returns {ciphertext, nonce, version} or
// null on 404 / network error / malformed body (caller degrades to local).
async function fetchBlob(relayBase, W) {
  try {
    const r = await fetch(walletBlobURL(relayBase, W), { method: "GET" });
    if (r.status === 404 || !r.ok) return null;
    const body = await r.json();
    if (typeof body.ciphertext !== "string" || typeof body.nonce !== "string" ||
        typeof body.version !== "number") return null;
    return body;
  } catch (_) {
    return null;
  }
}

// decryptBlob AES-GCM-decrypts {ciphertext,nonce} with kDir -> { v, instances }.
// Returns null on any decrypt / JSON / shape failure (degrade, never throw).
async function decryptBlob(kDir, blob) {
  try {
    const ct = b64stdDecode(blob.ciphertext);
    const nonce = b64stdDecode(blob.nonce);
    const pt = await crypto.subtle.decrypt({ name: "AES-GCM", iv: nonce }, kDir, ct);
    const obj = JSON.parse(new TextDecoder().decode(pt));
    if (!obj || obj.v !== 1 || !Array.isArray(obj.instances)) return null;
    return obj;
  } catch (_) {
    return null;
  }
}

// verifyAll keeps only the entries with a valid Pi signature.
async function verifyAll(instances) {
  const out = [];
  for (const e of instances || []) {
    if (await verifyEntry(e)) out.push(e);
  }
  return out;
}

// loadDirectory: GET blob -> decrypt -> verify -> merge with local -> cache.
// DEGRADE: a missing blob / bad PRF / decrypt failure returns the local copy.
// kDir may be null/undefined (no PRF on this browser) -> skip the relay, use
// the browser-carried copy only.
export async function loadDirectory(userHandleB64u, kDir, relayBase) {
  const local = await readLocal();
  if (!kDir) {
    _cache = local.slice();
    return getCachedInstances();
  }
  const blob = await fetchBlob(relayBase, userHandleB64u);
  if (!blob) {
    _cache = local.slice();
    return getCachedInstances();
  }
  const dec = await decryptBlob(kDir, blob);
  if (!dec) {
    _cache = local.slice();
    return getCachedInstances();
  }
  const remote = await verifyAll(dec.instances);
  const merged = mergeUnion(local, remote);
  await writeLocal(merged);
  return getCachedInstances();
}

// Classic-script bridge (parity with device-key.js) for any non-module consumer.
try {
  if (typeof window !== "undefined") {
    window.ftwInstanceSync = { loadDirectory, saveDirectory: undefined, verifyEntry, getCachedInstances };
  }
} catch (_) { /* non-browser (test harness) — ignore */ }
```

> Note: `saveDirectory` is referenced in the window bridge as `undefined` for now; Step 2 fills it in and the bridge line is updated then. (Keeping the bridge present from the start mirrors `device-key.js`.)

### 1b. Run — expect GREEN
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && node --test 'web/owner-access/instance-sync.test.mjs'
```
**Expected output (exact tail):**
```
✔ loadDirectory — decrypt + verify + cache happy path
ℹ tests 1
ℹ pass 1
ℹ fail 0
```

### 1c. Commit
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && git add web/owner-access/instance-sync.js web/owner-access/instance-sync.test.mjs && git commit -m "feat(web): instance-sync loadDirectory + verifyEntry + cache (GREEN)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Step 2 — `saveDirectory`: encrypt + PUT version+1 (RED → GREEN)

### 2a. Append the failing test to `instance-sync.test.mjs`

Append after the existing `describe`:

```js
describe("saveDirectory — re-encrypt + PUT version+1", () => {
  it("encrypts the instances and PUTs them at stored.version+1; relay round-trips back", async () => {
    const pi = await mintPiIdentity();
    const entry = await makeEntry(pi, "site:Home", "Home", 1000);
    const kDir = await makeKDir();

    // Pre-seed the relay with version 5 so saveDirectory must PUT version 6.
    const seed = await encryptDir(kDir, { v: 1, instances: [] });
    relay.store.set(W, { ciphertext: seed.ciphertext, nonce: seed.nonce, version: 5 });

    await sync.saveDirectory(W, kDir, "", [entry]);

    const stored = relay.store.get(W);
    assert.equal(stored.version, 6, "PUT must bump version to stored+1");

    // The stored ciphertext must decrypt back to exactly our instance.
    const ct = Buffer.from(stored.ciphertext, "base64");
    const nonce = Buffer.from(stored.nonce, "base64");
    const pt = await crypto.subtle.decrypt({ name: "AES-GCM", iv: new Uint8Array(nonce) }, kDir, new Uint8Array(ct));
    const obj = JSON.parse(new TextDecoder().decode(pt));
    assert.equal(obj.v, 1);
    assert.equal(obj.instances.length, 1);
    assert.equal(obj.instances[0].site_id, "site:Home");

    // saveDirectory must also have updated the in-memory cache.
    assert.equal(sync.getCachedInstances().length, 1);
  });

  it("PUTs version 1 when no blob exists yet (first write)", async () => {
    const pi = await mintPiIdentity();
    const entry = await makeEntry(pi, "site:Home", "Home", 1000);
    const kDir = await makeKDir();
    await sync.saveDirectory(W, kDir, "", [entry]);
    assert.equal(relay.store.get(W).version, 1, "first write is version 1");
  });
});
```

### 2b. Run — expect RED
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && node --test 'web/owner-access/instance-sync.test.mjs'
```
**Expected failure (exact):** `sync.saveDirectory` is `undefined` →
```
TypeError: sync.saveDirectory is not a function
```
on both new tests; `ℹ fail 2`, the Step-1 test still `pass`.

### 2c. Implement `saveDirectory` (encrypt + version+1 PUT). Add to `instance-sync.js`, replacing the bridge line and adding the function + an `encryptDir`/`putBlob` helper.

Add these helpers above the bridge:

```js
// encryptDir AES-GCM-encrypts { v:1, instances } with a fresh random 12-byte
// nonce. Returns {ciphertext, nonce} as base64 STD.
async function encryptDir(kDir, instances) {
  const nonce = crypto.getRandomValues(new Uint8Array(12));
  const pt = new TextEncoder().encode(JSON.stringify({ v: 1, instances }));
  const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv: nonce }, kDir, pt));
  return { ciphertext: b64stdEncode(ct), nonce: b64stdEncode(nonce) };
}

// putBlob PUTs the ciphertext at `version`. Returns the HTTP status (200 | 409 |
// 413 | 400) or 0 on a network error.
async function putBlob(relayBase, W, ciphertext, nonce, version) {
  try {
    const r = await fetch(walletBlobURL(relayBase, W), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ ciphertext, nonce, version }),
    });
    return r.status;
  } catch (_) {
    return 0;
  }
}
```

Add the `saveDirectory` export (above the bridge):

```js
// saveDirectory: write the instances to the relay as ciphertext at version+1.
// Always updates the local copy first (source of truth). On a 409 (another of
// the user's devices wrote concurrently) it re-GETs, merges union-by-site_id
// newest-wins, and re-PUTs ONCE. A network/PRF failure still leaves the local
// copy authoritative (degrade, never discard).
export async function saveDirectory(userHandleB64u, kDir, relayBase, instances) {
  await writeLocal(instances);          // source of truth, regardless of relay
  if (!kDir) return;                    // no PRF -> browser-carried only

  const cur = await fetchBlob(relayBase, userHandleB64u);
  const baseVersion = cur ? cur.version : 0;
  const enc1 = await encryptDir(kDir, instances);
  const status1 = await putBlob(relayBase, userHandleB64u, enc1.ciphertext, enc1.nonce, baseVersion + 1);
  if (status1 === 200) return;
  if (status1 !== 409) return;          // 413/400/network: local copy stands

  // 409: concurrent write. Re-GET, decrypt, merge, re-PUT once.
  const latest = await fetchBlob(relayBase, userHandleB64u);
  if (!latest) return;
  const dec = await decryptBlob(kDir, latest);
  const remote = dec ? await verifyAll(dec.instances) : [];
  const merged = mergeUnion(remote, instances); // local wins ties via newest added_ms
  await writeLocal(merged);
  const enc2 = await encryptDir(kDir, merged);
  await putBlob(relayBase, userHandleB64u, enc2.ciphertext, enc2.nonce, latest.version + 1);
}
```

Update the bridge line to reference the real `saveDirectory`:

```js
try {
  if (typeof window !== "undefined") {
    window.ftwInstanceSync = { loadDirectory, saveDirectory, verifyEntry, getCachedInstances };
  }
} catch (_) { /* non-browser (test harness) — ignore */ }
```

(Replace the earlier `saveDirectory: undefined` bridge block.)

### 2d. Run — expect GREEN (3 tests pass)
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && node --test 'web/owner-access/instance-sync.test.mjs'
```
**Expected tail:** `ℹ tests 4` `ℹ pass 4` `ℹ fail 0` (Step-1's 1 + Step-2's 2 + the cache-side assert inside Step-1; count is the test cases declared with `it`). Confirm `ℹ fail 0`.

### 2e. Commit
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && git add web/owner-access/instance-sync.js web/owner-access/instance-sync.test.mjs && git commit -m "feat(web): instance-sync saveDirectory encrypt+PUT version+1

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Step 3 — reject a bad Pi signature (RED → GREEN, logic already present; this LOCKS it)

### 3a. Append the failing-intent test

```js
describe("verifyEntry / loadDirectory — reject a tampered entry", () => {
  it("verifyEntry returns false when the signature is over a different label (tamper)", async () => {
    const pi = await mintPiIdentity();
    const good = await makeEntry(pi, "site:Home", "Home", 1000);
    // Tamper: keep the signature, change the label the relay claims.
    const tampered = { ...good, label: "Evil" };
    assert.equal(await sync.verifyEntry(good), true, "untouched entry verifies");
    assert.equal(await sync.verifyEntry(tampered), false, "tampered label must fail verification");
  });

  it("verifyEntry returns false for a signature from a DIFFERENT Pi key", async () => {
    const pi = await mintPiIdentity();
    const attacker = await mintPiIdentity();
    const entry = await makeEntry(pi, "site:Home", "Home", 1000);
    // Swap in the attacker's pubkey but keep the real Pi's signature.
    const forged = { ...entry, pi_pubkey: attacker.pubHex };
    assert.equal(await sync.verifyEntry(forged), false, "wrong pubkey must fail");
  });

  it("loadDirectory DROPS an unverifiable entry (relay-injected fake instance)", async () => {
    const pi = await mintPiIdentity();
    const good = await makeEntry(pi, "site:Home", "Home", 1000);
    const fake = { site_id: "site:Attacker", pi_pubkey: pi.pubHex, label: "Pwn", sig: good.sig, added_ms: 2000 };
    const kDir = await makeKDir();
    const enc = await encryptDir(kDir, { v: 1, instances: [good, fake] });
    relay.store.set(W, { ciphertext: enc.ciphertext, nonce: enc.nonce, version: 1 });

    const got = await sync.loadDirectory(W, kDir, "");
    assert.equal(got.length, 1, "only the genuinely-signed entry survives");
    assert.equal(got[0].site_id, "site:Home");
  });
});
```

### 3b. Run — expect GREEN immediately (the Step-1 `verifyEntry`/`verifyAll` already enforce this)
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && node --test 'web/owner-access/instance-sync.test.mjs'
```
**Expected:** all pass, `ℹ fail 0`. This step is a **characterization lock** — if any of these fail, the signature check has a hole; fix `verifyEntry` until green before committing. (If you reached here with the Step-1 implementation intact, they pass without code changes — that is the desired evidence that reject-bad-signature is real, not assumed.)

### 3c. Commit
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && git add web/owner-access/instance-sync.test.mjs && git commit -m "test(web): instance-sync rejects tampered/forged Pi signatures

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Step 4 — 409 optimistic-concurrency retry merge (RED → GREEN)

The Step-2 implementation already handles 409. This step proves the **merge-union newest-wins on retry** behavior with a relay that returns 409 on the first PUT.

### 4a. Append the failing-intent test (uses a relay stub that rejects the first PUT, succeeds on the second)

```js
describe("saveDirectory — 409 retry merges union-by-site_id newest-wins", () => {
  it("on 409, re-GETs, merges the concurrent device's entry, and re-PUTs once", async () => {
    const pi = await mintPiIdentity();
    const kDir = await makeKDir();

    // Device B already wrote a SECOND home concurrently at version 4.
    const homeB = await makeEntry(pi, "site:Cabin", "Cabin", 1500);
    const bWrite = await encryptDir(kDir, { v: 1, instances: [homeB] });
    relay.store.set(W, { ciphertext: bWrite.ciphertext, nonce: bWrite.nonce, version: 4 });

    // Force a 409 on THIS device's first PUT: stub the relay so the first PUT
    // sees a stale baseVersion. We simulate by raising stored.version between
    // our GET and our PUT via a one-shot interceptor.
    const realHandler = relay.handler;
    let bumped = false;
    globalThis.fetch = (url, opts) => {
      const isPut = (opts && (opts.method || "").toUpperCase() === "PUT");
      if (isPut && !bumped) {
        // Someone else (device B again) bumped to version 5 just before our PUT.
        bumped = true;
        const cur = relay.store.get(W);
        relay.store.set(W, { ...cur, version: 5 });
      }
      return realHandler(url, opts);
    };

    // Device A adds its own home and saves.
    const homeA = await makeEntry(pi, "site:Home", "Home", 1000);
    await sync.saveDirectory(W, kDir, "", [homeA]);

    // After the retry, the stored blob must contain BOTH homes (union).
    const stored = relay.store.get(W);
    assert.ok(stored.version >= 6, "retry PUT bumps past the conflicting version");
    const ct = Buffer.from(stored.ciphertext, "base64");
    const nonce = Buffer.from(stored.nonce, "base64");
    const pt = await crypto.subtle.decrypt({ name: "AES-GCM", iv: new Uint8Array(nonce) }, kDir, new Uint8Array(ct));
    const obj = JSON.parse(new TextDecoder().decode(pt));
    const sites = obj.instances.map((e) => e.site_id).sort();
    assert.deepEqual(sites, ["site:Cabin", "site:Home"], "merge keeps both homes union-by-site_id");
  });
});
```

### 4b. Run — expect GREEN (Step-2 logic covers it). If RED, read the message and fix the merge in `saveDirectory`.
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && node --test 'web/owner-access/instance-sync.test.mjs'
```
**Expected:** all pass, `ℹ fail 0`. The decisive assertion is `deepEqual(sites, ["site:Cabin", "site:Home"])` — proof the 409 path re-GET/merge/re-PUT preserved the concurrent device's entry.

### 4c. Commit
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && git add web/owner-access/instance-sync.test.mjs && git commit -m "test(web): instance-sync 409 retry merges union-by-site_id

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Step 5 — degrade-on-no-PRF / decrypt failure (RED → GREEN)

### 5a. Append the failing-intent test

```js
describe("loadDirectory — degrade, never discard", () => {
  it("with kDir=null (no PRF), returns the browser-carried local copy and skips the relay", async () => {
    const pi = await mintPiIdentity();
    const entry = await makeEntry(pi, "site:Home", "Home", 1000);
    // Seed the local IndexedDB copy via a real saveDirectory with a working key,
    // then simulate a fresh browser session that has no PRF (kDir=null).
    const kDir = await makeKDir();
    await sync.saveDirectory(W, kDir, "", [entry]);

    relay.calls.length = 0; // reset call log
    const got = await sync.loadDirectory(W, null, "");
    assert.equal(got.length, 1, "local copy is returned despite no PRF");
    assert.equal(got[0].site_id, "site:Home");
    const hitRelay = relay.calls.some((c) => /\/wallet\//.test(c.url));
    assert.equal(hitRelay, false, "no-PRF must NOT touch the relay blob");
  });

  it("when the blob is undecryptable (wrong kDir), returns the local copy, never empties it", async () => {
    const pi = await mintPiIdentity();
    const entry = await makeEntry(pi, "site:Home", "Home", 1000);
    const goodKey = await makeKDir();
    await sync.saveDirectory(W, goodKey, "", [entry]); // local + relay now hold Home

    // A different key cannot decrypt the stored ciphertext.
    const wrongKey = await makeKDir();
    const got = await sync.loadDirectory(W, wrongKey, "");
    assert.equal(got.length, 1, "decrypt failure degrades to the local copy, not empty");
    assert.equal(got[0].site_id, "site:Home");
  });

  it("on a 404 (relay never saw this wallet), returns the empty local copy without throwing", async () => {
    const kDir = await makeKDir();
    const got = await sync.loadDirectory(W, kDir, ""); // relay store empty -> 404
    assert.deepEqual(got, [], "fresh wallet, no local + 404 relay -> empty, no throw");
  });
});
```

### 5b. Run — expect GREEN (Step-1 degrade branches cover all three). If RED, fix `loadDirectory`'s degrade branches.
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && node --test 'web/owner-access/instance-sync.test.mjs'
```
**Expected tail:** all describe blocks pass, `ℹ fail 0`.

### 5c. Commit
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && git add web/owner-access/instance-sync.test.mjs && git commit -m "test(web): instance-sync degrades to local copy on no-PRF/decrypt-fail/404

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Step 6 — full-suite green + repo-wide web test gate

Run the whole web suite to confirm no regression in sibling modules:
```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && npm test
```
**Expected:** every `web/**/*.test.mjs` passes, including the new `instance-sync.test.mjs`, `ℹ fail 0`.

> No changeset is required for a `web/`-only change? **No** — `web/` is NOT in the auto-exempt list (only `*.md`/`docs/`/`.github/` etc. are). This is a user-visible new feature surface. Add a `minor` changeset before opening the PR:
> ```bash
> cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && npx changeset
> # pick: minor — "Multi-tenant home route: encrypted per-wallet instance directory (instance-sync.js)"
> git add .changeset/*.md && git commit -m "chore: changeset for instance-sync directory manager"
> ```

---

## Cross-group integration notes (do NOT implement here — for the orchestrator)

- **`kDir` shape (depends on Task Group 4 `prf.js`)**: this module treats `kDir` as a `CryptoKey` usable for `crypto.subtle.{encrypt,decrypt}` with `{name:"AES-GCM"}`. `prf.js` `derivePrfKey()` MUST return exactly that (non-extractable AES-GCM-256). The tests stand in with `crypto.subtle.generateKey({name:"AES-GCM",length:256}, false, ["encrypt","decrypt"])` — the real key is HKDF-derived but identical in usage. If `derivePrfKey` returns `null` (no PRF), callers pass `null` here and this module degrades — matches the `kDir=null` branch tested in Step 5.
- **Relay endpoints (Task Group 1/2)**: `loadDirectory`/`saveDirectory` assume `GET/PUT {relayBase}/wallet/{W}/blob` with `b64std` ciphertext/nonce + integer `version`, 404/409/413/400 statuses per CONTRACTS. The fetch stub in the test encodes that contract; the real relay must match byte-for-byte.
- **Entry signature (Pi side, owner API `GET /api/owner-access/instance-descriptor`)**: `verifyEntry` checks the Pi ES256 raw r||s (b64url) over `"ftw-instance:v1:"+site_id+":"+pi_pubkey+":"+label`. The Pi's descriptor signer (using `go/internal/nova/identity.go`'s key) MUST sign that exact string.
- **`relayBase`**: passed `""` in tests (home-host-relative, matching `p2p.js` `relayURL` which is origin-relative). The caller (`next-app.js`/gate, a different group) supplies the right base.

---

# Task Group 6 — web `p2p.js` per-instance identity + public landing / gate

**Branch:** `feat/multi-tenant-home-route`
**Worktree root (all paths absolute):** `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5`

## Scope & grounding (read before starting)

This group rewires the browser P2P transport to pin the Pi identity **per `(origin, site_id)`** taken from the decrypted instance directory (no relay `/api/identity` round-trip), and replaces the single-tenant gate with a **public landing** state plus an auto-open-on-1-entry flow.

Real code this group edits / depends on (all verified):

- `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/p2p.js` — classic IIFE. Today `pinnedIdentity()` (lines ~227-257) caches a single `_pinPromise`, reads/writes `localStorage` key `"ftw.identity:" + apiBase()` storing `{pub, site}`, and falls back to `fetch(relayURL("/api/identity"))`. `site()` (line ~657) resolves through `pinnedIdentity()`. `window.ftwP2P` surface is the contract other modules consume.
- `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/index.html` — gate markup at lines ~120-151 (`#signin-gate` with `data-mode`, children `.signin-gate-connecting`, `.signin-gate-auth`, `.signin-gate-needs-setup`, `.signin-gate-trust`). Module load order at lines ~740-742: `device-key.js` (module) → `p2p.js` → `next-app.js`.
- `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/next-app.js` — gate logic: `showGate(mode)` (~3586), `hideGate()` (~3593), `setupAuth()` (~3723), `showSignInGate()` (~3776), `ownerNotAuthed` (~3585), init block (~3784-3809).
- `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/next.css` — gate CSS lines ~252-346 already define `data-mode="connecting|signin|setup"`. This group **adds** `data-mode="public-landing"`.
- **Dependency from earlier groups** (DO NOT define here): `web/owner-access/instance-sync.js` exports `getCachedInstances() -> instances[]` and `loadDirectory(userHandleB64u, kDir, relayBase) -> instances[]`. Each instance = `{site_id, pi_pubkey, label, sig, added_ms}` (v1 list has exactly 1 entry). `web/owner-access/prf.js` exports `derivePrfKey(assertion)`. These are produced by the web-crypto task group; this group **consumes** them by name only.

Test harness pattern (verified working): `node --test` with a `vm` sandbox for `p2p.js` (see `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/p2p-owner-fetch-wiring.test.mjs`) and static `readFileSync` regex guards for `index.html` / `next-app.js` (see `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/home-route-silent-auth.test.mjs`). Run all web tests with `npm test` (package.json: `node --test 'web/**/*.test.mjs'`).

`pi_pubkey` format = 128-char lowercase hex X||Y, exactly what `go/internal/nova/identity.go::PublicKeyHex()` emits and what `importP256Pub()` in p2p.js already accepts.

---

## Task 6.1 — `p2p.js`: per-`(origin, site_id)` pin store, seeded from the directory, with legacy migration

Pin the Pi identity per `(origin, site_id)` at `localStorage["ftw.identity:" + apiBase() + ":" + site_id]`, sourcing `site_id` + `pi_pubkey` from the directory (`window.ftwInstanceSync.getCachedInstances()`), with a one-time seed from the legacy `ftw.identity:<apiBase>` record so existing single-home users don't re-enroll. NO relay `/api/identity` round-trip when the directory has an entry.

> The earlier web-crypto group's `instance-sync.js` attaches `window.ftwInstanceSync` (classic-consumable). p2p.js reads `getCachedInstances()` synchronously; when empty it falls back to the legacy localStorage record, and only as a last resort to the relay `/api/identity` fetch (preserving today's behaviour for not-yet-migrated users).

### Step 6.1.a — Write the failing test

Create `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/p2p-identity-pin.test.mjs`:

```javascript
// node --test web/p2p-identity-pin.test.mjs
//
// Multi-tenant pin (Task Group 6): pinnedIdentity()/site() must key the pin per
// (origin, site_id) at localStorage "ftw.identity:<apiBase>:<site_id>", taking the
// site_id + pi_pubkey from the decrypted instance directory (window.ftwInstanceSync)
// with NO relay /api/identity round-trip. A pre-existing single-home user is
// migrated: the legacy "ftw.identity:<apiBase>" record seeds the first per-site key.

import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import vm from "node:vm";

const __dirname = dirname(fileURLToPath(import.meta.url));
const P2P_SRC = readFileSync(join(__dirname, "p2p.js"), "utf8");

// A 128-hex (X||Y) public key whose value is irrelevant to the pin-keying logic:
// importP256Pub() is stubbed in the sandbox so we never run real WebCrypto here.
const PUB_A = "a".repeat(128);
const PUB_LEGACY = "b".repeat(128);

// loadP2P evaluates p2p.js inside a vm sandbox. fetchCalls records every fetched
// URL so a test can assert the relay /api/identity endpoint was (not) hit.
function loadP2P({
  pathname = "/",
  hostname = "home.fortytwowatts.com",
  seedStore = {},
  instances = [],
} = {}) {
  const store = new Map(Object.entries(seedStore));
  const fetchCalls = [];
  const win = {
    localStorage: {
      getItem: (k) => (store.has(k) ? store.get(k) : null),
      setItem: (k, v) => store.set(k, String(v)),
      removeItem: (k) => store.delete(k),
    },
    ftwInstanceSync: {
      getCachedInstances: () => instances,
    },
  };
  const sandbox = {
    window: win,
    location: { pathname, hostname },
    localStorage: win.localStorage,
    crypto: { getRandomValues: (a) => a },
    Headers: class {},
    fetch: (url) => {
      fetchCalls.push(String(url));
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () =>
          Promise.resolve({ public_key_hex: PUB_LEGACY, site_id: "site:Relay" }),
      });
    },
    setTimeout: () => 0,
    clearTimeout: () => {},
    console: { warn() {}, log() {} },
    btoa: (s) => Buffer.from(s, "binary").toString("base64"),
    atob: (s) => Buffer.from(s, "base64").toString("binary"),
    TextEncoder,
    TextDecoder,
  };
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(P2P_SRC, sandbox, { filename: "p2p.js" });
  return { win, store, fetchCalls };
}

describe("p2p.js per-(origin, site_id) pin from the directory", () => {
  let h;
  beforeEach(() => {
    h = loadP2P({
      instances: [{ site_id: "site:Home", pi_pubkey: PUB_A, label: "Home" }],
    });
  });
  afterEach(() => {
    h = null;
  });

  it("site() resolves the directory entry's site_id with no relay round-trip", async () => {
    const site = await h.win.ftwP2P.site();
    assert.equal(site, "site:Home");
    assert.equal(
      h.fetchCalls.filter((u) => u.indexOf("/api/identity") !== -1).length,
      0,
      "the directory carries the Pi pubkey + site_id; /api/identity must NOT be fetched",
    );
  });

  it("pins per (origin, site_id) at ftw.identity:<apiBase>:<site_id>", async () => {
    await h.win.ftwP2P.site(); // triggers pinnedIdentity()
    assert.ok(
      h.store.has("ftw.identity::site:Home"),
      "pin key must be ftw.identity:<apiBase>:<site_id> (apiBase is '' on the bare home host)",
    );
    const rec = JSON.parse(h.store.get("ftw.identity::site:Home"));
    assert.equal(rec.pub, PUB_A);
    assert.equal(rec.site, "site:Home");
  });
});

describe("p2p.js legacy single-home migration", () => {
  it("seeds the first per-site pin from the legacy ftw.identity:<apiBase> record", async () => {
    // Existing single-home user: legacy record present, directory has the same
    // site (instance-sync seeded it from the legacy record). The per-site key is
    // written WITHOUT a relay fetch — the pubkey comes from the directory entry.
    const h = loadP2P({
      seedStore: {
        "ftw.identity:": JSON.stringify({ pub: PUB_LEGACY, site: "site:Home" }),
      },
      instances: [{ site_id: "site:Home", pi_pubkey: PUB_LEGACY, label: "Home" }],
    });
    const site = await h.win.ftwP2P.site();
    assert.equal(site, "site:Home");
    const rec = JSON.parse(h.store.get("ftw.identity::site:Home"));
    assert.equal(rec.pub, PUB_LEGACY, "migrated pubkey matches the legacy record");
    assert.equal(
      h.fetchCalls.filter((u) => u.indexOf("/api/identity") !== -1).length,
      0,
      "migration must not re-TOFU against the relay",
    );
  });

  it("falls back to the relay /api/identity ONLY when no directory + no legacy record", async () => {
    const h = loadP2P({ instances: [] }); // empty directory, no legacy record
    const site = await h.win.ftwP2P.site();
    assert.equal(site, "site:Relay", "with nothing cached, the relay TOFU still works");
    assert.equal(
      h.fetchCalls.filter((u) => u.indexOf("/api/identity") !== -1).length,
      1,
      "relay /api/identity is the last-resort path only",
    );
  });
});
```

### Step 6.1.b — Run it, see it FAIL

```
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5
node --test web/p2p-identity-pin.test.mjs
```

Expected: FAIL. The current `pinnedIdentity()` keys on `"ftw.identity:" + apiBase()` (no `:site_id` suffix) and always fetches `/api/identity` on a cache miss, so:
- `pins per (origin, site_id)` fails: `store.has("ftw.identity::site:Home")` is `false`.
- `site() resolves the directory entry's site_id` fails: `site === "site:Relay"` (from the relay stub) and the `/api/identity` filter count is `1`, not `0`.
Look for output like:
```
✖ pins per (origin, site_id) at ftw.identity:<apiBase>:<site_id>
  AssertionError [ERR_ASSERTION]: pin key must be ftw.identity:<apiBase>:<site_id> ...
✖ site() resolves the directory entry's site_id with no relay round-trip
  AssertionError [ERR_ASSERTION]: ... the directory carries the Pi pubkey ... 1 !== 0
```

### Step 6.1.c — Minimal implementation

In `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/p2p.js`, replace the `pinnedIdentity()` function (the block beginning `var _pinPromise = null;` through the closing of `function pinnedIdentity()` returning `_pinPromise`, lines ~227-257) with:

```javascript
  var _pinPromise = null;

  // directoryEntry returns the chosen instance from the decrypted directory
  // (window.ftwInstanceSync, attached by instance-sync.js). v1: the directory is
  // a 1-entry list, so we take the first. Returns null when no directory exists
  // yet (anonymous, pre-decrypt, or a not-yet-migrated single-home user).
  function directoryEntry() {
    try {
      var sync = window.ftwInstanceSync;
      if (!sync || typeof sync.getCachedInstances !== "function") return null;
      var list = sync.getCachedInstances() || [];
      var e = list[0];
      if (e && e.site_id && e.pi_pubkey) return { pub: e.pi_pubkey, site: e.site_id };
    } catch (_) {}
    return null;
  }

  // legacyRecord reads the pre-multi-tenant single pin written at
  // "ftw.identity:<apiBase>" ({pub, site}). Used once to seed the first per-site
  // record so an existing single-home user doesn't re-TOFU.
  function legacyRecord() {
    try {
      var raw = localStorage.getItem("ftw.identity:" + apiBase());
      var rec = raw ? JSON.parse(raw) : null;
      if (rec && rec.pub && rec.site) return rec;
    } catch (_) {}
    return null;
  }

  // pinKey is the per-(origin, site_id) localStorage key. The site_id is part of
  // the key so two tenants reached through the same origin pin independently and
  // can never clobber each other's Pi identity.
  function pinKey(site) { return "ftw.identity:" + apiBase() + ":" + site; }

  // pinnedIdentity resolves {key, site} for the chosen instance, pinning per
  // (origin, site_id). Source priority — NO relay round-trip until the last:
  //   1. the decrypted directory entry (Pi pubkey is Pi-signed there);
  //   2. an already-pinned per-site localStorage record;
  //   3. the legacy single-pin record (migrate it into a per-site record);
  //   4. relay /api/identity TOFU (only when nothing is cached at all).
  // The pubkey is imported once and the promise cached, exactly as before.
  function pinnedIdentity() {
    if (_pinPromise) return _pinPromise;
    var p = (function () {
      var dir = directoryEntry();
      var rec = null;
      if (dir) {
        // Directory wins. Persist the per-site pin so later verifications need
        // neither the directory nor the relay.
        rec = { pub: dir.pub, site: dir.site };
        try { localStorage.setItem(pinKey(dir.site), JSON.stringify(rec)); } catch (e) {}
        return importP256Pub(rec.pub).then(function (key) { return { key: key, site: rec.site }; });
      }
      // No directory. Try a previously-migrated legacy record to learn which site
      // we last talked to, then read its per-site pin (or migrate the legacy one).
      var legacy = legacyRecord();
      if (legacy) {
        var existing = null;
        try { existing = JSON.parse(localStorage.getItem(pinKey(legacy.site)) || "null"); } catch (e) {}
        rec = existing || { pub: legacy.pub, site: legacy.site };
        try { localStorage.setItem(pinKey(rec.site), JSON.stringify(rec)); } catch (e) {}
        return importP256Pub(rec.pub).then(function (key) { return { key: key, site: rec.site }; });
      }
      // Nothing cached anywhere — last-resort relay TOFU (unchanged behaviour).
      return fetch(relayURL("/api/identity"), { credentials: "same-origin" })
        .then(function (r) { if (!r.ok) throw new Error("/api/identity " + r.status); return r.json(); })
        .then(function (id) {
          if (!id.public_key_hex || !id.site_id) throw new Error("identity response missing fields");
          var fresh = { pub: id.public_key_hex, site: id.site_id };
          try { localStorage.setItem(pinKey(fresh.site), JSON.stringify(fresh)); } catch (e) {}
          return importP256Pub(fresh.pub).then(function (key) { return { key: key, site: fresh.site }; });
        });
    })();
    // Cache only on success — a transient fetch failure must not poison later
    // verifications (the pinned record persists in localStorage regardless).
    p.catch(function () { if (_pinPromise === p) _pinPromise = null; });
    _pinPromise = p;
    return p;
  }
```

> Note: `importP256Pub` and `apiBase`/`relayURL` are already defined above this block in p2p.js, so they are in scope. The `site()` exported method (line ~657) is unchanged — it already delegates to `pinnedIdentity()`, so it inherits the new keying for free.

### Step 6.1.d — Run it, see it PASS

```
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5
node --test web/p2p-identity-pin.test.mjs
```

Expected: all tests pass, e.g.:
```
✔ p2p.js per-(origin, site_id) pin from the directory
✔ p2p.js legacy single-home migration
ℹ pass 5
ℹ fail 0
```

Also confirm no regression in the existing transport tests:
```
node --test web/p2p-owner-fetch-wiring.test.mjs web/home-route-silent-auth.test.mjs
```
Expected: all pass (these don't attach `ftwInstanceSync`, so `directoryEntry()` returns `null` and the legacy/relay paths preserve prior behaviour).

### Step 6.1.e — Commit

```
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5
git add web/p2p.js web/p2p-identity-pin.test.mjs
git commit -m "feat(web): pin Pi identity per (origin, site_id) from the instance directory

pinnedIdentity()/site() now key the pin at ftw.identity:<apiBase>:<site_id>,
sourcing site_id + Pi pubkey from the decrypted directory with no relay
/api/identity round-trip. Existing single-home users migrate from the legacy
ftw.identity:<apiBase> record; relay TOFU stays only as the last resort.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6.2 — `index.html` + `next.css`: the public landing panel (no instance data pre-auth)

Add a `public-landing` panel inside `#signin-gate`: brand + one passkey button + a discreet "Learn more" link, and **nothing** about any instance (no count, label, site_id, or Pi key). Wire `data-mode="public-landing"` in `next.css`.

### Step 6.2.a — Write the failing test

Create `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/public-landing.test.mjs`:

```javascript
// node --test web/public-landing.test.mjs
//
// Public landing (Task Group 6): an anonymous visitor with no decryptable
// directory sees brand + a passkey button + a discreet "Learn more" link, and
// NOTHING about any instance (no count, label, site_id, or Pi key) pre-auth.
// Static markup/CSS guards — the gate logic is covered in landing-gate.test.mjs.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const read = (p) => readFileSync(join(__dirname, p), "utf8");
const INDEX = read("index.html");
const CSS = read("next.css");

describe("index.html public landing panel", () => {
  it("has a public-landing panel inside the sign-in gate", () => {
    assert.match(INDEX, /signin-gate-landing/,
      "a .signin-gate-landing panel must exist for the anonymous visitor");
  });

  it("offers a passkey/sign-in button on the landing", () => {
    assert.match(INDEX, /id="signin-landing-btn"/,
      "the landing must carry its own passkey button");
  });

  it("offers a discreet 'Learn more' link", () => {
    assert.match(INDEX, /class="signin-gate-learn"/);
    assert.match(INDEX, /Learn more/i);
  });

  it("leaks NO instance data pre-auth (no site_id/pi_pubkey/label/count tokens)", () => {
    // Slice just the landing panel so we only assert about THIS markup.
    const m = /<div class="signin-gate-landing">([\s\S]*?)<\/div>\s*<!-- \/signin-gate-landing -->/.exec(INDEX);
    assert.ok(m, "landing panel must be delimited by the closing comment for this guard");
    const panel = m[1];
    assert.doesNotMatch(panel, /site:/i, "no site_id in the landing markup");
    assert.doesNotMatch(panel, /pi_pubkey|pubkey/i, "no Pi key in the landing markup");
    assert.doesNotMatch(panel, /instance|tenant/i, "no instance/tenant wording pre-auth");
  });
});

describe("next.css drives the public-landing mode", () => {
  it("shows .signin-gate-landing only in data-mode=public-landing", () => {
    assert.match(CSS, /\.signin-gate-landing\s*\{[^}]*display:\s*none/,
      "landing panel hidden by default");
    assert.match(CSS, /\[data-mode="public-landing"\]\s*\.signin-gate-landing\s*\{[^}]*display:\s*block/,
      "shown only when data-mode is public-landing");
  });

  it("hides the connecting line in public-landing mode (no 'reaching your home')", () => {
    assert.match(CSS, /\[data-mode="public-landing"\]\s*\.signin-gate-connecting\s*\{[^}]*display:\s*none/);
  });
});
```

### Step 6.2.b — Run it, see it FAIL

```
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5
node --test web/public-landing.test.mjs
```

Expected: FAIL — `signin-gate-landing`, `signin-landing-btn`, `signin-gate-learn` do not exist in `index.html`, and `data-mode="public-landing"` is absent from `next.css`. e.g.:
```
✖ has a public-landing panel inside the sign-in gate
  AssertionError [ERR_ASSERTION]: a .signin-gate-landing panel must exist ...
✖ shows .signin-gate-landing only in data-mode=public-landing
```

### Step 6.2.c — Minimal implementation

**1) `index.html`** — insert the landing panel as the first child of `.signin-gate-card`, immediately after the opening `<div class="signin-gate-card">` (line ~121, before `<div class="signin-gate-brand">`). Add:

```html
      <!-- PUBLIC landing: shown to an anonymous visitor with no decryptable
           directory. Brand + one passkey button + a discreet "Learn more" link
           ONLY. Per the multi-tenant spec it reveals NOTHING about any instance
           (no count, label, site_id, or Pi key) before the user authenticates —
           home.fortytwowatts.com is a public front door for everyone. -->
      <div class="signin-gate-landing">
        <div class="signin-gate-brand">&#9889; forty-two watts</div>
        <p class="signin-gate-lead">This is your forty-two-watts home. Sign in to reach it.</p>
        <button id="signin-landing-btn" class="signin-gate-btn">Sign in</button>
        <p class="signin-gate-msg" id="signin-landing-msg" role="status" aria-live="polite"></p>
        <p class="signin-gate-learn-row">Don&rsquo;t have one yet?
          <a class="signin-gate-learn" href="https://fortytwowatts.com" rel="noopener">Learn more &rarr;</a></p>
      </div>
      <!-- /signin-gate-landing -->
```

**2) `next.css`** — after the existing mode-switch block (after line ~346, `body.ftw-next .signin-gate[data-mode="connecting"] .signin-gate-trust { display: none; }`), append:

```css
/* Public landing (multi-tenant): the anonymous front door. Brand + passkey +
   a discreet "Learn more" — no instance data pre-auth. Shown ONLY in
   data-mode="public-landing"; it suppresses the "reaching your home" line and
   the single-tenant auth/setup panels. */
body.ftw-next .signin-gate-landing { display: none; }
body.ftw-next .signin-gate[data-mode="public-landing"] .signin-gate-landing { display: block; }
body.ftw-next .signin-gate[data-mode="public-landing"] .signin-gate-connecting,
body.ftw-next .signin-gate[data-mode="public-landing"] .signin-gate-auth,
body.ftw-next .signin-gate[data-mode="public-landing"] .signin-gate-needs-setup { display: none; }
body.ftw-next .signin-gate-learn-row { color: var(--fg-dim); font-size: 12px; margin: 18px 0 0; }
body.ftw-next .signin-gate-learn { color: var(--accent-e); text-decoration: none; }
```

> The landing reuses the existing `.signin-gate-brand`, `.signin-gate-lead`, `.signin-gate-btn`, `.signin-gate-msg` tokens (all DESIGN.md-compliant: single amber accent, near-black on-accent text, mono eyebrow). No hard-coded hex.

### Step 6.2.d — Run it, see it PASS

```
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5
node --test web/public-landing.test.mjs
```
Expected: all pass.

### Step 6.2.e — Commit

```
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5
git add web/index.html web/next.css web/public-landing.test.mjs
git commit -m "feat(web): public landing panel for the multi-tenant home route

Anonymous visitors with no decryptable directory get brand + a passkey button
+ a discreet Learn more link and NOTHING about any instance. New
data-mode=public-landing drives it; reuses existing gate tokens (single amber
accent per DESIGN.md).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6.3 — `next-app.js`: gate routing — landing for anonymous, auto-open on exactly 1 entry

Drive the new state machine in `setupAuth()`/`showSignInGate()`: anonymous / no-decryptable-directory → `public-landing`; after a successful passkey assertion + PRF decrypt yields **exactly 1** directory entry → auto-open (no picker), i.e. proceed straight to silent device-PoP + dashboard. The landing button runs the same `runSignIn()` ceremony as the existing gate button.

### Step 6.3.a — Write the failing test

Create `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/landing-gate.test.mjs`:

```javascript
// node --test web/landing-gate.test.mjs
//
// Gate routing (Task Group 6): next-app.js must show the PUBLIC landing when
// there's no decryptable directory (anonymous), wire the landing button to the
// same runSignIn ceremony, and AUTO-OPEN when the decrypted directory has
// exactly 1 entry (no picker in v1). Static source guards — they lock the exact
// branch shape so the web-crypto group and this group stay aligned.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));
const read = (p) => readFileSync(join(__dirname, p), "utf8");
const APP = read("next-app.js");

describe("next-app.js — public landing for the anonymous visitor", () => {
  it("shows the public-landing gate mode when no directory is decryptable", () => {
    assert.match(APP, /showGate\("public-landing"\)/,
      "an anonymous / no-directory visitor must land on data-mode=public-landing");
  });

  it("reads the cached directory via instance-sync (getCachedInstances)", () => {
    assert.match(APP, /getCachedInstances\(\)/,
      "the gate must consult the decrypted directory, not invent its own store");
  });

  it("wires the landing button to the SAME runSignIn ceremony as the gate button", () => {
    assert.match(APP, /getElementById\("signin-landing-btn"\)/);
    assert.match(APP, /landingBtn[\s\S]{0,80}runSignIn\(\)/,
      "the landing button must call runSignIn() — one ceremony, not a fork");
  });
});

describe("next-app.js — auto-open on exactly 1 entry (no picker in v1)", () => {
  it("auto-opens when the directory has exactly one entry", () => {
    assert.match(APP, /length\s*===\s*1/,
      "exactly-1-entry must short-circuit straight through (no picker)");
  });

  it("does NOT render a picker UI in v1", () => {
    assert.doesNotMatch(APP, /pickInstance|instance-picker|chooseInstance/i,
      "v1 routes the single home automatically; the picker is deferred");
  });
});
```

### Step 6.3.b — Run it, see it FAIL

```
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5
node --test web/landing-gate.test.mjs
```

Expected: FAIL — `showGate("public-landing")`, `getCachedInstances()`, `signin-landing-btn`, and the `length === 1` branch don't exist in `next-app.js` yet. e.g.:
```
✖ shows the public-landing gate mode when no directory is decryptable
  AssertionError [ERR_ASSERTION]: an anonymous / no-directory visitor must land ...
```

### Step 6.3.c — Minimal implementation

In `/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/web/next-app.js`:

**1)** Add a directory helper just above `showGate` (~line 3586, before `var ownerNotAuthed = false;`):

```javascript
  // hasDecryptableDirectory reports whether instance-sync has a usable directory
  // cached (the user already authenticated + PRF-decrypted this session, or a
  // migrated single-home record). When false the visitor is anonymous and gets
  // the PUBLIC landing — never any instance data.
  function hasDecryptableDirectory() {
    try {
      var sync = window.ftwInstanceSync;
      if (!sync || typeof sync.getCachedInstances !== "function") return false;
      var list = sync.getCachedInstances() || [];
      return list.length >= 1;
    } catch (e) { return false; }
  }
```

**2)** Replace `showSignInGate()` (~lines 3776-3782) with a version that routes the anonymous visitor to the landing, keeps the single-home / unenrolled copy when a directory exists:

```javascript
  // showSignInGate routes the gate. An anonymous visitor (no decryptable
  // directory) sees the PUBLIC landing — brand + passkey + Learn more, NO
  // instance data. A returning visitor whose directory is cached gets the
  // "signin" card, or the "setup" card when p2p.js reports this origin is
  // UNENROLLED (no device key — never set up on the LAN).
  function showSignInGate() {
    if (!hasDecryptableDirectory()) { showGate("public-landing"); return; }
    var unEnrolled = false;
    try {
      unEnrolled = !!(window.ftwP2P && window.ftwP2P.isUnenrolled && window.ftwP2P.isUnenrolled());
    } catch (e) { /* default to the normal sign-in gate */ }
    showGate(unEnrolled ? "setup" : "signin");
  }
```

**3)** Wire the landing button in `setupAuth()` (~line 3725, alongside the existing `gateBtn` wiring). After the `if (gateBtn && !gateBtn._wired) { ... }` line add:

```javascript
    var landingBtn = document.getElementById("signin-landing-btn");
    if (landingBtn && !landingBtn._wired) { landingBtn._wired = true; landingBtn.onclick = function () { runSignIn(); }; }
```

**4)** Auto-open on exactly 1 entry. In `runPasskeySignIn`'s success tail (~lines 3700-3705), after the assertion is decoded but before `applySignedIn()` runs, decrypt + load the directory and short-circuit on a single entry. Replace the `.then(function (finish) { ... })` block (the one that checks `finish.ok` and returns `true`) with:

```javascript
          }).then(function (finish) {
            if (!finish.ok) { say("Sign-in failed (" + finish.status + ").", "err"); return false; }
            // Decrypt the instance directory with the PRF-derived key from THIS
            // assertion, then route. v1: exactly 1 entry → auto-open (no picker).
            return openDirectoryAfterAssertion(cred, say).then(function () {
              say("Signed in.", "ok");
              return true;
            });
          });
```

**5)** Add the `openDirectoryAfterAssertion` helper just above `runPasskeySignIn` (~line 3683):

```javascript
  // openDirectoryAfterAssertion derives K_dir from the login assertion's PRF
  // result, loads + decrypts the relay directory blob, and routes. v1 contract:
  // the directory is a LIST with exactly ONE entry, so we auto-select it and let
  // the dashboard connect to that site (no picker). A PRF/decrypt failure is
  // non-fatal — the browser-carried copy is the source of truth — so we proceed
  // to the dashboard regardless; instance-sync falls back to its local cache.
  function openDirectoryAfterAssertion(cred, say) {
    if (!window.ftwOwnerPrf || typeof window.ftwOwnerPrf.derivePrfKey !== "function" ||
        !window.ftwInstanceSync || typeof window.ftwInstanceSync.loadDirectory !== "function") {
      return Promise.resolve(); // crypto modules absent → carry-local only
    }
    return window.ftwOwnerPrf.derivePrfKey(cred).then(function (kDir) {
      if (!kDir) return; // PRF unsupported (e.g. Firefox) → browser-carried copy
      var userHandleB64u = (cred && cred.response && cred.response.userHandle) || null;
      if (!userHandleB64u) return;
      return window.ftwInstanceSync
        .loadDirectory(userHandleB64u, kDir, location.origin)
        .then(function (instances) {
          if (Array.isArray(instances) && instances.length === 1) {
            // Exactly one home — auto-open. The pin re-resolves from the freshly
            // cached directory entry (p2p.js::pinnedIdentity), so the next
            // owner fetch connects to the right site with no relay round-trip.
            return; // single-entry: nothing to pick, just continue
          }
          // length !== 1 is unreachable in v1 (single-instance-per-wallet); the
          // picker is the deferred multi-instance follow-up.
        });
    }).catch(function () { /* PRF/decrypt failure → carry-local; never blocks login */ });
  }
```

> `cred` here is the raw `PublicKeyCredential` from `navigator.credentials.get`; `derivePrfKey(assertion)` (from `prf.js`) reads `assertion.getClientExtensionResults().prf.results.first`. The userHandle for the blob route is the base64url-encoded `cred.response.userHandle` — but `runPasskeySignIn` already passes `encodeAssertionResult(cred)` to `login/finish`, which base64url-encodes `userHandle`. Use `window.ftwInstanceSync` / `window.ftwOwnerPrf` (the classic-script globals the web-crypto group attaches); if your earlier group exported only ES-module symbols, add the `window.ftwOwnerPrf` / `window.ftwInstanceSync` bridge there — do NOT re-derive crypto here.

**6)** Init: when not gated by a signed-in session, the anonymous default must be the landing (not a bare "connecting"). The existing init (~line 3789) keeps `showGate("connecting")` while reaching home, and `setupAuth()` → `showSignInGate()` resolves it to `public-landing` when no directory exists. No init change needed beyond steps 1-5 — confirm by re-reading `setupAuth`'s not-signed-in tail (~3753-3762), which already calls `showSignInGate()`.

### Step 6.3.d — Run it, see it PASS

```
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5
node --test web/landing-gate.test.mjs
```
Expected: all pass.

Run the full web suite to confirm no regression (the silent-auth guards assert `showGate(unEnrolled ? "setup" : "signin")` still appears — it does, unchanged, inside `showSignInGate`):

```
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5
npm test
```
Expected: all `web/**/*.test.mjs` pass, including `home-route-silent-auth.test.mjs`, `p2p-owner-fetch-wiring.test.mjs`, `p2p-identity-pin.test.mjs`, `public-landing.test.mjs`, `landing-gate.test.mjs`.

### Step 6.3.e — Commit

```
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5
git add web/next-app.js web/landing-gate.test.mjs
git commit -m "feat(web): gate routes anonymous -> landing, auto-opens on 1 entry

setupAuth/showSignInGate show the public landing when no directory is
decryptable; the landing button reuses runSignIn. After a passkey assertion the
PRF-derived key decrypts the directory and, with exactly 1 entry, auto-opens
(no picker in v1). PRF/decrypt failure degrades to the browser-carried copy and
never blocks login.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Cross-task notes & contract compliance

- **Pin key format** is exactly `ftw.identity:<apiBase>:<site_id>` per the contract; on the bare home host `apiBase()` is `""`, so the key is `ftw.identity::site:Home` (double colon) — the test asserts this literally.
- **No relay round-trip**: `pinnedIdentity()` verifies the DTLS fingerprint against the directory's Pi pubkey; `/api/identity` is only the last-resort path. `verifyAnswerSignature()` (unchanged) already uses `pinnedIdentity().then(pin => pin.key)`, so it inherits the directory-pinned key.
- **`site_id` + `pi_pubkey`** come from `window.ftwInstanceSync.getCachedInstances()[0]` — the single-entry-per-wallet v1 directory. `pi_pubkey` is 128-hex X||Y (matches `nova/identity.go::PublicKeyHex()` and `importP256Pub`).
- **Landing leaks nothing**: the panel markup contains no `site:`, `pubkey`, `instance`, or count tokens; the test slices the delimited panel to enforce this.
- **No picker**: `landing-gate.test.mjs` asserts `doesNotMatch(/pickInstance|instance-picker|chooseInstance/i)` and a `length === 1` auto-open branch.
- **Dependencies on earlier groups** (`window.ftwOwnerPrf.derivePrfKey`, `window.ftwInstanceSync.loadDirectory` / `getCachedInstances`): these are the classic-script bridges the web-crypto task group must attach. If that group exported ES-module-only symbols, the bridge belongs in THAT group's `prf.js` / `instance-sync.js`, not here — Task 6.3 consumes the globals by name only.
- **Run-all command** for the whole group: `cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && npm test`.

---

# Task Group 7 — e2e + docs (multi-tenant home route)

Branch: `feat/multi-tenant-home-route`.

## Scope & ground truth (read before starting)

Two deliverables:

1. **A binary-level e2e test** proving the multi-tenant routing + C2 device-key
   refusal: TWO real `ftw-relay`-registered sites with **distinct `site_id`s** and
   **distinct published device-key sets**; a browser-side stand-in that, after the
   C2 challenge, is **accepted (204)** when it signals its own site and **refused
   (403)** when it signals the other site; and an **anonymous GET** that gets only
   the relay-served landing/shell, **never a Pi**.
2. **`docs/relay-deploy.md`** updated with the `-multi-tenant` cutover.

### Why this lives in `go/test/e2e/` and what it actually exercises

`go/test/e2e/owner_gate_test.go` already builds the **real** `ftw-relay` binary
(`buildBinary(t, repo, "ftw-relay")`) and asserts **relay-edge verdicts** — it
does NOT complete a WebRTC/DTLS handshake (that needs a real browser). My test
follows the identical philosophy: it asserts the **relay's routing + C2 gate
verdicts**, which is exactly the security property Task Group 7 is asked to lock
in. The harness helpers I reuse are all defined in this package:

- `buildBinary(t, repo, name)` — `go/test/e2e/pair_test.go:283`
- `repoRoot(t)` — `pair_test.go:395`
- `waitForAPI(t, url)` — `pair_test.go:296`
- `freePort(t)` — `go/test/e2e/stack_test.go:71`

The two "Pis" register with **real `nova.Identity` ES256 keypairs** driving the
real relay binary's `POST /me/register` over HTTP, each publishing a **distinct
device-key set** (C1), exactly as `home_web_test.go` does in-process but here
against the real binary. This keeps "two Pis register distinct site_ids" honest
(real relay, two real site identities, two real device-key sets, real C2 gate)
while keeping the browser stand-in tractable (a signed offer envelope, the same
wire shape `signal_proof_test.go` builds).

### Wire contracts this test depends on (verified in the tree, do NOT invent)

- `POST /me/register` body: `meRegisterRequest{site_id, host_id, public_key, ts_ms, sig, device_pubkeys}` — `handlers.go:917`.
  Signed string when `device_pubkeys` present: `tunnel.MeRegisterSigningStringV2(siteID, hostID, tsMs, keys)` = `"ftw-me-register:v2:"+site+":"+host+":"+ts+":"+sortedJoin(keys)` — `go/internal/tunnel/me_register.go:34`.
- Poll-secret header: `tunnel.PollSecretHeader` = `X-FTW-Poll` — `go/internal/tunnel/host.go:20`.
- `GET /signal/{site_id}/challenge` → `{"nonce":"<hex>","exp_ms":<int>}` — `handlers.go:1077`.
- `POST /signal/{site_id}/offer?n=<nonce>` — under `-require-device-key`, body is the JSON envelope `{"sdp","device_pubkey","nonce","sig"}`; on success → **204**, on C2 failure → **403** (Pi never contacted) — `handlers.go:1123,1200`.
  The rendezvous nonce `n=` must match `signalNonceRe` (`^[0-9a-fA-F]{8,64}$`, `handlers.go:1112`); the proof `nonce` field is the challenge nonce from `/challenge`.
- C2 proof signing string: `signalProofSigningString(site,nonce)` = `"ftw-signal:v1:"+site+":"+nonce` — `handlers.go:1041`. Browser signs raw r||s, base64url-no-pad (`verifyES256B64URL`, `auth.go:42`).
- Device pubkey wire format: 128 lowercase hex X||Y — `validDevicePubKeyHex`, `auth.go:62`.
- Multi-tenant boot rule (Task Group 1's deliverable, depended on here): `-multi-tenant` requires `-home-web`, forces `-require-device-key` ON, makes `-home-site`/`-home-pubkey` no-ops. I consume the flag; I do NOT define it (see "Cross-group dependency").

### Cross-group dependency (gating note — keep the plan honest)

The `-multi-tenant` flag, the `WalletBlobStore`, and the multi-tenant
`homeStaticForward` (anonymous-GET-only, no `-home-site` forward) are defined by
**Task Groups 1–3**. Task 7.1 below asserts the multi-tenant anonymous-landing
behaviour, so it can only go GREEN once Group 1's `-multi-tenant` + multi-tenant
`homeStaticForward` have landed. Task 7.2 (C2 two-site refusal) depends ONLY on
the already-shipped `-require-device-key` gate (`handlers.go:1168`) + `/me/register`
device-key publish (`handlers.go:999`) + `OwnerRegistry` TOFU — all present in the
tree today — so **7.2 is runnable immediately**. Sequence: land 7.2 first
(independent), then 7.1 after Group 1 merges, then 7.3 docs (no code dep). Each
task's "see it FAIL" step below makes the missing piece explicit so a failing run
is diagnostic, not mysterious.

Run commands (from repo root, matching `Makefile:49`):

```
make e2e                                  # cd go && go test ./test/e2e -v -timeout 180s
cd go && go test ./test/e2e/... -run TestMultiTenant -v -timeout 180s   # focused
```

---

## Task 7.2 — C2 two-site refusal (RUN FIRST; no cross-group dependency)

Proves the core multi-tenant security invariant against the **real relay binary**:
two sites register distinct `site_id`s and distinct device-key sets; a browser
stand-in holding Pi-A's device key is **accepted (204)** signaling site-A and
**refused (403)** signaling site-B. The Pi is never contacted on the refusal.

### Step 7.2.1 — Write the failing test

Create `go/test/e2e/multitenant_test.go`. This is the **first** file in this
group, so it also carries the small shared helper `mintSiteIdentity` (a real
ES256 site identity + its 2 device keys) used by 7.1 too.

**File: `go/test/e2e/multitenant_test.go`** (complete):

```go
package e2e

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/nova"
	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// multitenant_test.go — Task Group 7 e2e for the multi-tenant home route.
//
// Like owner_gate_test.go, this drives the REAL ftw-relay binary and asserts
// the relay-edge verdicts (routing + the C2 device-key gate). It does NOT
// complete a WebRTC/DTLS handshake — that needs a real browser. The security
// property under test is "the relay refuses to contact the wrong Pi", which is
// decided entirely at the relay edge by verifyOfferDeviceProof.

// siteFixture is one "Pi": a real ES256 site identity (signs /me/register) plus
// two P-256 device keys it publishes (C1). The device keys stand in for the
// browser's per-origin WebAuthn-derived device key (C3/C4).
type siteFixture struct {
	id        *nova.Identity
	siteID    string
	hostID    string
	devPriv   *ecdsa.PrivateKey
	devPubHex string // 128 lowercase hex X||Y — the published + presented device key
}

// mintSiteIdentity builds a siteFixture with a fresh site identity + device key.
func mintSiteIdentity(t *testing.T, siteID, hostID string) *siteFixture {
	t.Helper()
	id, err := nova.LoadOrCreateIdentity(filepath_Join(t, siteID))
	if err != nil {
		t.Fatalf("identity for %s: %v", siteID, err)
	}
	dp, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("device key for %s: %v", siteID, err)
	}
	return &siteFixture{
		id: id, siteID: siteID, hostID: hostID,
		devPriv: dp, devPubHex: devPubKeyHex(dp),
	}
}

// filepath_Join writes the identity key under a per-site temp path. Split out so
// the import list stays minimal in the fixture builder.
func filepath_Join(t *testing.T, siteID string) string {
	t.Helper()
	return fmt.Sprintf("%s/id-%x.pem", t.TempDir(), sha256.Sum256([]byte(siteID)))
}

// devPubKeyHex encodes a P-256 public key as 128-lowercase-hex X||Y (the
// device_pubkey wire format the relay's validDevicePubKeyHex accepts).
func devPubKeyHex(priv *ecdsa.PrivateKey) string {
	x := priv.PublicKey.X.Bytes()
	y := priv.PublicKey.Y.Bytes()
	buf := make([]byte, 64)
	copy(buf[32-len(x):32], x)
	copy(buf[64-len(y):64], y)
	return hex.EncodeToString(buf)
}

// signProof signs "ftw-signal:v1:<site>:<nonce>" as raw r||s base64url (no pad),
// the exact WebCrypto wire format the relay's verifyES256B64URL expects.
func (sf *siteFixture) signProof(t *testing.T, siteID, nonce string) string {
	t.Helper()
	msg := "ftw-signal:v1:" + siteID + ":" + nonce
	h := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, sf.devPriv, h[:])
	if err != nil {
		t.Fatalf("sign proof: %v", err)
	}
	sig := make([]byte, 64)
	rb, sb := r.Bytes(), s.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):64], sb)
	return base64.RawURLEncoding.EncodeToString(sig)
}

// register drives the real relay's POST /me/register for this site, publishing
// its single device key (C1), and returns the per-host poll secret.
func (sf *siteFixture) register(t *testing.T, relayURL string) string {
	t.Helper()
	tsMs := time.Now().UnixMilli()
	keys := []string{sf.devPubHex}
	sig, err := sf.id.SignRawHex(tunnel.MeRegisterSigningStringV2(sf.siteID, sf.hostID, tsMs, keys))
	if err != nil {
		t.Fatalf("sign me-register: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"site_id":        sf.siteID,
		"host_id":        sf.hostID,
		"public_key":     sf.id.PublicKeyHex(),
		"ts_ms":          tsMs,
		"sig":            sig,
		"device_pubkeys": keys,
	})
	resp, err := http.Post(relayURL+"/me/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("me-register %s: %v", sf.siteID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("me-register %s status=%d body=%q", sf.siteID, resp.StatusCode, b)
	}
	var out struct {
		PollSecret string `json:"poll_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode me-register %s: %v", sf.siteID, err)
	}
	if out.PollSecret == "" {
		t.Fatalf("me-register %s returned empty poll secret", sf.siteID)
	}
	return out.PollSecret
}

// challenge fetches a single-use device-key challenge nonce for siteID.
func challenge(t *testing.T, relayURL, siteID string) string {
	t.Helper()
	resp, err := http.Get(relayURL + "/signal/" + siteID + "/challenge")
	if err != nil {
		t.Fatalf("challenge %s: %v", siteID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("challenge %s status=%d", siteID, resp.StatusCode)
	}
	var out struct {
		Nonce string `json:"nonce"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode challenge %s: %v", siteID, err)
	}
	if out.Nonce == "" {
		t.Fatalf("challenge %s returned empty nonce", siteID)
	}
	return out.Nonce
}

// postOffer posts a C2 offer envelope for targetSite, proving possession of
// signer's device key, and returns the HTTP status. rendezvous is the opaque
// ?n= routing nonce (hex); challengeNonce is the single-use proof nonce.
func postOffer(t *testing.T, relayURL, targetSite, rendezvous string, signer *siteFixture, challengeNonce string) int {
	t.Helper()
	env, _ := json.Marshal(map[string]string{
		"sdp":           "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n", // opaque to the relay
		"device_pubkey": signer.devPubHex,
		"nonce":         challengeNonce,
		"sig":           signer.signProof(t, targetSite, challengeNonce),
	})
	url := fmt.Sprintf("%s/signal/%s/offer?n=%s", relayURL, targetSite, rendezvous)
	resp, err := http.Post(url, "application/json", bytes.NewReader(env))
	if err != nil {
		t.Fatalf("offer to %s: %v", targetSite, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestMultiTenantC2Routing is the heart of Task Group 7: two Pis register
// distinct site_ids with distinct device keys; a browser-side client holding
// Pi-A's device key is ACCEPTED (204) signaling site-A and REFUSED (403)
// signaling site-B — the C2 gate (verifyOfferDeviceProof) never contacts the
// wrong Pi. -multi-tenant forces -require-device-key ON, which is what makes the
// refusal real rather than a no-op park.
func TestMultiTenantC2Routing(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}
	repo := repoRoot(t)
	relayBin := buildBinary(t, repo, "ftw-relay")

	webDir := t.TempDir()
	if err := os.WriteFile(webDir+"/index.html", []byte("<h1>SHELL</h1>"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	relayAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	relayURL := "http://" + relayAddr
	// -multi-tenant: no -home-site/-home-pubkey; -home-web mandatory; the flag
	// forces -require-device-key ON internally (Group 1). -poll-timeout short so
	// the test's challenge/offer round-trips don't wait on the 25s default.
	relayCmd := exec.Command(relayBin,
		"-addr", relayAddr, "-poll-timeout", "2s",
		"-multi-tenant",
		"-home-host", "home.test",
		"-home-web", webDir,
	)
	relayCmd.Stdout = os.Stdout
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer relayCmd.Process.Kill()
	waitForAPI(t, relayURL+"/healthz")

	// Two Pis, distinct site_ids + distinct device keys.
	piA := mintSiteIdentity(t, "site:Alpha", "owner-alpha-1")
	piB := mintSiteIdentity(t, "site:Bravo", "owner-bravo-1")
	piA.register(t, relayURL)
	piB.register(t, relayURL)

	// The browser client is piA's device. Signaling its OWN site → accepted.
	nA := challenge(t, relayURL, piA.siteID)
	if code := postOffer(t, relayURL, piA.siteID, "deadbeef01", piA, nA); code != http.StatusNoContent {
		t.Fatalf("offer to own site %s: got %d, want 204 (accepted)", piA.siteID, code)
	}

	// Same client, but aimed at the OTHER Pi's site → refused 403. piA's device
	// key is not in site:Bravo's published set, so verifyOfferDeviceProof fails
	// and the Pi (site:Bravo) is NEVER contacted.
	nB := challenge(t, relayURL, piB.siteID)
	if code := postOffer(t, relayURL, piB.siteID, "deadbeef02", piA, nB); code != http.StatusForbidden {
		t.Fatalf("CROSS-TENANT LEAK: offer from site:Alpha's device aimed at %s: got %d, want 403 (device key not in that site's set)", piB.siteID, code)
	}

	// And the legitimate owner of site:Bravo IS accepted there — proves the gate
	// rejects only the cross-tenant case, not the site itself.
	nB2 := challenge(t, relayURL, piB.siteID)
	if code := postOffer(t, relayURL, piB.siteID, "deadbeef03", piB, nB2); code != http.StatusNoContent {
		t.Fatalf("offer to own site %s by its own device: got %d, want 204", piB.siteID, code)
	}
}
```

**Run it (expected: FAIL):**

```
cd go && go test ./test/e2e/... -run TestMultiTenantC2Routing -v -timeout 180s
```

**Expected failure output** — at this point Group 1's `-multi-tenant` flag does
not exist yet, so the relay binary exits immediately on the unknown flag and the
relay never comes up:

```
--- FAIL: TestMultiTenantC2Routing (15.xx s)
    pair_test.go:309: waitForAPI http://127.0.0.1:NNNNN/healthz: timed out
FAIL
```

(`flag provided but not defined: -multi-tenant` is printed on the relay's stderr,
which the test forwards to `os.Stderr` — so the cause is explicit.) This is the
expected, diagnostic failure: the test is correct, the flag is the missing piece.

### Step 7.2.2 — Minimal implementation

There is **no implementation work in Task Group 7 for 7.2** — the relay-side
behaviour (`-require-device-key` gate, `/me/register` device-key publish, the
`OwnerRegistry` TOFU + `HasDeviceKey` check) already exists in the tree. The ONLY
thing that turns this GREEN is **Group 1's `-multi-tenant` flag** (which must
imply `-require-device-key`). Once that flag merges, the relay boots, both Pis
register, and the three assertions pass.

To run 7.2 **independently of Group 1 before it lands** (so this group's work is
verifiable on its own branch), temporarily replace the relay flags in the test
with the already-shipped equivalents that produce the same gate:

```go
	relayCmd := exec.Command(relayBin,
		"-addr", relayAddr, "-poll-timeout", "2s",
		"-require-device-key", // the gate 7.2 actually tests
	)
```

This uses only flags present today, so 7.2 goes GREEN immediately and proves the
C2 routing logic. Revert to `-multi-tenant -home-host -home-web` once Group 1 is
on the branch (that combination is what 7.1 also needs, so the final committed
form uses `-multi-tenant`).

**Run it (expected: PASS, with the `-require-device-key` form):**

```
cd go && go test ./test/e2e/... -run TestMultiTenantC2Routing -v -timeout 180s
```

Expected:

```
=== RUN   TestMultiTenantC2Routing
--- PASS: TestMultiTenantC2Routing (2.xx s)
PASS
ok  	github.com/frahlg/forty-two-watts/go/test/e2e	2.xxx s
```

### Step 7.2.3 — Commit

```
git add go/test/e2e/multitenant_test.go
git commit -m "test(e2e): C2 refuses a cross-tenant offer between two registered Pis

Two real ftw-relay-registered sites with distinct device-key sets: a
browser stand-in holding site:Alpha's device key is accepted (204)
signaling site:Alpha and refused (403) signaling site:Bravo — the C2
gate never contacts the wrong Pi.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7.1 — Anonymous gets only the landing, never a Pi (depends on Group 1)

Proves the multi-tenant invariant #1: an anonymous GET to the home host is served
the relay-disk shell, and an `/api/*` GET is refused **at the relay** (403) — a Pi
is never reached, with **no `-home-site` configured at all**. This is the
multi-tenant tightening of `owner_gate_test.go`.

### Step 7.1.1 — Write the failing test

Add to the **existing** `go/test/e2e/multitenant_test.go` (helpers already there):

```go
// TestMultiTenantAnonymousNeverReachesPi proves invariant #1 under -multi-tenant:
// the bare home host serves ONLY the relay-disk landing/shell (200, from disk),
// and any /api/* path is refused at the relay (403) — never forwarded to a Pi —
// with NO -home-site configured. A Pi is registered (so a regression that
// forwarded would actually reach it), yet the anonymous GET still never does.
func TestMultiTenantAnonymousNeverReachesPi(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}
	repo := repoRoot(t)
	relayBin := buildBinary(t, repo, "ftw-relay")

	webDir := t.TempDir()
	if err := os.WriteFile(webDir+"/index.html", []byte("<h1>LANDING</h1>"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	relayAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	relayURL := "http://" + relayAddr
	relayCmd := exec.Command(relayBin,
		"-addr", relayAddr, "-poll-timeout", "2s",
		"-multi-tenant",
		"-home-host", "home.test",
		"-home-web", webDir,
	)
	relayCmd.Stdout = os.Stdout
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer relayCmd.Process.Kill()
	waitForAPI(t, relayURL+"/healthz")

	// A real Pi IS registered — so any accidental forward would actually land
	// somewhere. The anonymous GET must still never reach it.
	pi := mintSiteIdentity(t, "site:Alpha", "owner-alpha-1")
	pi.register(t, relayURL)

	hget := func(path string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, relayURL+path, nil)
		req.Host = "home.test" // trigger the home-host route
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	// Anonymous "/" → the relay-disk landing, 200. Never a Pi.
	if r, body := hget("/"); r.StatusCode != 200 || body != "<h1>LANDING</h1>" {
		t.Fatalf(`anonymous GET "/" status=%d body=%q, want 200 "<h1>LANDING</h1>" from -home-web (never a Pi)`, r.StatusCode, body)
	}

	// Anonymous /api/* → 403 at the relay (P2P-only), even under multi-tenant with
	// no -home-site. A 200 here would be the exposure incident.
	if r, body := hget("/api/status"); r.StatusCode == http.StatusOK {
		t.Fatalf("EXPOSURE: anonymous /api/status returned 200 — owner API must never be served over the relay. body=%q", body)
	} else if r.StatusCode != http.StatusForbidden {
		t.Fatalf("anonymous /api/status: got %d, want 403 (owner API is P2P-only, refused at relay). body=%q", r.StatusCode, body)
	}
}
```

**Run it (expected: FAIL until Group 1 lands `-multi-tenant`):**

```
cd go && go test ./test/e2e/... -run TestMultiTenantAnonymousNeverReachesPi -v -timeout 180s
```

**Expected failure output** (same root cause as 7.2 pre-Group-1 — the relay won't
boot on the unknown flag):

```
--- FAIL: TestMultiTenantAnonymousNeverReachesPi (15.xx s)
    pair_test.go:309: waitForAPI http://127.0.0.1:NNNNN/healthz: timed out
FAIL
```

After Group 1 merges, if the multi-tenant `homeStaticForward` were wrong (e.g. it
tried to resolve a `-home-site` and 503'd, or forwarded `/api/*` to the Pi), the
failure becomes the diagnostic assertion message instead — `anonymous /api/status:
got 503, want 403` or `EXPOSURE: ... returned 200`.

### Step 7.1.2 — Minimal implementation

No Task-Group-7 implementation. This test is GREEN once **Group 1's** multi-tenant
`homeStaticForward` is correct (serves `-home-web` for non-`/api` GETs, returns
403 for `/api/*`, and never depends on `-home-site`). This task's value is the
**regression guard** that pins that behaviour at the binary level.

**Run it (expected: PASS once Group 1 is on the branch):**

```
cd go && go test ./test/e2e/... -run TestMultiTenantAnonymousNeverReachesPi -v -timeout 180s
```

Expected:

```
--- PASS: TestMultiTenantAnonymousNeverReachesPi (2.xx s)
PASS
```

### Step 7.1.3 — Run the whole group + commit

```
make e2e
```

Expect the existing suite plus both new tests to pass (`TestMultiTenantC2Routing`,
`TestMultiTenantAnonymousNeverReachesPi`, alongside `TestOwnerGateThroughRelay`,
`TestPairFlow*`, `TestE2E_FullStack`).

```
git add go/test/e2e/multitenant_test.go
git commit -m "test(e2e): anonymous home GET serves the landing, never a Pi (multi-tenant)

Under -multi-tenant with no -home-site, the bare home host serves the
relay-disk shell (200) and refuses /api/* at the relay (403) even with a
real Pi registered. Regression guard for invariant #1 (anonymous never
reaches a Pi).

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7.3 — `docs/relay-deploy.md` `-multi-tenant` cutover

Doc-only (auto-exempt from changeset per `docs/` allowlist in
`CLAUDE.md` → changeset-check). No test; verification is a render read.

### Step 7.3.1 — Append the cutover section

Append a new top-level section to
`/Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5/docs/relay-deploy.md`
after the existing "Migration from the old subetha relay" section. Exact content
to add (keep prose tight; document trust shift + the four flag rules):

````markdown
## Multi-tenant home route (`-multi-tenant`)

`home.fortytwowatts.com` is the **public** front door for *every* user, not one
owner's box. The relay never pins to a single Pi: an anonymous visitor sees only
the landing; a signed-in owner's browser picks its own Pi (from a PRF-encrypted
directory the relay stores but never decrypts) and drives the existing
`/signal/{site_id}/*` rendezvous. The relay stays a **blind rendezvous + a blind
ciphertext store**.

### One A-record, one cert

Multi-tenant changes nothing about DNS/TLS: still exactly **one** `A` record
(`home → <AWS VM IP>`, proxied) and **one** edge cert for `home.fortytwowatts.com`.
There are **no per-home subdomains** — every tenant shares the single host.

### Flag cutover

| Flag | Single-tenant (legacy v0.117.0) | Multi-tenant |
|---|---|---|
| `-multi-tenant` | absent | **set** |
| `-home-host` | `home.fortytwowatts.com` | `home.fortytwowatts.com` (unchanged) |
| `-home-web` | required | **required** (mandatory: the relay serves the sign-in shell + landing itself so an anonymous GET never reaches a Pi) |
| `-home-site` | `site:Home` (pins the one Pi) | **unused** — legacy no-op; the relay routes per-`site_id` from the browser's decrypted directory |
| `-home-pubkey` | required (pins the one Pi's key) | **unused** — legacy no-op; each Pi TOFU-self-registers its own `site_id`, and the browser pins the Pi key from the Pi-signed directory entry, not from the relay |
| `-require-device-key` | optional | **forced ON** — `-multi-tenant` implies it; if device-key enforcement is off the relay **refuses to boot** (fail closed), because a forged `site_id` must fail the C2 gate so the relay never contacts the wrong Pi |
| `-wallet-blob-dir` | n/a | **new, durable** — see below |

If you pass `-multi-tenant` without device-key enforcement the relay logs an error
and exits non-zero. `-home-site`/`-home-pubkey` are accepted but ignored under
`-multi-tenant` (kept only so an existing unit file doesn't fail to parse).

### Systemd `ExecStart` (multi-tenant)

```
ExecStart=/usr/local/bin/ftw-relay \
  -addr :443 \
  -cert /etc/ssl/relay/cert.pem \
  -key  /etc/ssl/relay/key.pem \
  -multi-tenant \
  -home-host home.fortytwowatts.com \
  -home-web  /var/lib/ftw-relay/web \
  -wallet-blob-dir /var/lib/ftw-relay/wallet-blobs
```

`-require-device-key` is implied by `-multi-tenant`; pass it explicitly if you
prefer it visible in the unit. Deploy the same `web/` bundle the Pi serves into
`-home-web` (the shell + `p2p.js` + landing).

### Durable opaque state: `-wallet-blob-dir`

This is the **one piece of durable relay state**. Per signed-in wallet the relay
stores an **opaque ciphertext** directory blob (`<user_handle>.blob`), keyed by
the wallet's WebAuthn `userHandle`. The relay **never decrypts it**: the blob is
AES-GCM ciphertext whose key is derived browser-side from the passkey's WebAuthn
PRF output. The relay only enforces size caps (`-wallet-blob-max-bytes`, default
65536), an opaque `userHandle` validation, optimistic-concurrency `version`
monotonicity, and idle GC.

Provision a durable path that survives relay restarts and add it to the unit's
writable paths (relax `ProtectSystem=strict` for this directory):

```
sudo install -d -m 0700 -o root -g root /var/lib/ftw-relay/wallet-blobs
```

…and add to the `[Service]` block:

```
ReadWritePaths=/var/lib/ftw-relay/wallet-blobs
StateDirectory=ftw-relay
```

### Trust shift (read this before cutting over)

Multi-tenant moves the relay from **stateless** to **holding one durable secret-ish
artifact**. Three honest consequences:

1. **A disk compromise yields ciphertext only** — `{user_handle, ciphertext,
   nonce, version}`. The relay cannot read a directory; the PRF key never leaves
   the browser. But `user_handle → ciphertext` + IP logs is a richer correlation
   target than the old stateless relay. Mitigated by storing nothing else, padding
   ciphertext to fixed buckets so blob length never leaks the instance count, and
   count-bound + GC.
2. **Metadata correlation is a known residual.** The relay (or a network observer)
   can timing-correlate a `/wallet/{W}/blob` GET followed by a
   `/signal/{site}/offer` from the same IP and *infer* `W → site`. "The relay never
   learns wallet→site" is true at the request level but defeatable by timing+IP.
   Accepted for a Sourceful-operated relay; weaker than a mixnet — said plainly.
3. **The relay never routes to a Pi without device-key proof.** With
   `-require-device-key` forced on, the relay→Pi forward is gated by the C2 proof:
   a forged or swapped `site_id` fails because the browser's device key isn't in
   that site's published set, so the wrong Pi is never contacted. Owner data + the
   session cookie still ride DTLS only; nothing owner-side traverses the relay.

### Cutover from single-tenant

The v0.117.0 single-tenant home route is currently disabled (flags removed;
`home.*` 404s). To bring it up multi-tenant:

1. Deploy the `web/` bundle to `-home-web` on the VM.
2. Create the durable `-wallet-blob-dir` and grant the unit `ReadWritePaths` to it.
3. Replace the `ExecStart` flags per the table above (`-multi-tenant`,
   `-home-web`, `-wallet-blob-dir`; drop `-home-site`/`-home-pubkey`).
4. `sudo systemctl daemon-reload && sudo systemctl restart ftw-relay`.
5. Confirm the relay booted (it exits non-zero if device-key enforcement is off):
   `sudo systemctl status ftw-relay` and `curl -fsS https://home.fortytwowatts.com/`
   should serve the landing shell.
````

### Step 7.3.2 — Verify the doc renders + commit

```
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/home-route-phase5 && grep -n "multi-tenant\|wallet-blob-dir\|require-device-key" docs/relay-deploy.md
```

Expected: the grep lists the new section's lines (flag table + trust-shift +
`ExecStart`). No build/test needed (doc-only; changeset-exempt).

```
git add docs/relay-deploy.md
git commit -m "docs(relay): -multi-tenant cutover — one A-record, forced device-key, wallet-blob-dir trust shift

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Final verification for the group

```
make e2e
```

Expected tail:

```
--- PASS: TestE2E_FullStack
--- PASS: TestOwnerGateThroughRelay
--- PASS: TestPairFlow
--- PASS: TestPairFlowThroughRelay
--- PASS: TestMultiTenantC2Routing
--- PASS: TestMultiTenantAnonymousNeverReachesPi
PASS
ok  	github.com/frahlg/forty-two-watts/go/test/e2e	NN.NNNs
```

(`TestMultiTenant*` go GREEN only once Group 1's `-multi-tenant` flag is on the
branch; run them with the `-require-device-key` substitute described in 7.2.2 to
verify this group's logic independently before then.)

## Notes / honest caveats

- **No full DTLS handshake.** Like `owner_gate_test.go`, these tests assert
  relay-edge verdicts, which is precisely the security boundary Task 7 owns. A
  browser-in-the-loop test (real WebAuthn PRF + DataChannel) is out of scope for
  the Go e2e harness and belongs in the web `node --test` suite (Task Group 5/6).
- **The `device key == browser key` mapping** in the fixture is a faithful stand-in:
  in production the browser's per-origin non-extractable device key (C3/C4) is what
  the Pi publishes (C1) and the browser proves (C2). The test mints that keypair
  directly instead of running WebAuthn, identical to `signal_proof_test.go`.
- **Run-first ordering.** 7.2's logic is verifiable today via the
  `-require-device-key` substitute; only the final `-multi-tenant` flag form depends
  on Group 1. 7.1 depends on Group 1's `homeStaticForward`. 7.3 has no code dep.

---

