# Multi-tenant home-route onboarding bootstrap — design

**Status:** approved-pending-review (brainstorming → spec). 2026-06-06.
**Builds on:** `2026-06-05-multi-tenant-home-route-design.md` (the infrastructure,
shipped behind `-multi-tenant` in v0.119.0 — relay routing + Ed25519 blob
write-auth, `prf.js`/`instance-sync.js`, the Pi instance-descriptor).
**Resolves:** the onboarding deadlock found in the 2026-06-05 live cutover.

## Goal

Let a brand-new user get their first encrypted directory seeded — so the
multi-tenant home route is actually usable — without breaking any of the five
hard invariants.

## The deadlock this resolves

A first-time user can't bootstrap because three needs form a cycle:

1. WebAuthn passkey **create** + the **PRF** secret (which derives the directory
   key) only work at origin `https://home.fortytwowatts.com` (RP-ID is pinned in
   the Pi's config; browsers refuse WebAuthn on a plain-HTTP LAN IP).
2. Seeding the directory needs the Pi's signed **descriptor** `{site_id,
   pi_pubkey, label}` — served only over the owner-authed P2P channel.
3. Opening that channel under `-multi-tenant` needs a **device-key proof** the Pi
   published — but the device key is minted *during* enrollment.

Directory → descriptor → channel → device-key → enrollment → (reach the Pi) →
channel. Circular.

## Decisions (locked with Fredrik, 2026-06-06)

1. **Approach A — LAN-handoff bootstrap.** The Pi (not the browser) publishes its
   own signed descriptor into a blind, site-keyed relay blob, unlocked by a
   one-time 6-digit LAN PIN. At `home.*` the browser claims + verifies the
   descriptor, enrolls through one narrow PIN-gated relay forward, and seeds the
   directory. No pre-enrollment P2P channel. (Chosen over B = pair-grant/E2E and
   C = LAN-HTTPS-subdomain.)
2. **Enroll-forward = maximally hardened.** The one new pre-device-key path to the
   Pi is forwarded ONLY when: zero devices are enrolled **and** a valid live PIN
   matches **and** the `site_id` matches; single-use (burns after the first
   `enroll/finish`); rate-limited per source IP; never reachable via the friend
   pair-flow loopback tunnel. Covered by dedicated hardening tests + a Codex audit
   before the flag is flipped.
3. **PRF determinism gates go-live.** Before `-multi-tenant` is flipped in
   production, a real-device test (iPhone + Android, iCloud/Google synced) must
   confirm a synced passkey yields an identical PRF output. "Remember this
   browser" (the browser-carried directory copy) is the fallback when PRF is
   absent (Firefox) or non-deterministic.
4. **Wallet-squat = documented residual.** First-write TOFU on `/wallet/{W}/blob`
   and `/bootstrap/{site_id}` lets a stranger squat an *unused* high-entropy
   handle. Bounded by the wallet-count cap; handles are high-entropy; `site_id` is
   Pi-TOFU-pinned via `/me/register`. A pin-preserving tombstone for reclamation is
   deferred.

## The five hard invariants (must hold throughout)

1. **Relay stays blind** — never sees plaintext, never co-presents
   `userHandle`↔`site_id` in one request, can't decrypt the blob, can't MITM the
   Pi identity (the descriptor is Pi-ES256-signed and browser-verified).
2. **Pi is the sole WebAuthn RP** — RP-ID `home.fortytwowatts.com` is baked into
   the Pi; the relay never becomes an RP.
3. **Owner data is E2E-only** — the attestation/cookie/owner-API only over the
   P2P DTLS channel after auth (the bootstrap forward carries the *enrollment
   ceremony*, which is itself WebAuthn-attested; no owner *data* leaks).
4. **First trust = proven LAN presence** — the 6-digit enroll-PIN, mintable only
   by a genuine private-range LAN source that did not arrive via the relay tunnel.
5. **Generic for all users** — each user runs their own Pi pointed at the shared
   relay; nothing hardcoded to one box.

## The flow (Approach A, end to end)

```
LAN (http, no WebAuthn)                 relay (blind)            home.* (https, WebAuthn+PRF)
─────────────────────────               ─────────────           ───────────────────────────
1. user → "Set up remote access"
   Pi mints 6-digit PIN (10-min TTL,
   LAN-source-gated). Shows it.
