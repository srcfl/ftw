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

`<token>.fortytwowatts.com` instead of `relay.fortytwowatts.com/h/<token>/`.

> **Decided (2026-05-28):** first-level tokens, `<token>.fortytwowatts.com`
> — covered by the existing Cloudflare Universal SSL `*.fortytwowatts.com`
> edge cert, no Advanced Certificate Manager needed. The control plane
> (`/tunnel/register`, `/tunnel/<host>/next`, …) stays on
> `relay.fortytwowatts.com`; per-session browser/MCP traffic lands on the
> wildcard. The relay process serves both, routing by `Host` header.

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
Browser ── https://<token>.fortytwowatts.com/        ─┐
                                                      ├─ tunnel ── ftw-pair → :8080
Claude  ── https://<token>.fortytwowatts.com/mcp     ─┘
```

- Relay routes on the `Host` header (`<token>.fortytwowatts.com` → token
  → host's long-poll queue) instead of on a path prefix.
  `relay.fortytwowatts.com` stays the control-plane host (register +
  long-poll); a known set of reserved labels (`relay`, `www`, `subetha`,
  the apex) is never treated as a session token.
- The host sees `/`, `/api/status`, `/style.css` — **normal paths**. No
  rewriting, no `<base>` injection, no frontend surgery.
- Landing/approval lives at the session root: `GET <token>.fortytwowatts.com/`
  shows the code-entry page when pending, the dashboard when active.
- Owner access gets the same treatment: `<site_id>.fortytwowatts.com`
  (site-id is already a single DNS-safe label) — one routing mechanism,
  not two.

## Security model — the code is a one-time exchange, not a password

OAuth authz-code → access-token, applied to the pair flow.

```
1. friend opens   https://<token>.fortytwowatts.com/   → pending: code-entry page
2. friend types the 4-digit code (received out-of-band from host)
3. relay verifies code ONCE, mints grant = 32 random bytes (base64url),
   marks token active, and:
     • Set-Cookie: ftw_grant=<grant>; HttpOnly; Secure; SameSite=Strict
       (host-only cookie → bound to THIS subdomain, isolated per session)
4. every request after activation requires the grant:
     • browser  → cookie carries it automatically
     • MCP      → Authorization: Bearer <grant> header, in the one-liner
                  the success page shows (confirmed supported, see below)
```

The MCP add command becomes:

```
claude mcp add ftw-friend --transport http \
  https://<token>.fortytwowatts.com/mcp \
  --header "Authorization: Bearer <grant>"
```

The relay validates the Bearer grant and does **not** forward it to the
host (it's a relay-side session secret). Keeping the grant in a header
rather than the URL keeps it out of proxy/access logs and shoulder-surf
range.

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

The 6-word token stays a low-entropy **routing key** (it's a DNS label
now, so treat it as fully public — it shows up in DNS queries, CT logs
of any per-name cert, and the friend's address bar). Security rests on
the grant, not the token.

## Work breakdown — distinct PRs

1. **Relay: Host-header routing.** Parse `Host`; `<token>.fortytwowatts.com`
   → token lookup → host's long-poll queue. Reserved labels (`relay`,
   `www`, `subetha`, apex) never resolve to a token. Keep the
   `/h/<token>/…` path family working through one release for backward
   compat, then delete. Unit-test the Host parser (reserved labels,
   unknown token, port stripping, IDN/case).
2. **Relay: grant exchange + gate.** `approve` mints the grant, sets the
   cookie, and returns the grant so ftw-pair can surface the Bearer
   one-liner. `web`/`mcp` handlers require grant (cookie for browser,
   `Authorization: Bearer` for MCP). Test: forwarded-URL-without-cookie →
   back to code entry; right cookie → through; wrong/expired/revoked
   grant → 401; grant never forwarded to host.
3. **Infra: wildcard DNS + Origin cert** (operator task, CF dashboard —
   see open question 1). Edge cert already covers it; the CF→VM Origin
   cert needs regenerating to include `*.fortytwowatts.com`.
4. **ftw-pair: surface the subdomain URL** in `/api/pair/status` and the
   pair-card (replaces the `/h/<token>` form). Post-approval the card
   shows the browser URL + the `--header "Authorization: Bearer <grant>"`
   MCP one-liner.
5. **Dashboard "shared session" banner** (carried over from
   relay-as-tunnel Phase 2 follow-up): 1px amber hairline, token last
   word + relative TTL, per `DESIGN.md`.

## Open questions

1. ~~**Edge cert for a two-level wildcard.**~~ **Resolved (2026-05-28):**
   first-level `<token>.fortytwowatts.com`, covered by the existing
   Cloudflare Universal SSL `*.fortytwowatts.com` edge cert — free, no
   ACM. Cost accepted: the token namespace shares the apex, so every
   future first-level subdomain (`app`, `docs`, …) needs an explicit DNS
   record (more specific than the wildcard) before it works, and a typo
   subdomain lands on the relay's "unknown session" page. Infra to-do
   for PR 3: add proxied wildcard `*.fortytwowatts.com` A record → relay
   VM, and regenerate the CF Origin CA cert to include
   `*.fortytwowatts.com` (currently `relay.fortytwowatts.com` only).
2. ~~**Does `claude mcp add --transport http` support a custom header?**~~
   **Resolved (2026-05-28):** yes — `--header`/`-H` is documented and the
   help text shows an `Authorization: Bearer …` example verbatim. Use the
   Bearer header for the MCP grant (no URL-embedded `/g/<grant>/` path).
   OAuth flags exist too (`--client-id`, `--client-secret`) but Bearer is
   the right weight for our minted grant.
3. **HSTS preload interaction.** Apex is (or will be) preloaded →
   every subdomain must serve valid HTTPS forever. The wildcard edge
   cert satisfies this, but confirm before enabling preload that the
   wildcard is live, or session subdomains will hard-fail in browsers.
4. **Grant lifetime vs token TTL.** Grant should expire with the token
   (session TTL), but should a browser refresh renew it (sliding) or
   hard-expire at TTL? Lean hard-expire — the TTL is the operator's
   stated intent; sliding would silently extend it.
5. **Per-session isolation of the grant cookie.** Host-only cookie on
   `<token>.fortytwowatts.com` is isolated by construction (not sent to
   sibling subdomains). Confirm no `Domain=.fortytwowatts.com` slips in
   during implementation — that would leak the grant across every
   session AND to the apex dashboard.
