# P2P-only home route — v1 implementation plan

> Executes the spec `docs/superpowers/specs/2026-06-04-p2p-only-home-route-v1-design.md`.
> **Ordering invariant:** every slice keeps the local docker harness
> (`make e2e-docker-up`) and `make verify` green. The cleartext tunnel is
> removed LAST, only after the P2P-only path is proven. The fail-closed gate
> lands WITH (never after) anything that removes the `X-FTW-Tunnel` marker.

Branch: `p2p-only-home-route` (off the harness branch so the harness tests it).
Test bench: the docker harness (PR #433) + tier 2 (headless Chrome, in-container
direct P2P).

## Slices (in dependency order)

### 1 — Opt-in config ✅ DONE
- `internal/config`: `RemoteAccess{Enabled, TURN}` block + `RemoteAccessEnabled()`
  (nil-safe). `main.go`: dial the relay only when `RemoteAccessEnabled() && FTW_RELAY_URL`.
  `config.example.yaml` documents it (off). Harness config sets `enabled: true`.
- **Acceptance:** `TestRemoteAccessOptInDefaultsOff` green; a Pi without the flag
  logs "not dialing (opt-in)" and never registers.

### 2 — Identity signing primitives
- `internal/nova/identity.go`: add `Signer() crypto.Signer`; add a **domain-separated**
  signing entry (a context label at the SignRawHex layer, not a caller prefix) so the
  DTLS-fp use can't be confused with the JWT/claim/me-register uses of the same key.
  Add stable DTLS-cert support: mint a cert from the identity key and **persist the
  DER** next to `nova.key` so the fingerprint never drifts across restarts
  (ECDSA signing is non-deterministic).
- **Acceptance:** unit tests — signing string is domain-separated; the persisted
  cert yields a stable fingerprint across reloads; `SignRawHex` output still
  verifies as WebCrypto-compatible raw R||S.

### 3 — Signed-fingerprint handshake (belt-and-suspenders alongside existing P2P)
- `internal/p2p` (manager/bridge) + `api_p2p.go`: the Pi mints the stable cert,
  extracts its answer `a=fingerprint`, signs `ftw-dtls-fp:v1:<site>:<fp>:<ts>`,
  returns `(answerSDP, fpSig, ts)`.
- `web/p2p.js`: on first connect fetch + **TOFU-pin** `GET /api/identity`; on every
  answer, verify the signed fingerprint against the pinned key (WebCrypto ECDSA,
  **SPKI import** for Safari) AND assert it equals the SDP's `a=fingerprint`; abort
  on mismatch.
- **Acceptance:** Go test that the answer carries a valid signature over its own
  fingerprint; harness/tier-2 shows the browser verifying + connecting.

### 4 — Blind signaling rendezvous
- `ftw-relay/handlers.go`: add the 2-slot `/signal/{site|host}/{offer|answer}`
  mailbox (no `Queue`, no body plumbing); authenticate the Pi's offer-drain with the
  already-signed `/me/register` presence; rate-limit + TTL the browser offer slot.
- `owner_relay_register.go`: replace the reverse-proxy long-poll with a thin
  `GET /signal/{host}/offer` → `Manager.Answer` → `POST /signal/{host}/answer` loop;
  keep the 60 s signed heartbeat.
- `web/p2p.js`: send the offer / poll the answer over `/signal` instead of
  `/api/p2p/offer`.
- **Acceptance:** harness P2P connects via the rendezvous (no tunnel involved);
  relay logs show only opaque SDP/sig blobs.

### 5 — Fail-closed gate (THE safety fix)
- `internal/p2p` bridge + `api_owner_access.go`: stamp every P2P-delivered frame
  with a positive "authenticated-remote" signal; flip `authorizeOwner` /
  `api_owner_gate.go` to **default-DENY** any unmarked request that isn't a genuine
  private-LAN source — so a P2P frame can never inherit `lan-bypass`.
- **Acceptance:** regression test — a P2P frame with no owner session gets **401**
  on `/api/*`; LAN-bypass still requires a real private-LAN `RemoteAddr`; the
  existing `TestOwnerGateThroughRelay` semantics hold.

### 6 — Remove the cleartext owner tunnel (LAST)
- Remove `homeForward` + `meForward`/`meTunnel`/`meRoot` (owner routes) from the
  relay; remove `buildOwnerHost` + the `httputil.ReverseProxy` from
  `owner_relay_register.go`. **Keep `tunnel.Queue` + the friend routes** (the friend
  pair-flow still uses them — separate concern).
- **Acceptance:** `make verify` green; harness home route serves over P2P only; the
  owner cookie is never present in any relay-visible frame.

### 7 — Adversarial review + full e2e
- Run a verification **Workflow** (independent agents) against the diff: confirm no
  cleartext owner path remains, the gate can't fail open, the fingerprint signature
  is domain-separated and correctly verified, and the friend flow is untouched.
- Final: `make verify-all` + the docker harness (tiers 1 & 2) green.

## Deferred (NOT v1)
Device-key PoP session, WebAuthn-PRF unlock, mutual device-fingerprint pinning,
LAN-direct pin (option B), SRI/Service-Worker integrity, remote add-a-device
(v2 `#`-fragment token), friend-flow migration off the tunnel, a running TURN
server (config knob only).
