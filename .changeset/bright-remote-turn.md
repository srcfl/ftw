---
"forty-two-watts": minor
---

Make owner remote access reachable AND fast. The relay now publishes shared
ICE/TURN configuration (new `GET /signal/ice`, new `-ice-stun`/`-turn-url`/
`-turn-secret` flags) so hard-NAT/CGNAT owners can reach their Pi over a
TURN-relayed DTLS channel, while owner API writes stay on the fail-closed P2P
transport. Connection setup is also much snappier: the browser no longer arms
its cooldown on a transient cold-load (directory-not-yet-decrypted) race, both
ends return from ICE gathering as soon as a usable candidate pair exists
instead of waiting for full gather, the Pi caches the relay ICE config instead
of refetching it per offer, and the Pi resolves the browser's mDNS host
candidates so two devices on the same LAN connect directly without a STUN/WAN
round-trip.
