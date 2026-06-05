---
"forty-two-watts": minor
---

Hardened relay + device-key remote access: no anonymous path to a home Pi.

The relay now serves the sign-in shell (`-home-web`) and `/api/identity` (from its
pinned `-home-pubkey`) **itself** — an anonymous internet visitor never causes the
relay to contact the Pi. To even open a signaling channel, the browser must prove a
**device-key**: the relay issues a single-use nonce, the browser signs it with a
non-extractable ECDSA P-256 key (WebCrypto, IndexedDB, minted at LAN enrollment),
and the relay verifies it against the device-pubkeys the Pi publishes on
`/me/register` — anything else is 403'd and the Pi is never woken. The same
device-key mints the owner session **silently** over the channel (device-PoP), so a
returning device signs in with no Face ID; step-up still requires a passkey, and
revoking a device drops its key on both Pi and relay. The gate UI now conveys the
posture (direct + end-to-end + relay-blind + "this device is remembered"). LAN-first
enrollment; see `docs/superpowers/specs/2026-06-05-hardened-relay-device-key-design.md`.
