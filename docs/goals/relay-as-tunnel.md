# Relay-as-tunnel: drop `ftw-connect`, add web proxy, extend to owner remote access
## Thesis
The host already opens an outbound tunnel to the relay. Everything we want — MCP for AI, dashboard for humans, eventually owner remote access — can ride that single tunnel. The friend's side becomes "open a URL" instead of "install a binary".

Simple is king. Three phases, one primitive (a multiplexed HTTP tunnel), one auth concept (a token grants access to one host for one TTL).
## Today
```
friend laptop                   relay                       host (Pi)
─────────────                   ─────                       ─────────
ftw-connect ── ChaCha20-frames ── subetha ── ChaCha20-frames ── ftw-pair
   │                                                              │
 stdio MCP                                                    HTTP :8090
   │                                                              │
Claude Code ◄─ tools                                       MCP server
```

Relay is a dumb byte pipe. Friend needs a binary installed locally that does the crypto + exposes stdio MCP to their Claude Code.
## Tomorrow
```
friend laptop                       relay (HTTPS terminator)        host (Pi)
─────────────                       ───────────────────────         ─────────
Claude Code ── MCP/HTTP ──┐
                          ├── https://relay.fortytwowatts.com/h/<token>/{mcp,web,...} ── ftw-pair
Browser     ── HTTPS ─────┘                                            │
                                                                  serves :8080
                                                                  + MCP server
```

Friend opens a URL. No install. The relay terminates TLS and routes into the host's existing outbound tunnel. The token in the URL is both the rendezvous key and the access grant.
## Security model — explicit choices
**Threat model (what we are defending against):**

1. **URL leaks in transit** — Signal message forwarded, Slack-archived, quoted in a bug report.
  
2. **URL leaks at rest** — friend's browser history, screen-shared during a debug call, pasted to the wrong window.
  
3. **Spam against the host** — attacker with a leaked URL hammers approval requests at you.
  
4. **Phishing** — fake relay domain (`re1ay.fortytwowatts.com`, `relay-fortytwowatts.com`) tricks the friend.
  
5. **Compromised friend device** — out of scope; if Erik's laptop is owned, the session goes wherever the attacker wants.
  
6. **Compromised relay operator (you)** — already conceded; not in scope for v1.
  

**Access model — token + live host approval ("Pattern B"):**

The URL token is a **routing key**, not the access grant. Opening the URL puts the relay into a "pending" state for that token; it does not yet route any traffic to the host. The host has to approve the connect attempt before the session activates.

Flow:

