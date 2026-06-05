---
"forty-two-watts": minor
---

Home route: inline sign-in on the dashboard. When you open
`home.fortytwowatts.com` and aren't signed in, the dashboard now reveals a
discreet "Sign in with your passkey" banner (+ a header key) and runs the
passkey ceremony **in place**, over the same strict P2P channel — no redirect to
`/owner-access/login.html` (which would spawn a fresh channel with no session).
The dashboard is the door: you never have to hunt for the owner-access page.
LAN (bypass) views and already-signed-in sessions never see it. Re-checks whoami
when the P2P channel (re)connects, since the answer needs the channel up.