2. Pi → PUT /bootstrap/{site_id}
   {descriptor, pi_sig, sha256(PIN)} ──▶ store opaque,
   (authed by the Pi identity key          TTL ~10 min,
    the relay already pins)                 keyed by site_id
                                                                 3. user opens home.*, types
                                                                    home-name + PIN
                                            ◀── POST /bootstrap/claim {pin}
                                            return Pi-signed descriptor ──▶ 4. browser VERIFIES pi_sig
                                                                              against pi_pubkey (catches
                                                                              a tampering relay)
                                            ◀── enroll/start?pin (forward) ── 5. WebAuthn create + PRF.
                                            forward ONLY: 0 devices +          The ceremony reaches the
                                            live PIN + site match;             Pi via the ONE narrow
                                            single-use, rate-limited           PIN-gated relay forward.
                                            ── forward to Pi ──▶ Pi
                                            ◀── enroll/finish?pin (forward) ── 6. browser mints device-key
                                                Pi stores credential +            (C4, in the finish body)
                                                device-key + wallet W;
                                                re-/me/register → publishes
                                                device-key (C2 works hereafter);
                                                burn the bootstrap PIN+blob
                                            ◀── PUT /wallet/{W}/blob ───────── 7. PRF→K_dir, Ed25519 write
                                            TOFU-pin write_pub, store            key, encrypt directory
                                            ciphertext (blind)                   [{site_id,pi_pubkey,label}],
                                                                                 sign, PUT. "Home added."
```

**Steady state** (every later visit, any synced-passkey device): open `home.*` →
PRF → `GET /wallet/{W}/blob` → decrypt → verify each Pi sig → open the P2P channel
with the now-registered **device-key proof** (normal C2 gate) → route to the Pi.
The bootstrap PIN/blob expired long ago.

## Components & changes

### Relay — `go/cmd/ftw-relay`
- **`bootstrap.go` (NEW):** a `BootstrapStore` — `map[site_id]{descriptor bytes,
  pinHash, expiresAt}`. The descriptor is **Pi-signed cleartext** (`site_id` +
  `pi_pubkey` are not secret — the relay already holds both from `/me/register`,
  so the bootstrap blob reveals nothing new to it; the Pi *signature* is what
  matters, and the browser verifies it). In-memory + TTL'd (~10 min); NOT durable
  (unlike the wallet blob — bootstrap is ephemeral). Bounded count; GC'd by the
  janitor. The relay never needs to trust-parse the descriptor.
- **`PUT /bootstrap/{site_id}`** — the Pi self-publishes `{descriptor, pi_sig,
  pin_hash}`. Authenticated like `/me/register` (the Pi identity key the relay
  already pins per `site_id`); reject if the `site_id`'s pinned key doesn't
  verify. Bounded body. Registered only under `-multi-tenant`.
- **`POST /bootstrap/claim`** — body `{pin}`. Returns the stored descriptor blob
  for the bootstrap whose `pin_hash == sha256(pin)`, or 404. Rate-limited per IP;
  the relay learns only `sha256(pin)` + `site_id` (never `userHandle`).
- **The narrow enroll-forward** — under `-multi-tenant`, the relay forwards ONLY
  `POST /api/owner-access/enroll/start` and `/enroll/finish` to the Pi (over the
  existing tunnel), and ONLY when the request carries a `pin` whose `sha256`
  matches a live bootstrap blob for a `site_id` whose Pi has **zero** enrolled
  devices. Single-use: the bootstrap burns on the first successful `enroll/finish`
  (and on TTL expiry). Never reachable from the friend pair-flow loopback path
  (extend the owner-only deny list). This is the single most security-sensitive
  new surface — see Hardening below.
- **Janitor:** GC expired bootstrap blobs.

### Pi — `go/internal/api`, `go/cmd/forty-two-watts`
- **Self-publish the descriptor** while a fresh enroll-PIN is live **and** zero
  devices are enrolled: build the signed descriptor (reuse
  `instanceDescriptorSigningString` + the ES256 identity), `PUT /bootstrap/{site_id}`
  to the relay with `sha256(PIN)`. Triggered when the PIN is minted
  (`handleOwnerEnrollPin`) and refreshed if re-minted. Stops once any device
  exists.
- **`enrollAllowed`** already permits first-enroll over the tunnel with a valid
  PIN (the existing `isTunneled + valid PIN + zero devices` branch) — confirm it
  covers the bootstrap-forwarded path and nothing else.
- No change to the WebAuthn RP-ID, the credential store, or the device-key (C4)
  binding — all reused verbatim.

### Web — `web/owner-access`
- **LAN setup page** (`index.html` / a small "set up remote access" affordance):
  on a genuine-LAN origin, call the existing enroll-PIN endpoint, show the 6-digit
  PIN big + amber with a live countdown + copy. (Plain HTTP — display only.)
- **`enroll.html` at `home.*`:** add the **claim-by-PIN** step — `POST
  /bootstrap/claim {pin}` → `verifyEntry` the descriptor against `pi_pubkey`
  before trusting → drive `enroll/start+finish?pin` (forwarded) → mint device-key
  → seed the directory via `saveDirectory(W, encKey, https://<rp.id>, …)` (the
  relay base from the WebAuthn RP-ID, already fixed). The current
  `location.origin` seed is replaced by this PIN-anchored flow.
