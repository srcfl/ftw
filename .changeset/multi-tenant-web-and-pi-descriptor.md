---
"forty-two-watts": minor
---

Multi-tenant home-route client + Pi instance descriptor — still behind the relay's `-multi-tenant` flag (NOT yet live in production).

Completes the browser + Pi half of the multi-tenant home route (the relay half shipped in v0.118.x):

- **Web:** a PUBLIC landing for anonymous visitors (brand + passkey button only — no instance data); passkey sign-in that derives the directory key from the WebAuthn **PRF** extension, fetches + AES-GCM-decrypts the per-wallet directory blob from the relay, verifies each entry's Pi signature, and routes to the chosen instance's **own** Pi. Identity is pinned **first-key-wins** per `(origin, site_id)` and the relay's `/api/identity` is **never trusted on the public route** (anti-MITM); the Ed25519 directory write key is generated once and synced inside the encrypted blob.
- **Pi:** `GET /api/owner-access/instance-descriptor` (owner-authed, served over the P2P channel) returns the Pi's signed `{site_id, pi_pubkey, label}` so the browser can build + verify its directory entry; first enrollment seeds the encrypted directory blob.
- The single-tenant / LAN sign-in flow is **untouched** — the multi-tenant path is additive and only active on the public home route.

Codex-reviewed: two anti-MITM findings (relay-identity TOFU on the public route; pin-overwrite) found and fixed. Cross-language interop is locked by tests: JS-signed blob PUTs verify in Go, and Go-signed descriptors verify in the browser.

Cutover (flipping `-multi-tenant` on the relay + deploying this web bundle as `-home-web`) still needs the WebAuthn-PRF determinism device test on real synced devices + live browser validation. See `docs/superpowers/specs/2026-06-05-multi-tenant-home-route-design.md`.
