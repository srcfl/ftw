---
"forty-two-watts": minor
---

Make owner remote access reachable AND fast. The relay now publishes shared
ICE/TURN configuration (new `GET /signal/ice`, new `-ice-stun`/`-turn-url`/
`-turn-secret` flags) so hard-NAT/CGNAT owners can reach their Pi over a
TURN-relayed DTLS channel, while owner API writes stay on the fail-closed P2P
transport.

Connection setup is also much snappier. The browser no longer arms its retry
cooldown on a transient cold-load (directory-not-yet-decrypted) race — the
single biggest contributor to the old multi-second "Reaching your home" stall —
and both ends now POST their offer/answer as soon as a usable candidate set is
gathered (host + server-reflexive, plus a relay candidate when TURN is
configured so symmetric-NAT owners still traverse) instead of waiting for full
ICE gathering. The Pi caches the relay ICE config and refreshes it hourly
instead of refetching it on every offer, retries use exponential backoff, and a
transient `/signal/ice` failure reuses the last good config instead of silently
dropping TURN.
