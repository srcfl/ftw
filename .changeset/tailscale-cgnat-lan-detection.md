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

The Pi-side LAN-presence check (`isLANClientSource`) now recognises the same
CGNAT range, so owner-admin actions (manage passkeys, mint the setup PIN,
bootstrap the first passkey) work over Tailscale exactly as over an RFC1918 LAN.
An overlay you joined to your Pi is an explicit, authenticated owner decision —
genuine LAN presence — while the relay path stays excluded by the X-FTW-Tunnel
marker and the loopback check, so the friend pair-flow still cannot reach
owner-admin.
