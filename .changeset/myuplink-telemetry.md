---
"forty-two-watts": minor
---

Add a read-only MyUplink heat-pump telemetry driver (`drivers/myuplink.lua`).
Authenticates to the MyUplink Cloud REST API v2 (OAuth2 client_credentials,
READSYSTEM scope) and emits compressor power and hot-water / indoor / outdoor
temperatures into the time-series DB. Observe-only — no control. Configure the
Client ID in Settings → Devices; the Client Secret is stored as a masked
config secret.
