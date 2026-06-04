---
"forty-two-watts": minor
---

**Relay: the 4-digit code is now a one-time exchange for a session grant,
not a standing password.** Previously, once a pair session was approved,
anyone who got hold of the `/h/<token>/…` URL had full access for the
rest of the TTL — and for MCP that means powerful tools
(`run_command`, `modbus_write`, `deploy_driver`, `write_file`). A
forwarded or leaked-from-history URL was effectively a host handover.

Now, accepting the code mints a high-entropy session grant (32 bytes,
CSPRNG). It is handed to the friend exactly once:

- **MCP**: the landing page prints
  `claude mcp add ftw-friend --transport http <url>/h/<token>/mcp --header "Authorization: Bearer <grant>"`.
  `/h/<token>/mcp` now requires that Bearer grant.
- **Browser/dashboard**: approval sets an `HttpOnly; Secure;
  SameSite=Strict` `ftw_grant` cookie scoped to the session path;
  `/h/<token>/web/…` now requires it.

A leaked-but-already-active URL is useless without the grant — the
recipient lands back on the code-entry page and doesn't have the
out-of-band 4-digit code (5 wrong tries still locks it). The grant is
validated constant-time, never forwarded to the host, and expires with
the session. `POST /h/<token>/approve` now responds `200 {"grant":"…"}`
instead of `204`.

Works on the existing path-based routes — no subdomains or new domain
required (the browser-dashboard *rendering* fix and any subdomain work
remain deferred; see `docs/goals/relay-subdomain-sessions.md`).
