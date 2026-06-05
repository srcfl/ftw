---
"forty-two-watts": minor
---

Relay multi-tenant home-route foundation — behind `-multi-tenant` (default OFF, NOT active in production).

Server-side groundwork for `home.fortytwowatts.com` to become a public multi-tenant front door (anonymous visitor → landing; a signed-in wallet → its own Pi) instead of a single pinned instance. Adds: a BLIND per-wallet encrypted-directory store (`WalletBlobStore` — opaque ciphertext the relay never decrypts, durable, bounded, version-guarded), the `GET/PUT /wallet/{user_handle}/blob` endpoints, a per-site `GET /signal/{site_id}/identity` public-key read, and a fail-closed `-multi-tenant` mode that serves ONLY the relay-disk landing/shell (never forwards to a Pi), forces `-require-device-key` on, and requires `-home-web`.

Dormant scaffolding: the flag defaults off, the multi-tenant routes aren't registered unless it is passed, and the production home route stays disabled. Cutover is gated on a WebAuthn-PRF determinism device test and adding write-authentication to the blob PUT (see `docs/superpowers/specs/2026-06-05-multi-tenant-home-route-design.md`). No change to existing single-tenant behaviour.
