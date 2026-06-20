---
"forty-two-watts": minor
---

MyUplink heat-pump driver now uses the authorization-code OAuth flow the
MyUplink developer portal actually supports, fixing the `invalid_client`
startup failure (the old `client_credentials` grant is not offered for portal
apps). A new in-app consent flow (Settings → Devices → "Connect to MyUplink")
handles the one-time browser sign-in, stores the refresh token as a masked
secret, and keeps it fresh — the driver runs `grant_type=refresh_token` at
runtime and persists Azure-B2C-rotated tokens via the new `host.persist_secret`
capability so they survive restarts. A manual fallback (paste the redirected
URL) completes the consent on origins where the auto-callback can't reach the
Pi (relay/home route, or an http LAN callback the portal rejects) — the Pi
exchanges the code over its own outbound HTTPS, so no inbound callback is
needed.

A new **Heat pump** dashboard card surfaces the driver's telemetry (compressor
power + hot-water / indoor / outdoor temperatures, with a 24h power sparkline).

Note: the driver has no `mode` field — it is read-only telemetry for one
physical pump.