- The public landing (`next-app.js`) shows "**set up on your home network first**"
  guidance to a visitor with no directory, instead of letting sign-in 503 (the UX
  gap from the live test).
- Reuse `prf.js`, `device-key.js`, `instance-sync.js`, `webauthn.js` unchanged.

## Hardening — the narrow enroll-forward (the one new sensitive surface)

The relay's `-require-device-key` gate is its core fail-closed invariant; this is
the single deliberate exception. It MUST:
1. Forward **only** `enroll/start` + `enroll/finish` (never any other `/api/*`,
   never `/signal/*`).
2. Require a `pin` whose `sha256` matches a **live** bootstrap blob.
3. Refuse if the target `site_id`'s Pi has **any** enrolled device (zero-device
   window only).
4. Be **single-use** — burn the bootstrap (PIN + blob) on the first successful
   `enroll/finish`, and on TTL expiry.
5. Be **rate-limited** per source IP (reuse the existing offer/IP limiter).
6. Be **unreachable** via the friend pair-flow loopback tunnel.
7. PIN brute-force: 6 digits, ≤5 tries then burn, 10-min TTL; a remote attacker
   can't re-mint (minting needs LAN presence).

## Security analysis

- **LAN presence** is proven exactly as today (the enroll-PIN gate). The PIN gates
  both `/bootstrap/claim` and the enroll-forward, so a relay that wants to inject a
  fake Pi must first know a live PIN — which needs LAN presence.
- **Relay can't MITM the Pi identity:** the descriptor is Pi-ES256-signed and the
  browser verifies it (step 4) before trusting `pi_pubkey`; a substituted key is
  rejected.
- **Relay stays blind:** `/bootstrap` is keyed by `site_id` (never `userHandle`),
  so wallet↔site is never co-presented; stored bytes are Pi-signed or encrypted.
- **Pi stays the sole RP;** the forward carries the WebAuthn ceremony (attested),
  not owner data. The `ftw_owner` session cookie is still **stripped on the
  forward** (as in the base design — never issued over the relay); the bootstrap
  does not depend on a relay-carried session, because the directory seed (step 7)
  is authenticated by the **Ed25519 write signature**, not a cookie, and
  steady-state sign-in mints the session over the P2P channel.
- **Squat:** `site_id` is Pi-TOFU-pinned via `/me/register`, so a stranger can't
  pre-pin a different key for a real `site_id`.

## Residuals (documented, not closed in v1)

- **PRF cross-device determinism is unverified** — gated behind the real-device
  test (decision 3); fallback = browser-carried copy.
- **Wallet/site squat on first-write TOFU** — bounded, high-entropy; tombstone
  reclamation deferred (decision 4).
- **Relay timing+IP correlation** can weakly infer `userHandle`↔`site_id` during
  the bootstrap minute (claim then `/wallet` PUT from the same IP). Same residual
  class as the base design; not closed (no padding/jitter in v1).
- **One human step** — read the PIN on the LAN page, type it at `home.*`. The
  secure-context boundary makes this unavoidable; QR (plain-HTTP LAN page) and
  pin-in-URL (relay referrer/history leakage) are both rejected.

## Deferred (additive, not v1)

- Pin-preserving tombstone reclamation for abandoned blobs.
- 2nd-instance picker + the multi-instance claim flow (a 2nd Pi appends an entry
  via the same ceremony; concurrent edits use the optimistic 409 retry).
- Directory refresh after a Pi label/site rename (over the authed P2P channel).
- Remote add-a-device without a synced passkey (a one-time `#fragment` token).

## Test strategy

- **Relay (`go/cmd/ftw-relay`):** `bootstrap_test.go` — `PUT /bootstrap` requires
  the pinned Pi key; `claim` returns only on `sha256(pin)` match; TTL + GC; bound +
  rate-limit. `bootstrap_enroll_forward_test.go` — the forward fires ONLY with a
  live PIN + zero devices + site match; burns after `enroll/finish`; refused once a
  device exists; refused for any non-enroll path; refused over the loopback tunnel.
- **Pi (`go/internal/api`):** self-publish builds a descriptor that the browser
  `verifyEntry` accepts (reuse the existing cross-language interop fixture
  pattern); self-publish stops once a device is enrolled.
- **Web:** node tests for the claim-verify-enroll-seed sequence against a faithful
  mock relay (extend `instance-sync.test.mjs` style); the landing shows the
  "set up on your home network" state with no directory.
- **e2e (`go/test/e2e`):** a full bootstrap — mint PIN on the (simulated) LAN, Pi
  self-publishes, a browser stand-in claims + enrolls via the forward + seeds, then
  a fresh session loads the directory and routes. A second offer with no PIN is
  refused (C2 still fail-closed).
- **Manual gate:** the PRF real-device determinism test before the flag is flipped.
- **Codex audit** of the enroll-forward + `/bootstrap` before go-live.
