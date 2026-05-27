---
"forty-two-watts": minor
---

**Replace `ftw-connect` with a URL on `relay.fortytwowatts.com`.** Friend opens a browser to `/h/<6-word-token>`, sees a 4-digit code, reads it to the host on voice, host clicks Allow on the dashboard. Then both Claude Code (`--transport http https://relay.../h/<token>/mcp`) and the web dashboard (`/h/<token>/web/`) work for the rest of the TTL.

Under the hood: new `ftw-relay` HTTPS request-response relay (linux/amd64 + linux/arm64 release assets), new `internal/tunnel` long-poll protocol, rewired `ftw-pair` host loop. Deletes `ftw-connect`, `ftw-subetha`, `internal/subetha`, the curl installer script, and the old `docs/subetha-deploy.md` runbook. Operator deploys the new relay per `docs/relay-deploy.md` (Cloudflare Origin Cert + systemd, ~15 min).

Known temporary regression: the dashboard's "friend connected" counter always shows 0 until a follow-up PR wires it through a new relay-side sessions endpoint.
