# Overnight goal bug-hunt — forty-two-watts P2P-only home route

**For an autonomous overnight agent (Codex CLI + Docker + this repo). Runs
unattended; files a GitHub issue per CONFIRMED bug; hands off to humans in the
morning.**

## 0. Mission

You are an adversarial **security + reliability auditor**. The GOAL: find real,
**exploitable or reproducible** bugs in the new **P2P-only home route** (the
trustless remote-access transport) BEFORE it ships to production and gets
deployed to a real relay + a real Pi.

You **find, reproduce, and report** — you do **not** fix, merge, or deploy. A
finding without a concrete repro is "worth a second look", not an issue.

Bias: **false-negative-averse on security** (flag anything that could break an
invariant), but **only file an issue once you have a concrete repro** (a curl
script, a Playwright snippet, a Go test, or a precise code+trace argument). Quote
`file:line`.

## 1. Setup

```bash
git fetch origin
git checkout p2p-only-home-route
git pull                      # branch under test: slices 1-6 + docker harness + security hardening
make e2e-docker-up            # relay + Pi on the docker bridge net
make e2e-docker-tier2         # headless-Chromium: virtual-passkey login + DIRECT P2P proof
```

- If `make e2e-docker-tier2` is RED on a clean checkout, **that is finding #0** —
  the headline regression. Capture it first.
- Static analysis: `codex exec -s read-only -C . -o /tmp/pass.md "<prompt>"`.
- Dynamic repros: curl/scripts against the running harness (`http://localhost:8080`
  = Pi, `http://home.fortytwowatts.localhost/` = relay home route, relay also on
  `:80`), and Playwright in `deploy/local-e2e/tier2/`.
- The harness uses TOFU (`-home-allow-tofu`) and host-only ICE (`FTW_P2P_STUN=none`).

## 2. System under test (one paragraph)

The owner's remote dashboard rides a **direct browser↔Pi WebRTC DataChannel
(DTLS-E2E)**. The relay is a **blind broker**: a `/signal/*` rendezvous for the
SDP offer/answer (+ optional blind TURN); it never sees owner plaintext. The Pi
**signs its DTLS fingerprint** with its ES256 identity key; the browser pins
`/api/identity` (TOFU) and **verifies** before trusting the channel. Owner auth
runs **over the channel** (WebAuthn login → the `Bridge` captures the `ftw_owner`
session cookie server-side and stamps it on later frames). Every P2P frame is
**marked REMOTE** so the gate is fail-closed (no `lan-bypass` without a session).
The owner cleartext tunnel is REMOVED; a restricted `homeStaticForward` serves
only static GETs (refuses `/api/*` except `/api/identity`, strips cookies). The
**friend pair-flow** (`/h/*`, `/tunnel/*`, `tunnel.Queue`) is a SEPARATE,
**intentionally wide-open** path. Everything is **opt-in, default-off**.

Key files: `go/cmd/ftw-relay/{handlers.go,signal.go,pollsecrets.go,owners.go}`,
`go/cmd/forty-two-watts/{owner_signal_loop.go,owner_relay_register.go,main.go}`,
`go/internal/p2p/{bridge.go,manager.go}`, `go/internal/api/{api_owner_access.go,
api_owner_gate.go,api_p2p.go,api_identity.go}`, `web/p2p.js`, `web/owner-access/*`.
Design spec: `docs/superpowers/specs/2026-06-04-p2p-only-home-route-v1-design.md`.

## 3. KNOWN — do NOT open issues for these (verify the FIX held instead)

These were found by review and HARDENED. Confirm each fix actually holds; only
file an issue if you can show it is NOT fixed.

1. `/tunnel/register` (friend) recovering the **owner poll secret** by host_id →
   namespaced. Verify a friend register can't obtain an `owner-*` host's secret.
2. `p2pFetch` **cleartext relay fallback** for owner `/api/*` → strict mode.
   Verify a channel timeout during login/enroll does NOT send the WebAuthn body to
   the relay.
3. `/signal` **waiter leak** / capacity exhaustion → abandon-waiter + caps.
4. **Offer↔answer not bound** → nonce binding. **Unauth Pi session slots** → reaping.
5. `Set-Cookie: ftw_owner` **readable by JS** in DataChannel responses → stripped.

And these are **by design** (NOT bugs — do not report):
- Friend pair-flow gives a granted friend full Pi shell/API. Intentional.
- **Per-page re-auth**: a fresh page opens a fresh channel with no session until it
  re-logs-in. UX gap, not a security hole (fails to "log in again", never "open
  without a session").
- **Operator-at-login**: whoever serves the SPA JS is trusted at login (SRI/SW
  deferred). The ~80% trustless ceiling for a browser client.
- **Strict-P2P-no-TURN** can't connect behind CGNAT-without-IPv6 (TURN is the
  blind opt-in fallback).
