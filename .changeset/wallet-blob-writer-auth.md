---
"forty-two-watts": patch
---

Harden the (still dormant, behind `-multi-tenant`) relay wallet-blob endpoints.

- **Writer authentication on `PUT /wallet/{user_handle}/blob`** (closes a Codex HIGH from the v0.118.0 foundation). Each PUT now carries an Ed25519 `write_pub` + `sig`; the relay TOFU-pins the write key on the first write and rejects any later write whose key differs or whose signature fails to verify over a canonical `handle|version|nonce|sha256(ciphertext)` message. A `userHandle`-knower without the owner's passkey-derived write key can no longer overwrite or take over a blob. Wallet blobs are no longer time-GC'd (eviction would drop the pin and reopen a squat window).
- **Route gating:** the `/wallet/*` and `/signal/{site_id}/identity` routes are now registered ONLY in multi-tenant mode, and the single-tenant home-host catch-all reserves those paths (404) — so with the flag off the endpoints add no surface (a plain 404, not a 503 or a public-key answer).

Still dormant: `-multi-tenant` defaults off and the production home route stays disabled. The remaining cutover blocker is the WebAuthn-PRF determinism device test. See `docs/superpowers/specs/2026-06-05-multi-tenant-home-route-design.md`.
