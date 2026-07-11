# Multi-tenant home route — design

**Status:** approved-pending-review (brainstorming → spec). 2026-06-05.
**Supersedes the single-tenant pin** shipped in v0.117.0 and the Phase-4
"live announce table" idea (`2026-06-03-home-route-passkey-design.md`).
**Builds on:** `2026-06-04-p2p-only-home-route-v1-design.md`,
`2026-06-05-hardened-relay-device-key-design.md`.

## Goal

`home.fortytwowatts.com` is a single **public** entry point for everyone: an
anonymous visitor sees only a landing, and a signed-in owner is routed to
**their own** Pi — the relay never being pinned to any one instance and never
learning which Pi belongs to whom.

## The correction this fixes

v0.117.0 shipped the home route **single-tenant**: the relay was booted with
`-home-site site:Home -home-pubkey <owner key>`, hard-pinning
`home.fortytwowatts.com` to one Pi. That was a foundational mistake — the URL is
meant to be the public front door for *all* users, not the owner's personal
instance. The owner's box must be just one tenant among many. The home route is
currently **disabled** on the relay (flags removed; `home.*` 404s; `relay.*`
friend-access untouched) pending this rebuild.

## Decisions (locked with Fredrik, 2026-06-05)

1. **Multi-tenant model.** Public landing for the unpaired; route a paired user
   to their own Pi; the owner's instance is just one tenant.
2. **Binding = hybrid/encrypted directory.** The browser carries the directory
   locally **and** the relay holds a per-wallet **ciphertext** blob that only the
   user's passkey (via WebAuthn PRF) can decrypt — so a fresh, synced-passkey
   device can bootstrap remotely while the relay stays blind.
3. **Approach = full hybrid in v1** (Fredrik chose this over the phased option):
   the PRF-encrypted relay blob ships in the **first** release so fresh-device
   remote bootstrap works from day one.
4. **Multi-instance = later.** v1 routes each wallet to its **one** home (1 entry
   → auto-open, no picker). The directory is a **LIST from day one** (N-ready);
   the cross-Pi "claim" flow that puts several Pis under one wallet, and the
   picker UI, are an additive follow-up.
5. **`-require-device-key` forced ON** under multi-tenant (a forged `site_id`
   must fail the C2 gate, so the relay never contacts the wrong Pi).