- **TOFU first connect** trusts the path once (SSH known-hosts model).

## 4. Test plan — hunt these (by area, with concrete attack ideas)

### A. Signaling rendezvous `/signal/*`
- Malformed/oversized offers & answers; missing/garbage nonce; nonce reuse,
  collision, very long nonce, nonce with `/`, `..`, unicode, empty.
- **Cross-delivery**: can browser B (or an attacker) receive browser A's answer?
  Can an attacker displace/starve the legit offer despite nonce-keying? Hit the
  per-site nonce cap and check the legit owner still connects.
- **Waiter exhaustion**: hammer `GET /signal/<random-site>/answer?n=<random>` for
  thousands of sites/nonces; confirm waiters are reclaimed (no permanent growth,
  no "at capacity" lockout of a legit site). Watch relay RSS over the run.
- Poll the Pi-side `/signal/{host}/offer|answer` WITHOUT the poll secret, with a
  wrong one, with a friend's secret — must be rejected.
- Race conditions: concurrent offers/answers, overwrite timing, TTL boundaries.

### B. Fail-closed gate + channel session
- Open a P2P channel (offer/answer) but DON'T log in; hit every owner endpoint
  (`/api/owner-access/devices`, `/api/status`, `/api/config`, `/api/mode`,
  `/api/p2p/offer`, pair endpoints) — all must be 401, NEVER lan-bypass.
- Try to forge the session: put `Cookie: ftw_owner=...` or `X-FTW-Tunnel: ...` in
  a request frame; try a guessed/stolen token; try to make the Bridge capture a
  `Set-Cookie` from a NON-login endpoint (any endpoint that sets ftw_owner?).
- **Cross-channel leak**: does channel 1's captured session ever authorize a frame
  on channel 2? Open two channels, log in on one, hit owner endpoints on the other.
- Logout-over-channel: does it actually de-authorize subsequent frames?
- Can a frame reach the UNGATED mux somehow (bypassing the gate)?

### C. Poll-secret / registration (`/me/register`, `/tunnel/register`, pollsecrets.go, owners.go)
- Re-verify §3.1. Try every way to get an `owner-*` poll secret without the ES256
  key: friend register with an owner host_id; register a fresh site with your key
  but a victim host_id (the old F2); host_id collision; case/whitespace variants.
- `/me/register` signature: replay (skew window), wrong key, key-rotation (does a
  changed site key get rejected / re-pinned?), unsigned, truncated sig.
- Memory caps on the owner registry + poll secrets + tokens under a flood.

