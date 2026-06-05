---
"forty-two-watts": minor
---

Owner remote access: reach your own dashboard from anywhere via a single URL
(`home.fortytwowatts.com`) + a passkey — over a **direct, end-to-end-encrypted
browser↔Pi WebRTC DataChannel**. OPT-IN, default OFF (`remote_access.enabled` /
`FTW_REMOTE_ACCESS_ENABLED`); the Pi makes no outbound connection unless you turn
it on.

The relay is a **blind signaling rendezvous** — it brokers the connection and
serves the static shell, but owner traffic and the session cookie exist only as
DTLS-encrypted DataChannel frames and never traverse it in cleartext. Hardening
that shipped with it:

- **Signed DTLS-fingerprint handshake** (ES256 over the site identity key): the
  browser TOFU-pins the Pi's key from `/api/identity` and verifies every answer,
  so a relay that swaps the fingerprint can't MITM the channel (fail-closed).
- **Fail-closed gate**: an unauthenticated remote request can never reach owner
  data or control endpoints; the relay forwards only `GET` static assets +
  `/api/identity`, strips the owner cookie, and the Pi's tunnel marker blocks any
  LAN-bypass on a tunnelled request. The LAN enrollment PIN is LAN-only.
- **Operator-pinned home site** (`-home-pubkey`): the public home host refuses to
  run trust-on-first-use, so a racing attacker can't claim it across relay
  restarts.
- **Blind TURN** (optional) as a ciphertext-only fallback for hard-NAT/CGNAT
  peers — costs zero trustlessness.
- DoS-resilience on the relay: per-source-IP signaling throttle (Cloudflare-aware
  via `-trust-cf-ip`), nonce-keyed signaling mailbox, fast unauth-peer reap, and
  principal-bound poll secrets.
