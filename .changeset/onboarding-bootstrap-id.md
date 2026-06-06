---
"forty-two-watts": minor
---

Multi-tenant onboarding bootstrap on a high-entropy `bootstrap_id` — behind the relay's `-multi-tenant` flag (default OFF, not yet live in production).

A first-time user with no device key can enroll their first passkey on `home.*` without a prior P2P channel. On the LAN the box mints a 6-digit PIN **and** a high-entropy `bootstrap_id` (≥32 bytes CSPRNG, base64url); the raw secret travels only in the onboarding link's `#fragment` (QR or tap). The relay keys its blind, TTL'd bootstrap store on `claim_key = hex(sha256(bootstrap_id))` — **never** the PIN — so it never holds a guessable secret. The browser derives the same `claim_key`, claims the box's signed descriptor back, verifies its Pi signature, and enrolls through the relay's single enroll-forward (`?claim_key` relay gate + `?pin` validated by the box, 5-try burn). The publish carries `ts_ms` (±30s replay guard) and the enroll-forward is atomic single-use (a 200 finish consumes the window).

Reworked from the earlier `sha256(PIN)`-keyed store (a Codex audit found the ~10⁶ PIN space brute-forceable offline). Cross-language interop is locked by tests (Go-signed bootstrap descriptor verifies in the browser `verifyEntry`; browser `claim_key` derivation matches the box/relay) and a full relay↔box e2e covers publish → claim → enroll with C2 fail-closed assertions.