6. **De-risk PRF (my addition, doesn't change #3):** the **browser-carried copy
   is the source of truth**; a PRF decrypt failure degrades to "can't bootstrap a
   brand-new browser," never "lost your homes." A **PRF real-device determinism
   spike is build-step 1** (gate the relay-blob reliance behind it).

## Architecture overview (end-to-end, v1)

```
Anonymous visitor → home.fortytwowatts.com
  └─ relay serves the LANDING + SPA shell + p2p.js from -home-web disk (never a Pi)

Owner sign-in (any device with the synced passkey):
  1. Passkey assertion (RP-ID home.fortytwowatts.com) WITH the prf extension
     → yields (a) userHandle W, (b) a PRF secret → HKDF → AES-GCM key K_dir
  2. GET /wallet/{W}/blob  → relay returns opaque ciphertext (or 404)
  3. Browser AES-GCM-decrypts with K_dir → directory = [{site_id, pi_pubkey, label}]
     (verify each entry's Pi-ES256 signature; merge with the browser-local copy)
  4. v1: exactly 1 entry → auto-select it (no picker)
  5. Drive the EXISTING signaling, keyed by that site_id:
       GET  /signal/{site_id}/challenge
       POST /signal/{site_id}/offer?n=<nonce>   (carries the C2 device-key proof)
       GET  /signal/{site_id}/answer  (long-poll)
     Relay routes by site_id (OwnerRegistry TOFU self-registration); the
     relay→Pi forward is authorised by the C2 device-key proof, NOT the site_id.
  6. Browser verifies the answer's DTLS fingerprint against the pinned Pi pubkey
     (taken from the directory entry — no relay round-trip needed), opens the
     P2P DTLS DataChannel, runs device-PoP + (if needed) passkey step-up ON the
     Pi over the channel. All owner data + the session cookie ride DTLS only.
```

The browser, not the relay, chooses the destination. The relay is a blind
rendezvous + a blind ciphertext store.

## Components & changes

Grounded in the current tree. Single-instance-v1 scope noted where it narrows
the full design.

### `go/cmd/ftw-relay/main.go` — flags + boot
- Add `-multi-tenant` (default false). When set: do **not** require
  `-home-site`/`-home-pubkey`, do **not** `owners.Pin(...)`, and skip the
  single-pin pieces of `requireHomePin` — but still require `-home-web` (the
  shell/landing must be relay-served so an anonymous GET never reaches a Pi).
- `-multi-tenant` **implies `-require-device-key`**: if multi-tenant is on and
  device-key enforcement is off, refuse to boot (fail closed).
- Add `-wallet-blob-dir <path>` (durable ciphertext store) and
  `-wallet-blob-max-bytes` (default 65536). Construct the `WalletBlobStore`, wire
  it into `Relay`, and add it to the janitor GC loop (evict idle blobs by
  last-touch, same cadence as `Owners.GC`/`Polls.GC`).
- `-home-host`/`-home-web` stay. `-home-site`/`-home-pubkey` become legacy
  single-tenant-only no-ops under `-multi-tenant`.

### `go/cmd/ftw-relay/owners.go` — OwnerRegistry
- **No structural change.** Per-`site_id` key-continuity (first ES256 key wins),
  the 1024-site cap, and 30-minute GC already bound and self-heal a multi-Pi
  population. Under `-multi-tenant` it simply runs in pure-TOFU mode (every Pi
  self-registers its own `site_id` via the existing ES256-signed `/me/register`).
- **Do NOT** add wallet→site grouping here. Grouping lives only inside the
  client-decrypted blob; the relay must stay blind to wallet→site.

### `go/cmd/ftw-relay/walletblob.go` — NEW, encrypted directory store
- `WalletBlobStore`: `map[userHandle]blobEntry`,
  `blobEntry = {ciphertext []byte, nonce []byte, version int, updatedAtMs int64, touchedAt}`.
  Persisted to `-wallet-blob-dir` (one file per userHandle) so it survives relay
  restart. **The relay never parses ciphertext.**
- `GetBlob(userHandle) → (ciphertext, nonce, version)` or not-found.
- `PutBlob(userHandle, ciphertext, nonce, version)` with **optimistic
  concurrency**: reject `version <= stored.version` (lost-update / rollback
  guard). Cap ciphertext at `-wallet-blob-max-bytes`.
- `userHandle` is validated as a fixed-length opaque base64url token (32–64
  bytes) **before** it is used as a map/file key (no path traversal).
- Bound the number of distinct userHandles (`maxWalletBlobs`) and GC idle entries
  by `touchedAt` (same pattern as `maxOwnerSites`).
- **Size-pad** stored ciphertext to fixed buckets so blob length never leaks the
  instance count.

### `go/cmd/ftw-relay/handlers.go` — routing + new endpoints
- `homeStaticForward`: under `-multi-tenant`, the bare home host serves **only**
  the public landing + SPA shell + `p2p.js` from `-home-web`. There is no single
  `-home-site` to resolve and it **never** forwards to a Pi (keeps and tightens
  the slice-1/2 "anonymous GET never reaches a Pi" guarantee).
- Replace `serveHomeIdentity`'s single-pubkey answer: under `-multi-tenant`,
  `/api/identity` becomes **per-site** — `GET /signal/{site_id}/identity` returns
  the **public** key the relay holds for that `site_id` from `OwnerRegistry`
  (public key only, no secret). Anonymous callers with no `site_id` get nothing.
  Note: the browser already learns the Pi pubkey from the (Pi-signed) directory
  entry, so it can pin **without** trusting this relay read — this endpoint is a
  convenience/cross-check, not the trust anchor (closes today's relay-MITM-on-
  first-`/api/identity`-TOFU gap).
- Add `walletBlobGet` (`GET /wallet/{user_handle}/blob`) and `walletBlobPut`
  (`PUT /wallet/{user_handle}/blob`) — opaque ciphertext in/out, never parsed,
  body-capped via `limitBody` + a tighter per-blob cap. Register on **both** the
  host-less mux and the `r.HomeHost`-prefixed mux (mirror how the browser
  `/signal/*` routes are double-registered today for Go ServeMux host-precedence).
- `signalBrowserOffer/Answer/challenge`: **no wire-shape change** — they already
  take a browser-supplied `{site_id}`. Under multi-tenant the `site_id` comes from
  the decrypted directory instead of the relay pin. The C2 device-key proof
  (`verifyOfferDeviceProof`) is what authorises relay→Pi: a forged/swapped
  `site_id` fails C2 because the browser's device key isn't in that other site's
  published set, so the relay never contacts the wrong Pi.

### `go/internal/api/...` + `owner_relay_register.go` — Pi side
- The Pi, after enrollment, exposes over the **P2P channel** (owner API,
  DTLS-only) a **signed instance descriptor** `{site_id, pi_es256_pubkey, label}`,
  signed with its ES256 identity key (the same key that signs DTLS fingerprints),
  so the browser can verify each directory entry before trusting it. The Pi does
  **not** push to the relay blob (it has no PRF key — relay-blind invariant holds;
  the **browser** writes the blob).
- `owner_relay_register.go`: **no** wallet/site_label added to `/me/register`
  (this reverses the Phase-4 announce plan). The announce stays
  `{site_id, host_id, device_pubkeys}` so the relay never learns wallet→site.
- **v1 keeps `wallet_handle` per-Pi** (a single home → `W` = this Pi's wallet;
  the blob lists 1 entry). The cross-Pi stable-`W` claim/replicate flow + a
  `LookupTrustedDeviceByWallet(W)` helper are **deferred** (multi-instance later).

### `web/owner-access/prf.js` — NEW, browser crypto
- PRF capability detection + derivation: request the WebAuthn `prf` extension on
  the login assertion; if `clientExtensionResults.prf.results.first` is present
  (64 bytes), `HKDF-SHA256(prf, salt, info='ftw-instance-blob:aes-gcm:v1')` → a
  **non-extractable** AES-GCM-256 key `K_dir`.
- Keep the per-origin non-extractable **device key** (C3/C4) unchanged.
- **Fallback** when PRF is unsupported (Firefox today; older OSes): the browser
  carries the directory in IndexedDB/localStorage only and **skips** the relay
  blob — it works on the device that enrolled but loses fresh-device remote
  bootstrap. Surface this honestly in the UI ("encrypted home sync isn't
  available on this browser").

### `web/owner-access/instance-sync.js` — NEW, directory manager
- Canonical directory = a **LIST** `[{site_id, pi_es256_pubkey_hex, label,
  origin:'local'|'replicated', added_ms}]`. **Source of truth is browser-side**
  (IndexedDB); mirror to the relay as ciphertext when PRF is available.
- On login: (1) derive `K_dir`, (2) `GET /wallet/{W}/blob`, (3) AES-GCM-decrypt,
  (4) verify each entry's Pi signature, (5) merge with the local copy (union by
  `site_id`, newest wins), (6) if changed, re-encrypt and `PUT` with `version+1`.
- v1 writes a **1-entry** directory at enrollment. The "append on claim" path is
  the deferred multi-instance follow-up; `getInstances()` already returns a list.
- Optimistic-concurrency retry on `409` (re-GET → merge → re-PUT).

### `web/p2p.js` — per-instance identity
- `pinnedIdentity()` takes the chosen entry's `site_id` + expected `pi_pubkey`
  from the directory instead of the single relay `/api/identity`. Pin per
  `(origin, site_id)` in localStorage (`ftw.identity:<origin>:<site_id>`). The
  blob already carries the Pi pubkey **signed by the Pi**, so the browser pins
  without a relay round-trip.
- Backwards-compat: seed the directory's first entry from the legacy
  `ftw.identity:<apiBase>` record so an existing single-home user doesn't
  re-enroll.

### `web/index.html` + `next-app.js` — landing, gate, (no picker in v1)
- A **public landing** panel shown to anyone with no decryptable directory:
  brand + "This is your forty-two-watts home. Sign in to reach it." + one passkey
  button + a discreet "Don't have one yet? Learn more" link to a marketing page.
  **Nothing about any instance** (no count, labels, site_ids, Pi keys) pre-auth.
- Gate state machine: `public-landing → (assertion+PRF) → decrypt-directory →
  connect+device-PoP → dashboard`. v1: exactly 1 entry → auto-open. (The
  `pick-instance if >1` state is wired but unreachable until multi-instance.)

### `docs/relay-deploy.md` + this spec
- Document the `-multi-tenant` cutover: one A-record + one TLS cert for
  `home.fortytwowatts.com`, no per-home subdomains; `-home-web` mandatory;
  `-home-site`/`-home-pubkey` unused; `-require-device-key` forced on; the new
  durable `-wallet-blob-dir` (opaque ciphertext — the one piece of durable relay
  state) and the trust shift it implies.

## Directory blob — schema & crypto

```
plaintext (browser-only):
  { "v": 1,
    "instances": [
      { "site_id": "site:…", "pi_pubkey": "<hex X||Y>", "label": "Home",
        "sig": "<Pi ES256 over site_id|pi_pubkey|label>", "added_ms": 0 } ] }

K_dir = HKDF-SHA256( prf_output, salt=<fixed>, info="ftw-instance-blob:aes-gcm:v1" )
ciphertext = AES-GCM-256( K_dir, nonce, plaintext )

relay stores (opaque): { userHandle: W, ciphertext, nonce, version, updatedAtMs }
```

- Each entry is **Pi-signed**, so a tampering relay cannot inject a fake instance
  even though it stores the blob.
- `version` gives optimistic concurrency (two of the user's devices editing →
  re-GET/merge/re-PUT on `409`). v1 has 1 writer-per-wallet in practice.

## Security invariants (same or stronger than today)

1. **Anonymous never reaches a Pi** — `home.*` serves only the relay-disk
   landing/shell; under `-multi-tenant` there is no `-home-site` to forward to.
2. **Relay never routes to a Pi without user auth** — the relay→Pi forward is
   gated by the C2 device-key proof (forced ON); sessions are minted on the Pi
   over DTLS only.
3. **Relay never decrypts the blob** — opaque ciphertext keyed by `userHandle`;
   all crypto is client-side.
4. **Relay never learns wallet→site** — `W` appears only on `/wallet/*`,
   `site_id` only on `/signal/*`; never co-presented; blob `site_id`s are
   encrypted; no announce carries the wallet.
5. **Pi stays the sole WebAuthn RP** — the relay never verifies an assertion;
   RP-ID stays `home.fortytwowatts.com` on the Pi.
6. **Owner data is DTLS-only after passkey auth** — no owner `/api/*` or
   `ftw_owner` cookie ever traverses the relay.
7. **Per-site key continuity** — each `site_id` is pinned to its first ES256 key,
   so one tenant can never hijack another's site mapping.
8. **Blob integrity without relay trust** — every entry is Pi-ES256-signed; the
   `version` guard prevents lost-update/rollback within the optimistic window.

## PRF de-risk plan (build order)

1. **Spike first (gate):** a real-device PRF determinism test — create one passkey,
   read PRF on device A, sync it (iCloud Keychain **and** Google Password Manager),
   read PRF on device B, assert **identical** output. If it diverges, the relay
   blob can't be relied on for fresh-device bootstrap → ship browser-carried-only
   and revisit (e.g. a passphrase-derived key typed once on a fresh device).
2. **Browser-carried copy is the source of truth** at all times. A PRF decrypt
   failure degrades to "can't bootstrap a brand-new browser," never "lost homes."
3. **Fallback matrix:** {PRF / no-PRF} × {enrolled device / fresh device}. PRF +
   fresh-synced = remote bootstrap works. No-PRF (Firefox) or fresh-without-synced
   passkey = enroll on-LAN; surfaced plainly in the UI as a security property.

## Before the relay flag is flipped in production (HARD blockers)

`-multi-tenant` ships **default OFF** and the relay's home route stays disabled
until BOTH of these are closed. Until then the multi-tenant routes are not even
registered, so the code is dormant.

1. **PRF determinism spike** (above) — verify identical PRF output across a
   synced passkey on real devices, or fall back to browser-carried-only. **This is
   the one remaining hard blocker.**
2. ~~Blob write authentication.~~ **DONE (v0.119.0).** `PUT /wallet/{user_handle}/blob`
   is now writer-authenticated: the client derives an **Ed25519 write key** from the
   PRF secret (`HKDF(prf, info="ftw-blob-write:v1")` → seed), and each PUT carries
   `write_pub` + an Ed25519 `sig` over `blobWriteMessage` =
   `"ftw-blob:v1:" + handle + ":" + version + ":" + base64url(nonce) + ":" +
   hex(sha256(ciphertext))`. The relay **TOFU-pins** `write_pub` on the first write
   and rejects any later write whose key differs (constant-time) or whose signature
   fails — so a `userHandle`-knower without the owner's passkey-derived write key
   cannot overwrite or take over a blob. Wallet blobs are **not time-GC'd** (that
   would drop the pin and reopen a squat window — Codex HIGH). Residual: a stranger
   can still squat an **unused, high-entropy** handle before its first legitimate
   write (TOFU), and an enrollment-capacity flood is bounded by the wallet-count cap;
   a pin-preserving tombstone for abandoned-blob reclamation is a later refinement.
   The web client (`instance-sync.js`, next slice) MUST construct `blobWriteMessage`
   byte-identically.

