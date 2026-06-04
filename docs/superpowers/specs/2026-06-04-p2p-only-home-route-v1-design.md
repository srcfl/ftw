# P2P-only trustless home route — v1 design (TOFU pin)

**Goal:** make the owner's remote dashboard ride a direct browser↔Pi DTLS
DataChannel **only**, so the relay can never read the owner's traffic — by killing
the cleartext HTTP tunnel and authenticating the WebRTC handshake with the Pi's
own identity key.

**Architecture:** the relay drops from a tunneling proxy to a **blind signaling
rendezvous**. The Pi signs its DTLS fingerprint with its existing ES256 identity
key; the browser pins that key (TOFU, first connect) and verifies every
fingerprint, so a key-less relay cannot MITM. Owner data only exists as
DTLS-encrypted DataChannel frames. The whole thing is **opt-in, default off**.

This is a **refactor + hardening of the existing Phase-5 P2P stack** (`go/internal/p2p`,
`web/p2p.js`, `api_p2p.go` all exist on `master`), NOT a greenfield build.

---

## Scope

**In v1 (the minimal delta):**
1. Kill the cleartext owner tunnel; P2P-DTLS is the only owner transport.
2. Blind signaling rendezvous on the relay (replaces offer-over-tunnel).
3. **TOFU pin (option A):** browser pins the Pi key on first connect via the
   existing `GET /api/identity`.
4. Signed-fingerprint handshake (the one new crypto).
5. The fail-closed gate fix (non-negotiable safety).
6. Opt-in, default off.
7. **Keep the existing `ftw_owner` cookie session** — it's safe inside DTLS.

**Explicitly NOT in v1 (deferred — anti-over-engineering):** device-key
proof-of-possession session, WebAuthn-PRF key unlock, mutual device-fingerprint
pinning, LAN-direct pin (option B), SRI/Service-Worker app-integrity, remote
add-a-device (→ v2, one-time `#`-fragment token), friend-flow migration off the
tunnel, an actually-running TURN server (ship only the **config knob**, off).

---

## Threat model

**Killed by construction** (no owner byte on any relay path):
- **F1** (relay/CF reads + replays the owner cookie) — owner data is DTLS-only.
- **F2** (owner HTTP tunnel hijack via `host_id`) — no owner request/response
  tunnel exists; `homeForward`/`meForward`/`Queue`-owner-path are removed.
- **F4** (`homeForward` 16 MiB body DoS) — no owner body forwarder exists.

