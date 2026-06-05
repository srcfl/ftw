---
"forty-two-watts": minor
---

Home route: a real sign-in **gate** + inline passkey login (the dashboard IS the
door). When you open `home.fortytwowatts.com` and aren't signed in, the dashboard
is fully covered by a clean sign-in card ("Reaching your home…" → "Sign in with
your passkey") instead of the empty dashboard chrome — which previously rendered
"No devices configured / run the setup wizard" to logged-out visitors and falsely
read as an unconfigured instance. The passkey ceremony runs in place over the same
strict P2P channel (`ownerFetch` / FIX-B) — no redirect to
`/owner-access/login.html`. Never shown on the LAN (bypass) or once signed in; the
"no devices" prompt is suppressed while logged out. No owner DATA is ever served
unauthenticated — the gate is purely the lock's UI.

Also: the transport indicator is now purely informational (it explains direct vs
relayed vs connecting) rather than a click-to-toggle that, on the P2P-only route,
just broke the channel. Part of the #438 seamless-UX layer (device-key silent
re-auth still to come).