### D. Signed fingerprint / MITM (manager.go SignFingerprint, web/p2p.js verify)
- As a malicious relay: swap the answer SDP's `a=fingerprint` (browser must abort);
  strip `fp_sig` (must abort — no downgrade to unsigned); replay an old signed
  answer (skew window); swap `site_id`/`ts`; serve a DIFFERENT Pi's validly-signed
  answer (browser pinned site A's key — must reject site B).
- The browser's SDP fingerprint parse + `normalizeFp` + WebCrypto SPKI import:
  malformed SDP, multiple m-lines, session-vs-media fingerprint, weird casing/line
  endings. Confirm Safari-shaped SPKI import path works.
- Cross-protocol signature reuse: can a `ftw-dtls-fp:v1:` signature be replayed as
  a `ftw-me-register:v1:` or a Nova JWT/claim, or vice-versa? Audit every
  `SignRawHex` / `Signer()` consumer for prefix collision.

### E. `homeStaticForward` (relay blindness)
- Try to reach an owner `/api/*` through the relay home host via path tricks:
  `/api/%2e%2e/`, `/API/`, `//api/`, trailing dot, `;`, encoded slashes, the
  `{rest...}` matcher, `/api/identity/../config`, query smuggling. Must stay 403.
- Confirm inbound `Cookie` and outbound `Set-Cookie` are stripped in BOTH
  directions on the static path. Confirm no owner body is ever forwarded.
- Can `/api/identity` leak anything beyond pubkey/site_id? Can it be widened?

### F. WebAuthn / enroll / PIN — the "accidental access" class
- Over the public home host (P2P, marked remote): try to **enroll your own
  passkey** without the LAN PIN. Must require the PIN; the PIN endpoint must refuse
  tunneled/P2P frames (genuine-LAN only). Try to mint/read the PIN remotely.
- First-device TOFU on a shared LAN; lost-device/recovery; RP-ID/origin checks
  (`clientDataJSON.origin`, rpIdHash); the `BackupEligible/BackupState` round-trip.
- Can a friend grant + the (intentional) shell be turned into a PERMANENT owner
  passkey or lock the owner out? (That escalation MUST stay closed.)
- Device list/delete + sign-out: session revocation, can a friend/LAN actor delete
  the owner's credential?

### G. Friend pair-flow isolation
- Confirm the friend tunnel is fully SEPARATE from the owner P2P path now — no
  shared state that lets a friend reach owner-gated surfaces. (Friend shell is
  intentional; escalation to *permanent owner* / *lockout* is not.)
- `/h/{token}/web/` asset loading end-to-end (Codex flagged this needs a browser
  check) — does the friend web view still load with absolute asset paths?
- `ftw-pair` denylist / path-normalization bypass (encoded `/api/pair/...`).

### H. DoS / resource exhaustion
- Relay: flood `/signal/*`, `/tunnel/register`, `/me/register`, the home host;
  watch memory (sites map, waiters, poll secrets, owner registry, tunnel queue).
- Pi: flood unauthenticated offers → fill `Manager` session slots (max 16) → is the
  legit owner denied? Confirm unauth/half-open reaping works. Slow-loris a channel.
- Body-size caps everywhere (offer 64KiB, control endpoints, ceremony bodies).

### I. Browser / JS (`web/p2p.js`, `web/owner-access/*`)
- `p2pFetch` strict mode: prove owner `/api/*` never silently uses the relay on a
  dead channel. Race the channel up/down mid-request.
- localStorage pin poisoning / eviction (Safari ITP): clear `ftw.identity:*`; does
  it re-pin safely (TOFU) or fail open? Cross-site/cross-origin pin confusion.
- Header/Set-Cookie exposure to JS; request/response frame injection; req_id
  correlation abuse (collide req_ids across concurrent calls).

### J. Broader system (lighter — only if time, and clearly separate from above)
- Control loop / dispatch / clamping safety (`go/internal/control`, `docs/clamping.md`),
  the stale-meter guard, the watchdog. Lua driver host capability boundaries.
- Config reload, the API method-mux, the TS DB / parquet rolloff. Don't go deep —
  the home route is the priority.

### K. Codex static passes (run a few, targeted)
- Whole-`go/cmd/ftw-relay` + `go/internal/p2p` + `go/internal/api` owner-access:
  "find auth bypasses, fail-open paths, missing caps, TOCTOU, unbounded growth".
- A pass specifically diffing `p2p-only-home-route` vs `master` for the home-route
  files, asking "what did this refactor break or leave exploitable?".
- Cross-check every Codex finding against the harness for a live repro.

## 5. Methodology (per candidate)
1. Hypothesis: which invariant breaks, what's the impact, who's the attacker.
2. **Build a concrete repro** against the local harness (curl script / Playwright /
   Go test). No repro → "second look" list, not an issue.
3. Confirm it's real AND not in §3 (known/by-design).
4. Severity (critical/high/medium/low) + the invariant it breaks.
5. File the issue.

## 6. Issue format (one bug per issue, deduped)
- **Title**: `[home-route] <concise>` (or `[relay]`/`[p2p]`/`[owner-access]`).
- **Labels**: `bug`, `security` (if applicable), severity label.
- **Body**: Summary · Invariant broken · **Repro** (exact commands/script/test) ·
  Impact · Suggested fix · Environment (branch SHA, harness).
- Create with `gh issue create --repo frahlg/forty-two-watts ...`. If `gh` is
  unauthed or creation fails, append the issue to `bug-hunt-report.md` instead.

## 7. Guardrails (hard limits)
- **NEVER** deploy to, connect to, or modify any real relay/Pi/Cloudflare/production.
  ONLY the local docker harness. No real-network attacks (STUN to a public server
  is fine for the harness; nothing else outbound).
- **NEVER** push to master, merge, or alter the branch's product code. You may write
  throwaway test scripts / local-only branches; don't push them.
- No data exfiltration. Dual-use/destructive test ideas: simulate against the
  LOCAL harness only.
- **Cap ~15 issues.** Dedupe hard: if it's a class, file ONE issue with examples.
- Don't re-file §3 items. Don't file pure UX/style nits.

## 8. Deliverables by morning
- `bug-hunt-report.md` at the repo root: ranked confirmed findings (with repro
  pointers), what you verified HELD, and a "couldn't reproduce / needs human" list.
- One GitHub issue per confirmed bug.
- Leave the harness torn down (`make e2e-docker-down`) and the working tree clean.
