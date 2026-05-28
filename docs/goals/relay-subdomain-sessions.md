# Relay subdomain-per-session: make the tunneled dashboard actually work + fix the access grant

## Thesis

Two things shipped half-done in the relay-as-tunnel work (`relay-as-tunnel.md`):

1. **The tunneled dashboard never rendered.** `/h/<token>/web/` returns
   the host's `index.html` (200, 43 KB) but the page is blank — every
   asset and every `fetch` is root-absolute and resolves against the
   relay root instead of the session prefix.
2. **The access grant is weaker than the doc claims.** Phase 2 says
   "the browser already has the approval cookie from the landing-page
   handshake." It doesn't — no cookie is ever set. After the one-time
   4-digit approval, the 6-word URL *alone* grants access for the whole
   TTL. A forwarded link is a full session handover.

Both dissolve if we stop path-prefixing an app that assumes it owns its
origin, and give each session its own origin: a **subdomain**.

`<token>.relay.fortytwowatts.com` instead of `relay.fortytwowatts.com/h/<token>/`.

Simple over clever: the dashboard already works on `/` — let it keep
thinking it owns `/`. The cookie security model we wanted becomes free,
because cookies scope to the origin.

## Today — why it's broken

```
Browser → https://relay.fortytwowatts.com/h/<token>/web/
            │  relay strips "/h/<token>/web", tunnels "/" to host:8080
            ▼
host returns index.html with:
   <link href="/style.css">          ← browser fetches relay.../style.css   → 404
   <script src="/components/index.js">← browser fetches relay.../index.js     → 404
   fetch('/api/status')              ← browser fetches relay.../api/status   → 404
```

The relay can rewrite the handful of `<link>/<script>` tags in static
HTML, but it cannot rewrite the dozens of runtime `fetch('/api/...')`
calls scattered across `web/*.js` without a fragile monkeypatch shim.
The frontend has **zero** base-path awareness (`grep fetch\( web/` → all
absolute). This affects owner-access (`/me/<site_id>/`) identically —
that path serves the same dashboard and is equally blank today; only the
`/owner-access/` login ceremony works there.

## Tomorrow — subdomain per session

```
friend laptop                         relay (HTTPS, routes by Host)        host (Pi)
─────────────                         ────────────────────────────         ─────────
Browser ── https://<token>.relay.fortytwowatts.com/        ─┐
                                                            ├─ tunnel ── ftw-pair → :8080
Claude  ── https://<token>.relay.fortytwowatts.com/mcp     ─┘
```

- Relay routes on the `Host` header (`<token>.relay…` → token → host's
  long-poll queue) instead of on a path prefix.
- The host sees `/`, `/api/status`, `/style.css` — **normal paths**. No
  rewriting, no `<base>` injection, no frontend surgery.
- Landing/approval lives at the session root: `GET <token>.relay…/`
  shows the code-entry page when pending, the dashboard when active.
- Owner access gets the same treatment: `<site_id>.relay…` (or a
  reserved `me-<site_id>` prefix) — one routing mechanism, not two.

## Security model — the code is a one-time exchange, not a password

OAuth authz-code → access-token, applied to the pair flow.

```
1. friend opens   https://<token>.relay…/         → pending: code-entry page
2. friend types the 4-digit code (received out-of-band from host)
3. relay verifies code ONCE, mints grant = 32 random bytes (base64url),
   marks token active, and:
     • Set-Cookie: ftw_grant=<grant>; HttpOnly; Secure; SameSite=Strict
       (host-only cookie → bound to THIS subdomain, isolated per session)
4. every request after activation requires the grant:
     • browser  → cookie carries it automatically
     • MCP      → no cookies; grant rides the URL the success page shows:
                  https://<token>.relay…/g/<grant>/mcp
```

What this buys:

- **A forwarded URL is useless.** No cookie → recipient lands on the
  code-entry page and doesn't have the out-of-band 4-digit code
  (5 wrong attempts → locked, existing `MaxApprovalAttempts`).
- **No IP pinning** — survives mobile carrier NAT, VPN, network hops.
- **Revocation is trivial** — drop the grant server-side.
- **The code regains meaning** — it bootstraps a real session secret
  instead of being a one-shot check that gates nothing afterward.
- Composes cleanly with Phase 3 passkeys: same grant-cookie shape, the
  WebAuthn assertion just replaces the 4-digit code as what mints it.

The 6-word token stays a low-entropy **routing key** (it's in DNS now,
so treat it as fully public). Security rests on the grant, not the token.

## Work breakdown — distinct PRs

1. **Relay: Host-header routing.** Add `<token>.relay…` → token lookup
   alongside (or replacing) the `/h/<token>/…` path family. Keep the
   path family working through one release for backward compat, then
   delete. Unit-test the Host parser (reject apex, reject unknown
   token, strip port).
2. **Relay: grant exchange + gate.** `approve` mints the grant, sets the
   cookie, returns the MCP `/g/<grant>/…` URL. `web`/`mcp` handlers
   require grant (cookie or path). Test: forwarded-URL-without-cookie →
   back to code entry; right cookie → through; revoked grant → 401.
3. **Infra: wildcard DNS + edge cert** (operator task, CF dashboard —
   see open question 1).
4. **ftw-pair: surface the subdomain URL** in `/api/pair/status` and the
   pair-card (replaces the `/h/<token>` form). MCP one-liner shows the
   `/g/<grant>/mcp` URL post-approval.
5. **Dashboard "shared session" banner** (carried over from
   relay-as-tunnel Phase 2 follow-up): 1px amber hairline, token last
   word + relative TTL, per `DESIGN.md`.

## Open questions

1. **Edge cert for a two-level wildcard.** `<token>.relay.fortytwowatts.com`
   is a second-level subdomain. Cloudflare Universal SSL covers
   `fortytwowatts.com` + `*.fortytwowatts.com` (first level) only — it
   does **not** cover `*.relay.fortytwowatts.com`. Options:
   - (a) **Advanced Certificate Manager** (~$10/mo) to issue
     `*.relay.fortytwowatts.com` at the edge. Cleanest namespacing.
   - (b) **First-level tokens**: `<token>.fortytwowatts.com`, covered by
     the existing Universal cert. Free, but the routing key squats on
     the apex namespace (collides with future `www`, `app`, …).
   The Origin cert (CF→VM) is unconstrained — CF Origin CA issues
   arbitrary wildcards, so the VM side is fine either way.
   **Recommendation: (a)** — keep the relay namespace clean; $10/mo is
   noise. Decide before PR 3.
2. **Does Claude Code's `claude mcp add --transport http` support a
   custom header?** If yes, prefer `Authorization: Bearer <grant>` over
   a URL-embedded `/g/<grant>/`. URL-embedded works regardless and is
   the safe default; header is the nicer upgrade if supported.
3. **HSTS preload interaction.** Apex is (or will be) preloaded →
   every subdomain must serve valid HTTPS forever. The wildcard edge
   cert satisfies this, but confirm before enabling preload that the
   wildcard is live, or session subdomains will hard-fail in browsers.
4. **Grant lifetime vs token TTL.** Grant should expire with the token
   (session TTL), but should a browser refresh renew it (sliding) or
   hard-expire at TTL? Lean hard-expire — the TTL is the operator's
   stated intent; sliding would silently extend it.
5. **Per-session isolation of the grant cookie.** Host-only cookie on
   `<token>.relay…` is isolated by construction (not sent to sibling
   subdomains). Confirm no `Domain=.relay…` slips in during
   implementation — that would leak the grant across sessions.
