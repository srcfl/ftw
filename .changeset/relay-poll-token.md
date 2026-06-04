---
"forty-two-watts": patch
---

Relay: authenticate the host long-poll with a per-host **poll token** — closes
the `host_id`-race flagged during the owner-access hardening review.

`POST /me/register` (ES256-signed) and `POST /tunnel/register` now return a poll
token that the Pi / pair sidecar must present (header `X-FTW-Poll`) on
`GET /tunnel/{host_id}/next` and `POST /tunnel/{host_id}/response/{req_id}`. The
relay verifies it constant-time and rejects unknown-host / wrong-token polls. So
a caller that merely learns a host's `host_id` can no longer poll for (and steal)
its tunneled traffic — which carries the owner's session cookie — and an
unregistered `host_id` can't create long-poll waiters at all. Tokens are minted
on the verified registration, refresh on re-registration (so they survive a relay
restart re-mint), and are GC'd after going unused.

Operators: upgrade the relay and the Pi together — the hardened relay requires
the token the updated Pi/sidecar sends.
