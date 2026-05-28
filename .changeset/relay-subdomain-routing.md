---
"forty-two-watts": patch
---

**Relay: subdomain-per-session routing (PR 1 of the subdomain track).**
`ftw-relay` now accepts an optional `-base-domain` flag (e.g.
`fortytwowatts.com`). When set, a request whose `Host` is
`<token>.<base-domain>` is served as that pair session — the dashboard
sees verbatim root-absolute paths (`/api/status`, `/style.css`) instead
of the `/h/<token>/web` prefix that the frontend's absolute paths can't
survive. The legacy `/h/<token>/…` path family keeps working unchanged
alongside it for one release. With the flag unset (default), Host routing
is off and nothing changes — local/dev and existing deploys are
unaffected until the wildcard DNS + cert land.

Also fixes a latent bug in the existing path-mode web tunnel: query
strings (`/api/history?range=24h&points=288`) are now preserved through
the tunnel instead of being dropped, and the friend landing page builds
its approve/dashboard/MCP URLs from explicit server-injected paths rather
than a positional token argument (structurally prevents the
"Wrong code" arg-order class of bug).

See `docs/goals/relay-subdomain-sessions.md`. Grant-exchange auth is PR 2.