## Honest residuals (documented, not over-built)

- **PRF support + sync stability is the load-bearing unknown.** Firefox ships no
  PRF; iCloud/Google PRF determinism across sync is undocumented. Mitigated by the
  spike + browser-carried source of truth.
- **Metadata correlation.** The relay (or a network observer) can timing-correlate
  a `/wallet/{W}/blob` GET followed by a `/signal/{site}/offer` from the same IP
  and **infer** `W → site`. "Relay never learns wallet→site" is true at the
  request level but defeatable by timing+IP. Mitigations: fixed-size ciphertext
  padding, optional jitter before first signaling; otherwise accepted as a known
  residual for a Sourceful-operated relay. Weaker than a mixnet — said plainly.
- **New durable relay state.** The ciphertext blob store breaks today's
  in-memory/ephemeral relay. A disk compromise yields ciphertext only, but
  `userHandle → ciphertext` + IP logs is a richer correlation target than a
  stateless relay. Mitigated by storing only `{userHandle, ciphertext, nonce,
  version}`, padding, count-bound + GC, and documenting the trust shift.
- **Two routing paths** (legacy single-pin vs multi-tenant) increase surface.
  Keep the fail-closed `homeStaticForward` checks identical on both paths; cover
  both with the existing `hardening_test.go` / `home_web_test.go` patterns.

