---
"forty-two-watts": patch
---

Reaching your dashboard over Tailscale no longer shows the passkey sign-in
gate. The browser's LAN-origin detection treated Tailscale's CGNAT addresses
(100.64.0.0/10, RFC 6598) as a public/relay origin, so it waited for a P2P
channel that a direct connection never opens and fell back to the sign-in gate
— while the same Pi reached over zerotier (192.168.0.0/16) sailed straight
through. The CGNAT range is now recognised as a direct-LAN origin in all three
copies of the check (`p2p.js`, `next-app.js`, `owner-access/owner-fetch.js`),
matching `p2p.js`'s own `isDirectLAN`. Direct IP access — LAN, zerotier, or
Tailscale — reaches the dashboard without a passkey prompt.