**Honest residuals (state plainly, don't hide):**
- **Operator-at-login:** the relay/CF edge serves the SPA JS that verifies the
  fingerprint. P2P closes the *data* plane, not the *code-delivery* plane. This is
  the ~80% ceiling for a browser client; SRI/SW (deferred) only shrink it.
- **First-connect TOFU:** the very first pin trusts the relay once (SSH
  `known_hosts` model). Every connect after is MITM-proof. (Option B / LAN-direct
  pin would close this but hits the `home.*`-valid-cert-on-LAN wall — deferred.)
- **Signaling metadata:** the relay still sees which `site_id` connects, when,
  from which IP, and the public fingerprints. Content is sealed.
- **Connectivity:** strict-P2P can't reach a Pi behind CGNAT-without-IPv6
  (~10–20% of WebRTC needs TURN) — covered later by the blind opt-in TURN.

---

## The four parts

### 1. Transport — P2P-DTLS only
Remove the owner cleartext tunnel: `homeForward` + `meForward`/`meTunnel`/`meRoot`
and their routes (`ftw-relay/handlers.go`), and `buildOwnerHost` + the
`httputil.ReverseProxy` long-poll (`owner_relay_register.go`). Reuse verbatim:
`tunnel.TunneledRequest/Response` framing, `p2p.Manager.Answer`, the `Bridge`,
`web/p2p.js`'s `p2pFetch`. There is **no cleartext fallback** — if WebRTC can't
connect, the route fails (later: opt-in TURN, still DTLS ciphertext).
Keep `tunnel.Queue` alive — the **friend** pair-flow still uses it (separate concern).

### 2. Signaling rendezvous (blind)
New tiny relay endpoints (a 2-slot-per-host offer/answer mailbox, no `Queue`, no
body plumbing): browser `POST /signal/{site}/offer` + long-poll
`GET /signal/{site}/answer`; Pi long-polls `GET /signal/{host}/offer` + `POST
/signal/{host}/answer`. The **already-signed** `/me/register` (ES256, shipped in
#424) authenticates which key owns the site before the Pi may drain offers, and
the browser's offer is rate-limited + slot-TTL'd. The relay forwards opaque
SDP+signature blobs; it never sees plaintext.

### 3. Trust anchor — TOFU pin (option A)
On first connect the browser fetches `GET /api/identity` (exists — returns
`{public_key_hex, ES256, P-256}`) and pins the Pi's pubkey in IndexedDB/localStorage.
First connect trusts the relay once (TOFU); thereafter the pinned key verifies
every fingerprint, so the relay is powerless to MITM. (Pinning only the **public**
key, which is small and re-fetchable, means storage eviction = a harmless re-pin,
**not** a lockout — this is why v1 avoids a non-extractable device *private* key.)

### 4. Signed-fingerprint handshake (the one new crypto)
The Pi mints a **stable** DTLS certificate from its `nova` ES256 key (persist the
cert DER next to `nova.key` so the fingerprint never drifts — ECDSA signing is
non-deterministic, so a regenerated cert changes the fingerprint). After the answer
SDP is final, the Pi extracts the `a=fingerprint:sha-256` line and signs
`ftw-dtls-fp:v1:<site>:<sha256-fp>:<tsMs>` with `SignRawHex` (raw R||S, already
WebCrypto-verify-compatible). **Domain separation is mandatory** — the same key
signs Nova JWTs/claims, so add a context label at the `SignRawHex` layer, not just
a caller prefix. The browser verifies with WebCrypto ECDSA against the pinned key
(import the pubkey as **SPKI** — Safari rejects `raw` EC import; prepend `0x04` for
the raw 65-byte form only where `raw` is supported). The relay carrying the public
SDP/fingerprint can't forge the signature → no MITM, independent of TURN.

---

## Session
**Keep the existing `ftw_owner` cookie.** With no cleartext relay path it only ever
travels inside DTLS, so the relay never sees it (F1 dead). WebAuthn login runs over
`p2pFetch` (the existing `/api/owner-access/login/*` handlers, unchanged — they
don't care the bytes arrived via DTLS). RP-ID stays `home.fortytwowatts.com`
(already the default on master). **No PoP-session rework in v1.**

---

## The one non-negotiable safety fix (fail-closed gate)
Today `authorizeOwner` returns `lan-bypass` when `OwnerAccessLANBypass &&
!isTunneled` (`api_owner_access.go:391`). Removing the relay tunnel removes the
`X-FTW-Tunnel` marker, so a P2P frame would be `!isTunneled` → **lan-bypass →
unauthenticated owner**. Fix: the P2P `Bridge` must stamp remote P2P frames with a
**positive** "authenticated-remote" signal (the way `api_p2p.go` already stamps the
marker when the offer arrived tunneled), and the gate must **default-DENY** any
unmarked request that isn't a genuine private-LAN source. Ship with a regression
test that a P2P frame with no owner session gets 401 on `/api/*`. **This is the
single most dangerous thing to get wrong** and must land before/with the tunnel
removal.

---

## Opt-in, default off
Add `cfg.RemoteAccess.Enabled bool` (default false), sibling to `cfg.Site`/`cfg.API`.
The Pi dials the signaling relay only when `Enabled && relayURL != ""` — env-only
`FTW_RELAY_URL` no longer auto-dials (Erik/xorath: zero unnecessary outgoing
connections; Local Over Cloud). `owner_relay_register.go` shrinks: keep the 60 s
signed `/me/register` heartbeat (now a signaling presence beat), replace the
reverse-proxy long-poll with a thin `GET /signal/{host}/offer` → `Manager.Answer`
→ `POST /signal/{host}/answer` loop.

---

## Integration plan (files that change)
| File | Change |
|---|---|
| `go/cmd/ftw-relay/handlers.go` | add `/signal/*` rendezvous; remove `homeForward`/`meForward` (owner path); keep `Queue` for friends |
| `go/cmd/forty-two-watts/owner_relay_register.go` | opt-in gate; reverse-proxy long-poll → signaling-offer poll |
| `go/internal/p2p/manager.go` (+ `api_p2p.go`) | sign the answer DTLS fingerprint; stable cert |
| `go/internal/nova/identity.go` | `Signer()`/sign helper + domain-separated signing; persist cert DER |
| `go/internal/api/api_owner_access.go` | fail-closed positive-P2P trust signal; default-deny unmarked-remote |
| `web/p2p.js` | signaling over `/signal`; TOFU-pin `/api/identity`; verify signed fingerprint; all owner I/O over `p2pFetch` |

Test bench: the **local docker harness** (PR #433) + **tier 2** (headless Chrome,
in-container direct P2P) being built now.

---

## Open decisions (still yours)
1. **Friend flow:** keep its existing tunnel for v1 (recommend yes — it's the
   intentionally-open, separate concern; migrating it is later).
2. **TURN:** ship the config knob now (off) vs defer entirely (recommend: knob now,
   off, so enabling it later is config-only).
3. **IndexedDB eviction:** v1's "pin only the Pi pubkey + keep the cookie" means
   eviction = re-pin, not lockout — confirm that's acceptable (recommend yes).