## Deferred (additive, do NOT block v1)

- **Multi-instance:** cross-Pi stable `W` via the claim/replicate flow (prove
  existing wallet + LAN presence code on the new Pi → import same `W`), the append
  path in `instance-sync.js`, and the **picker** UI.
- **Remote add-a-device** without a synced passkey (one-time high-entropy token in
  the URL `#fragment`, never sent to the relay).
- **Per-site device keys** (PRF-salted) so the relay can't weakly correlate that
  two sites share a device.

## Open questions resolved by default (object if wrong)

- **Landing for strangers** → brand + passkey + a discreet "Learn more" link
  (no instance data). (Confirmed allowed.)
- **`-require-device-key`** → hard-required under multi-tenant; a Pi with no
  published device key is unreachable remotely (secure default).
- **Fresh device without a synced passkey** → degrade to LAN enroll in v1;
  remote-add-device deferred.
- **Concurrency** → optimistic `version+1` with re-GET/merge/re-PUT; sufficient
  for v1's single-writer-per-wallet reality.

## Test strategy

- **Relay (`go/cmd/ftw-relay`):** `walletblob_test.go` — PUT/GET round-trip,
  version monotonicity (reject `<=`), size cap, userHandle validation (reject
  traversal/oversize), GC/eviction, padding. Extend `home_web_test.go` for
  `-multi-tenant` mode: landing served, no `-home-site` forward, `/api/*` 403,
  `/signal/{site}/identity` returns the registered public key. Add a test that
  `-multi-tenant` without `-require-device-key` **refuses to boot**.
- **C2 routing:** a forged/foreign `site_id` offer fails `verifyOfferDeviceProof`
  (the device key isn't in that site's published set) → relay never forwards.
- **Web:** node tests for `prf.js` (HKDF vectors, capability detection),
  `instance-sync.js` (decrypt → verify-signature → merge → re-encrypt; reject an
  entry with a bad Pi signature; 409 retry), and `p2p.js` per-`(origin,site_id)`
  pinning + legacy migration.
- **e2e:** extend the docker harness (`go/test/e2e`) with **two** Pis registering
  distinct `site_id`s + a browser that, after auth, routes to the correct one and
  is refused the other (C2). Anonymous → landing only.
- **Manual gate:** the PRF determinism spike on real synced devices **before**
  relying on the relay blob.