1. Host generates a token via the dashboard pair card. Token = 6 BIP39 words (66 bits — unguessable against any rate-limited attacker; routing-only, so we don't need 128).
  
2. Host sends the URL to the friend over Signal / SMS / Slack.
  
3. Friend opens `https://relay.fortytwowatts.com/h/<token>`. Relay shows a landing page with a 4-digit code: "Tell the host this code: **4827**." Browser is now pending.
  
4. Friend reads the code to the host on voice (phone, Signal voice call). The voice channel is the out-of-band cross-check — an attacker with a leaked URL doesn't have a voice line to you.
  
5. Host sees the same 4-digit code on the dashboard with the friend's claimed name + IP + geo: "Someone connecting. Code: 4827. Allow?" Host clicks Allow.
  
6. Relay flips the token from "pending" to "active". From this point, MCP and web access both work for the TTL window — Claude Code can connect via `claude mcp add ...`, the friend's browser sees the dashboard, all over the same token.
  
7. Host can revoke at any time from the dashboard. TTL is a hard stop.
  

This defeats threats #1–#3 with one mechanism. Threat #4 needs separate handling (see "Domain + phishing").

**What we give up:**

- E2E encryption host↔friend. The relay terminates TLS and sees plaintext MCP + HTTP. Today subetha cannot see what flows; tomorrow it can.
  

**Why that trade is OK for "help a friend":**

- Friend already trusts the operator (you). The relay is your relay.
  
- The threat E2E protected against was "compromised relay operator reads your tools/credentials". In a help-a-friend flow, if your relay is compromised the friend has bigger problems (you might never see the traffic at all — relay could drop or rewrite arbitrarily).
  
- For zero-trust deployments (federated relays, third-party operators), we'd revisit. Not in scope for v1.
  

**Token discipline:**

- Token lives in URL path (`/h/<token>/...`), never query string, never headers in screenshots. Browsers may log paths; the routing-key-only design means a leaked URL is useless without a parallel voice channel to you.
  
- Relay rate-limits token validation: a single token gets N failed approval-code attempts before it's blacklisted for the rest of its TTL. Brute force is already infeasible (2^66) but rate-limit costs nothing.
  
- Approval rate-limit on host side too: max M pending approvals per minute per token. After M, token enters "abuse-suspected" state and host gets a single consolidated toast instead of M separate ones.
  
- HSTS + HSTS preload + TLS 1.3 only on the relay. No HTTP fallback. Apex `fortytwowatts.com` redirects to canonical relay paths so typo-squatters can't claim the bare apex.
  
- Token TTL default 1h, max 24h. No "permanent" tokens in this flow. Approval lasts the TTL; once approved, friend doesn't re-approve each connect within the session.
  

**Domain + phishing (threat #4):**

- The relay lives on the company-owned domain `relay.fortytwowatts.com` (apex + subdomain owned by you). The pair-card UI prints the full URL prominently with the domain in bold so the friend learns to recognise it.
  
- Add a "verify this is real" line in the help-a-friend onboarding: "The URL must end with `.fortytwowatts.com`. Look-alike domains like `fortytwowatts-help.com` or `relay-fortytwowatts.com` are NOT us." One sentence in the docs, host UI repeats it.
  
- Browser-side: passkey / cookie scoping naturally binds to the exact domain, so a phishing site can't replay either.
  

**Web proxy specifics:**

- Same-origin to the relay domain: cookies set by the dashboard go to `relay.fortytwowatts.com`, scoped to the token path. They do NOT leak to other tokens' paths (cookie Path attribute on the token segment).
  
- The dashboard runs auth-less on LAN. For a paired session that's intentional — the token + host approval are the auth gate. Document that explicitly so operators understand "anyone the host approved can see what's on the dashboard until TTL expires".
  
- WebSockets + SSE work over HTTP/1.1 Upgrade — the tunnel protocol must pass these through transparently (relay = transparent reverse proxy, not a smart MCP-aware proxy).
  
## Phase 1 — MCP via relay (replaces `ftw-connect`)
**Scope:** the friend's Claude Code talks MCP directly to the relay URL. The binary + install script are deleted.

**Host changes:**

- `ftw-pair` already terminates MCP locally. Wrap its tunnel client to multiplex MCP framed messages over the existing subetha connection (or replace subetha framing with HTTP/2 streams — see "Tunnel protocol" below).
  

**Relay changes:**

- New HTTPS endpoint family `/h/<token>/{,mcp,web/...}` — landing page at the root, MCP HTTP streamable transport at `/mcp`, transparent reverse-proxy at `/web/`. All three gated by the same approval state machine (pending → active → expired/revoked). Validates token against active host registrations; routes request body through the tunnel to the host's `ftw-pair`.
  
- Per-token connection limit: one active MCP client at a time. Reject the second; surface that on the host dashboard.
  

**Friend UX:**

1. Friend opens `https://relay.fortytwowatts.com/h/garage-coffee-river-bicycle-window-cat` in a browser **first** — that's the approval handshake. Lands on a page that shows the 4-digit code, the operator's claimed identity, and a "tell the host this code on voice" hint.
  
2. Friend reads code to host on voice. Host clicks Allow.
  
3. The landing page now shows two copy-paste blocks:
  
  ```bash
  # For Claude Code:
  claude mcp add ftw-friend --transport http \
    https://relay.fortytwowatts.com/h/garage-coffee-river-bicycle-window-cat/mcp
  
  # For browser dashboard (Phase 2):
  https://relay.fortytwowatts.com/h/garage-coffee-river-bicycle-window-cat/web/
  ```
  

No install. The browser approval-handshake step is the one-time gate; after that any MCP/HTTP client can use the active token until TTL.

**Deletions:**

- `go/cmd/ftw-connect/` — gone.
  
- `scripts/install-ftw-connect.sh` — gone.
  
- ftw-connect binary builds from `release.yml` — gone (5 fewer build matrix entries; fewer release assets per cut).
  
- `docs/ftw-pair.md` updated to show the URL flow.
  
## Phase 2 — Web dashboard via relay (the new ask)
**Scope:** the friend opens a browser to `https://relay.fortytwowatts.com/h/<token>/web/` and sees the host's dashboard. Same token, same approved session.

**Host changes:**

- The tunnel grows from "MCP only" to "arbitrary HTTP". The host side reverse-proxies the relay's tunneled requests to `http://localhost:8080`. (The main `forty-two-watts` process serves the dashboard — `ftw-pair` becomes an HTTP forwarder, not just an MCP terminator.)
  
- Pair-card on the dashboard surfaces the web URL alongside the MCP one. One-click copy. Same `/h/<token>/{mcp,web}` family.
  

**Relay changes:**

- `/h/<token>/web/` (and everything under it) reverse-proxies to host port 8080. Strip the `/h/<token>/web` prefix when forwarding so the dashboard sees normal paths.
  
- WebSocket + SSE pass-through. The dashboard uses SSE for live telemetry; that's the load case that must work.
  
- Approval gate is shared with Phase 1 — by the time `/web/` is reached, the token is already in "active" state. The browser already has the approval cookie from the landing-page handshake; no second approval step.
  

**Security follow-ups:**

- Add a banner on the dashboard when served through a pair session: "You are viewing this dashboard through a shared session. Token: …. Expires ." So friend cannot forget they're in someone's home.
  
- Banner uses `--accent-e` per `DESIGN.md` — same amber as other warnings, single 1px hairline, no shadow.
  
## Phase 3 — Owner remote access (extension)
**Scope:** operator away from home reaches their own instance over the same relay primitive. No BIP39 token (too short-lived). Uses a **passkey** (WebAuthn) bound to the operator's devices.

**Why passkey, not bare password or ES256-key-file:**

- **Phishing-resistant by construction**: a passkey created for `relay.fortytwowatts.com` cannot be used against `re1ay.fortytwowatts.com`. The browser refuses. This is the strongest defense we have against threat #4 and it's free with the platform.
  
- **No shared secret on relay/host**: only public keys are stored. Relay compromise leaks public keys, not credentials.
  
- **Native UX on every modern OS**: Touch ID on Mac, Windows Hello, Android keystore, iOS keychain. One tap, no copy-paste-password games. Synced across the operator's Apple/Google account so a lost laptop doesn't lock you out of the system.
  
- **It IS ES256 under the hood** — passkeys in browsers use the same primitive as Nova federation today (`go/internal/nova/`). We reuse the same signature-verification path on the host side.
  

**Auth model:**

- **Enrollment (one-time, LAN-only, requires physical presence at the Pi):** Open dashboard from the Pi's LAN address, click "Add this device". Browser triggers WebAuthn registration — Touch ID / Windows Hello / OS-native UI runs. Host stores the public key + a friendly name ("Fredrik's MacBook", "iPhone 15") in `state.db.trusted_devices`. Repeat per device.
  
- **Remote login:** Operator opens `https://relay.fortytwowatts.com/me/<site-id>`. Relay forwards a WebAuthn challenge through the tunnel to the host. Host signs nothing — the _browser_ signs with the private key kept in the device keystore. Relay forwards the assertion through the tunnel; host validates against `trusted_devices`, returns a session bearer. From here, `/me/<site-id>/{web,mcp}/` works just like Phase 2's `/h/<token>/{web,mcp}/` did.
  
- **Host-id is public.** It's the existing Nova federation site-id (`site_<base32>`), not a secret. There is no security value in hiding which site you operate; the passkey is what gates access.
  

**Recovery — what if every passkey-device is lost:**

- Hard problem. Recovery options in priority order:
  
  1. **Passkey sync** (default for Apple/Google passkeys) — replacing the device pulls the passkey back from iCloud Keychain / Google Password Manager. No host involvement needed.
    
  2. **Add-second-device-while-you-still-have-one** — anytime you add a new laptop or phone, enroll a passkey on it via the LAN flow. Keep at least two enrolled. This is the recommended posture; UI nags if you only have one.
    
  3. **TOTP-protected recovery code** — at enrollment, host generates a one-time recovery code, displays it once, prompts the operator to print or password-manager-store it. Used only when (1) and (2) both fail. Stored as Argon2id hash on host. Combined with a TOTP factor configured at the same time, so a leaked recovery code alone is not enough.
    
  
  Password + TOTP is the _recovery_ path, not the daily-driver path. We don't ship a "log in with password" form on the relay for the same reason banks no longer let you reset a password with just an email — phishing.
  

**Owner UX:**

```
https://relay.fortytwowatts.com/me/site_3fqk7xpm9n2tv8y4
```

- First visit on a new browser: browser asks "Sign in with your passkey?" — Touch ID, in.
  
- Subsequent visits: same one-tap.
  
- Dashboard surfaces the URL as a copy-paste QR on the "Connected devices" page, so adding a phone is "scan QR, register passkey".
  

**Out of scope for this doc:**

- Multi-operator support (Erik also operates Fredrik's Pi remotely), shared-household scenarios, mobile app push notifications. Different product surface; Phase 3 just makes the existing dashboard + MCP reachable for the single owner.
  
## Tunnel protocol — request-response, long-polling

The tunnel today is "ChaCha20-Poly1305 length-prefixed frames over plain TCP". For phases 1+2+3 we replace it with **HTTP request-response, long-polled from the host**. No persistent multiplexing layer, no HTTP/2 server-push, no bespoke framing. Two endpoints on the relay; standard `net/http` everywhere.

**Protocol — host side:**

The host runs a single goroutine in a loop:

```
GET  /tunnel/<host-id>/next        ── long-poll up to 30s for a queued request
                                      response body = full HTTP request to forward, plus a req-id header
POST /tunnel/<host-id>/response/<req-id>
                                   ── body = full HTTP response from local :8080 / MCP / etc
```

When the GET returns a request, the host forwards it to the appropriate local service (MCP sidecar, dashboard on :8080), captures the response, POSTs it back tagged with the same req-id. Loop continues.

**Protocol — relay side:**

The relay maintains a per-host queue of pending requests. When a friend hits `/h/<token>/...`, the relay enqueues the request for that token's host, holds the friend's HTTP connection open, waits for the host's POST `/response/<req-id>`, and returns the response body to the friend.

That's the whole protocol. Two endpoints. No streams. No state machine beyond a request queue.

**SSE / WebSocket handling:**

The dashboard uses SSE for live telemetry. SSE doesn't fit request-response cleanly — a single response that lasts minutes and emits many events doesn't match "POST one response body and you're done."

**Decision: degrade SSE → polling when the dashboard is served via relay.** The dashboard already needs to know it's running in a paired session (for the warning banner from Phase 2). When that's true, swap the SSE consumer for a 1-2s poll on the same endpoint family. ~50 lines of JS in `web/`. Live telemetry is 1-2s stale instead of pushed — invisible for a remote operator, perfectly adequate for a friend debugging.

Concretely the dashboard's existing `EventSource('/api/...')` calls become a small `pollingEventSource(...)` shim that fetches `/api/.../latest` on a timer when `location.hostname === 'relay.fortytwowatts.com'`.

WebSocket: same story. None of the current dashboard endpoints use WebSocket — only SSE. If a future feature needs WebSocket, we'll revisit then.

**Why this is the right call:**

- MCP IS request-response (JSON-RPC over HTTP). The natural fit.
- No multiplexing library to maintain.
- Works through any HTTP infra — Caddy, nginx, Cloudflare, plain Go `http.Server`. We could swap relay implementations transparently.
- Trivial to test: two `curl` commands exercise the whole tunnel.
- Trivial to reason about errors — every request has a clear lifecycle, no half-closed streams.

**What we lose:**

- Push-style live SSE for the remote dashboard. We accept the polling fallback for the relay-served case; LAN access keeps native SSE. That's a one-off cost for a permanent simplicity win.

**Plan B (if we ever regret the polling fallback):**

Extend the protocol with a streaming response variant — host POSTs chunked responses tagged with the same req-id, relay forwards each chunk to the still-open friend connection. Same two endpoints, with a chunked-encoding flag. Defer until there's a real complaint; YAGNI for v1.
## Migration

No field installs depend on `ftw-connect` today, so we skip the backwards-compat dance entirely:

- Phase 1 ships and the old `ftw-connect` + `scripts/install-ftw-connect.sh` + `go/internal/subetha/` are deleted in the same PR. The `ftw-subetha` raw-TCP relay on `subetha.fortytwowatts.com:7777` stays up for one release as a safety net, then is decommissioned per `docs/relay-deploy.md`.

- Phase 2 is purely additive — adds the `/h/<token>/web/` reverse-proxy path on top of phase 1's tunnel.

- Phase 3 ships behind a feature flag (`owner_remote_access` on the dashboard) so the trusted-devices UI doesn't appear until the relay-side passkey support is live.

DNS + TLS for the new relay are described in `docs/relay-deploy.md`. The Cloudflare Origin Certificate covers `*.fortytwowatts.com` until 2041; no automated renewal needed.
## What this lets us delete
After phase 2 lands:

- `go/cmd/ftw-connect/` (~600 lines)
  
- `scripts/install-ftw-connect.sh` (~190 lines)
  
- `go/internal/subetha/` ChaCha20-Poly1305 + length-prefix framing (~400 lines) — replaced by stock `net/http` on both sides
  
- 5 release-asset build matrix entries
  
- 2 README + 1 `docs/ftw-pair.md` install paragraph
  

Net: simpler relay, simpler host, fewer artifacts to ship, no friend-side install. **Stronger** security posture than today for the help-a-friend case — voice-channel cross-check via the 4-digit approval code is a real second factor; today's design only has the BIP39 token, which protects against guessing but not against URL leakage.
## Open questions
1. **Approval-prompt placement on the host.** Persistent dashboard toast is the obvious one. Worth pushing through MQTT to Home Assistant as a notification too? Push to phone (would need a Sourceful push channel — Telegram already on the table, see `telegram:configure` skill).
  
2. **Approval timeout.** Friend opens URL → relay shows code → friend reads to host → host clicks Allow. How long does the "pending" state hold open before relay times out and asks friend to refresh? Suggestion: 2 minutes hard timeout, friend's page polls / SSE for state change.
  
3. **Token-lifecycle UI on host.** Show approved-but-not-yet-revoked tokens with friend name, IP, last activity, time-to-TTL. One-click revoke. Probably belongs on the same pair-card.
  
4. **Self-host federation.** Do we run the relay only, or document how a community member can self-host? If self-host, the conceded-E2E becomes a real concern again. Likely answer: v1 = we run it, v2 = doc the self-host path with a clear "you trust this relay operator" warning.
  
5. **Phishing-resistance verification.** Does `fortytwowatts.com` qualify for HSTS preload submission? (Check `hstspreload.org` rules — needs canonical apex redirect, valid TLS, full subdomain coverage.)
  
6. **Banner design in** `DESIGN.md` **terms.** Single 1px amber hairline at the top of the dashboard during shared sessions, with token last-word + relative-TTL. Sketch needed before implementation.
  
7. **What happens to** `internal/nova` **ES256 code** — keep it as is for federation telemetry signing (separate concern), or refactor so passkey verification reuses the same primitive? Probably the latter, but worth checking the existing JWT/JWS code first.
