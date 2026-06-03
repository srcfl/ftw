---
"forty-two-watts": minor
---

Owner remote access — **LAN-PIN first enrollment**. A short-lived 6-digit PIN,
readable only on the Pi's local network (`GET /api/owner-access/enroll-pin` —
`403` over the relay) and printed to the Pi's console, authorizes the very
first passkey enrollment through the relay origin. This resolves the deadlock
between the WebAuthn RP-ID origin requirement (the first passkey must be
created at `relay.fortytwowatts.com`) and the bootstrap hardening that blocks
un-authenticated first-enrollment over the tunnel. `enroll.html` gains an
optional PIN field. Once one passkey exists the PIN path is inert (further
enrollment requires a logged-in session).
