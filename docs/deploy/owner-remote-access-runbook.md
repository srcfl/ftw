# Owner Remote Access — Deploy Runbook (Phases 1–3)

Deploy the passkey-gated owner remote-access path end-to-end and prove
the **"Face ID from my phone → my own Pi dashboard"** flow on real
hardware:

- **Pi** on the home LAN at `192.168.192.40` (running `forty-two-watts`).
- **Cloud relay** `relay.fortytwowatts.com` (running `ftw-relay`).
- **Cloudflare** zone `fortytwowatts.com` (you own it).

This runbook is the operator-facing companion to:

- `docs/relay-deploy.md` — the canonical `ftw-relay` VM setup (TLS, systemd).
- `docs/superpowers/specs/2026-06-03-home-route-passkey-design.md` — the
  design (RP-ID rules, security must-fixes §8, phasing §14).

> **Scope note.** Phases 1–3 are **built and committed** — PR #414 (the
> safe-floor auth-gate + the forge-proof per-process tunnel marker, the
> always-on ES256 identity + wallet handle, and usernameless Conditional-UI
> login), plus the **LAN-PIN first-enrollment** mechanism (§4, committed on
> top). Everything in this runbook is **deploy/ops** — no code left to
> write. (Phases 4–5 are design docs only, not deployed here.) Build the Pi
> binary from branch `home-route-phase45`.

> **RP-ID is a one-way door.** Every passkey is bound to the RP-ID it was
> created under. This runbook deploys under **`relay.fortytwowatts.com`**
> (the value `forty-two-watts` already defaults to). If you ever move to
> the dedicated `home.fortytwowatts.com` host (spec D6), **every passkey
> enrolled here is invalidated** and owners must re-enroll. Do not enroll
> real owner passkeys you care about until the final RP-ID is pinned.
> Spec §3 R3.

---

## 0. Placeholders

Fill these in before you start; they appear verbatim below.

| Placeholder | Meaning | Example |
|---|---|---|
| `<RELAY_PUBLIC_IP>` | Public IPv4 of the relay VM | **`16.170.137.95`** (yours) |
| `<RELAY_PUBLIC_IP6>` | Public IPv6 of the relay VM (optional) | (none configured) |
| `<YourSiteName>` | `config.yaml` → `site.name`, **verbatim including spaces** | `Home` |
| `<PI_USER>` | SSH user on the Pi | `pi` |

`<YourSiteName>` becomes the relay key `site:<YourSiteName>` — the Pi
registers `site_id = "site:" + site.name` (see
`go/cmd/forty-two-watts/main.go:1491`). If `site.name` is `Home`, every
URL below uses `site:Home`. **Spaces are not URL-encoded by the Pi** — if
your site name has a space, URL-encode it in the browser address bar
(`site:My%20Home`) but keep it literal in `config.yaml`.

---

## 1. Cloudflare — DNS for `relay.fortytwowatts.com`

You own `fortytwowatts.com` in Cloudflare. **Already done** (you added these):

| Type | Name | Content | Proxy status |
|---|---|---|---|
| `A` | `relay` | `16.170.137.95` | DNS-only (grey) ✓ |
| `A` | `home` | `16.170.137.95` | DNS-only (grey) ✓ — for Phase 4 |
| `A` | `subetha` | `16.170.137.95` | DNS-only (grey) ✓ — spare |

That yields `relay.fortytwowatts.com → 16.170.137.95`. For Phases 1–3 only
`relay.*` is used; `home.*` is pre-positioned for the Phase 4 cutover.
DNS-only (grey cloud) is the right choice — see §1.1 (it means you need a
publicly-trusted cert on the VM; §2.1).

### 1.1 Proxy on or off — the load-bearing decision

`ftw-relay` terminates TLS itself (`-cert`/`-key`, Cloudflare **Origin
Cert**). The owner path also carries a **long-poll tunnel** (the Pi's
`/tunnel/<host_id>/next` held open up to 25 s) and an `HttpOnly; Secure;
SameSite` session cookie (`ftw_owner`). Both work through Cloudflare's
proxy, but there is a real tradeoff:

**Option A — DNS-only (grey cloud) — RECOMMENDED for first bring-up.**
- Click the orange cloud to grey it out for both the `A` and `AAAA`
  record. Cloudflare does pure DNS; clients connect straight to the relay
  VM on `:443`.
- Pro: simplest trust path (browser → relay → Pi, **two** hops). No proxy
  buffering of the 25 s long-poll, no Cloudflare 100 s `proxy_read`
  ceiling to worry about, no extra TLS hop. The relay's Origin Cert is
  served directly; browsers trust it because it is a **publicly-trusted
  cert** only if you used a real cert. A Cloudflare *Origin* Cert is
  **only trusted by Cloudflare**, so with DNS-only you must instead use a
  publicly-trusted cert (Let's Encrypt / ZeroSSL) on the VM. See §2.1.
- Con: the relay VM's public IP is exposed (no Cloudflare DDoS shield);
  you manage cert renewal yourself.

**Option B — Proxied (orange cloud) + SSL/TLS mode "Full (strict)".**
- This is what `docs/relay-deploy.md` describes. Cloudflare terminates
  the browser's TLS at the edge with a CF-managed edge cert, then
  re-encrypts to the relay VM using the **Cloudflare Origin Cert**
  (15-year, CF-trust-only — perfect here because only CF talks to the
  origin).
- Pro: DDoS/WAF shield, hides the VM IP, free auto-renewed edge cert.
- Con (the gotchas you must accept):
  - **Long-poll / WebSocket:** Cloudflare's proxy holds idle connections
    for ~100 s before a `524`. The relay's long-poll deadline is **25 s**
    (`ftw-relay -poll-timeout`, default `25s`), comfortably under that, so
    the tunnel survives the proxy. Do **not** raise `-poll-timeout` above
    ~90 s while proxied. Phase 5 WebRTC/WebTransport (spec §10) will not
    traverse the orange-cloud HTTP proxy — that traffic goes P2P/TURN, not
    through this record, so it is unaffected.
  - **Owner cookie:** `ftw_owner` is `Secure; HttpOnly; SameSite=Lax`
    (`go/internal/api/api_owner_access.go` `issueOwnerSession`). It rides
    the tunnel as a normal header and is opaque to Cloudflare; proxying
    does not break it. Just keep "Full (strict)" so the edge↔origin leg
    stays HTTPS (a downgrade to "Flexible" would make the cookie's
    `Secure` flag get dropped on the origin leg — never use Flexible).
  - **`Full` vs `Full (strict)`:** "Full" accepts any origin cert
    (including self-signed); "Full (strict)" validates the origin cert
    against the CF Origin CA. Use **Full (strict)** with the CF Origin
    Cert. Never "Flexible" — it terminates as plain HTTP to the origin
    and silently breaks `Secure` cookies and the security model.

**Recommendation:** start with **Option A (DNS-only)** to remove
Cloudflare from the variable set while you debug the passkey flow, using a
Let's Encrypt cert on the VM (§2.1). Once green end-to-end, optionally
flip to **Option B (proxied, Full strict)** for the DDoS shield and swap
the cert to the CF Origin Cert. Both are valid; the rest of this runbook
works under either.

### 1.2 Edge TLS hardening (Option B only)

Per `docs/relay-deploy.md` §"One-time DNS + CF setup":
`SSL/TLS → Overview` = **Full (strict)**; Always Use HTTPS **On**;
Minimum TLS **1.2**. HSTS/preload on the apex is optional and
**permanent** — skip it for a test deploy.

---

## 2. Relay server — deploy `ftw-relay`

Follow `docs/relay-deploy.md` for the full VM hardening. Summary of what
this runbook needs:

### 2.1 TLS material

- **Option B (proxied):** use the **Cloudflare Origin Cert** for
  `relay.fortytwowatts.com` (CF dashboard → SSL/TLS → Origin Server →
  Create Certificate). Install per `docs/relay-deploy.md` "Cert + key" to
  `/etc/ssl/relay/cert.pem` + `/etc/ssl/relay/key.pem` (key `0600`,
  root-owned).
- **Option A (DNS-only):** the CF Origin Cert is **not** browser-trusted,
  so issue a publicly-trusted cert instead:
  ```bash
  # On the relay VM, port 80 reachable, DNS A record already live:
  sudo apt-get install -y certbot
  sudo certbot certonly --standalone -d relay.fortytwowatts.com
  # → /etc/letsencrypt/live/relay.fortytwowatts.com/{fullchain,privkey}.pem
  ```
  Point the systemd unit at those paths instead of `/etc/ssl/relay/*`.
  Add a `certbot renew` timer (the Origin Cert path is renewal-free; LE is
  90-day).

### 2.2 Binary

```bash
# Build matrix from CI publishes ftw-relay-linux-amd64 to GH releases:
sudo curl -fsSL -o /usr/local/bin/ftw-relay \
  https://github.com/frahlg/forty-two-watts/releases/latest/download/ftw-relay-linux-amd64
sudo chmod 0755 /usr/local/bin/ftw-relay
```

Or build locally and `scp` it: `make build-arm64`/`build-amd64` emit
`bin/ftw-relay-linux-amd64` (Makefile `build-amd64` target). The relay is
pure-Go, CGO-free, single static binary.

### 2.3 Flags / systemd

The relay needs no env vars — only flags. From `go/cmd/ftw-relay/main.go`:

| Flag | Value | Notes |
|---|---|---|
| `-addr` | `:443` | bind public TLS port |
| `-cert` | `/etc/ssl/relay/cert.pem` (or LE fullchain) | enables HTTPS mode |
| `-key` | `/etc/ssl/relay/key.pem` (or LE privkey) | enables HTTPS mode |
| `-poll-timeout` | `25s` (default) | leave as-is; see §1.1 long-poll note |

Use the systemd unit verbatim from `docs/relay-deploy.md`
("Systemd unit"): `DynamicUser=yes`,
`AmbientCapabilities=CAP_NET_BIND_SERVICE`, `ReadOnlyPaths=/etc/ssl/relay`.
Then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ftw-relay
sudo systemctl status ftw-relay
```

### 2.4 What the relay serves for `relay.fortytwowatts.com`

The mux (`go/cmd/ftw-relay/handlers.go` `Handler()`) exposes the
owner-route family this runbook uses:

- `GET /healthz` — liveness.
- `POST /me/register` — the Pi posts `{site_id, host_id}` here every 60 s
  (in-memory `OwnerRegistry`; a relay restart drops it, the Pi
  re-registers within 60 s).
- `/me/{site_id}` and `/me/{site_id}/{rest...}` — forward verbatim through
  the tunnel to the registered host's long-poll loop, **preserving the
  query string** (login redirects, ceremony tokens land intact).

The relay does **not** inspect the `ftw_owner` cookie or verify the
passkey — it is a dumb router. All WebAuthn validation and the auth-gate
happen on the Pi (spec D4).

### 2.5 Smoke-test the relay alone

```bash
curl -sS https://relay.fortytwowatts.com/healthz        # → OK
# Before the Pi registers, this site is unknown:
curl -sS -o /dev/null -w '%{http_code}\n' \
  https://relay.fortytwowatts.com/me/site:<YourSiteName> # → 503 (site not registered)
```

A `503 "site not registered (host may be offline)"` here is **correct**
until the Pi registers in §3.

---

## 3. Pi (`192.168.192.40`) — env + deploy the binary

### 3.1 Environment

Set these for the `forty-two-watts` process on the Pi. Defaults come from
`go/cmd/forty-two-watts/main.go` (`OwnerAccessRPID` default is already
`relay.fortytwowatts.com`; `LANBypass` default is already `true`) — but
set them explicitly so the deploy is self-documenting:

```ini
FTW_RELAY_URL=https://relay.fortytwowatts.com
FTW_OWNER_ACCESS_RPID=relay.fortytwowatts.com
FTW_OWNER_ACCESS_ORIGINS=https://relay.fortytwowatts.com
FTW_OWNER_ACCESS_LAN_BYPASS=true
```

What each does:

- **`FTW_RELAY_URL`** — when non-empty, the Pi starts
  `runOwnerRelayRegistration` (`main.go:1490`): posts `site:<YourSiteName>`
  + a stable `host_id` to `POST /me/register` and runs the long-poll loop.
  Unset = no remote access at all.
- **`FTW_OWNER_ACCESS_RPID`** — WebAuthn Relying Party ID. **Must equal
  the host in the URL the browser uses** (`relay.fortytwowatts.com`), or
  every passkey ceremony fails the origin check. This is the one-way-door
  value (spec D6 / §3 R3).
- **`FTW_OWNER_ACCESS_ORIGINS`** — comma-separated allowed WebAuthn
  origins. If unset, the Pi derives `https://<RPID>` automatically, so
  this is belt-and-suspenders. Set it explicitly to
  `https://relay.fortytwowatts.com`.
- **`FTW_OWNER_ACCESS_LAN_BYPASS=true`** — on a **genuine loopback/LAN**
  request the dashboard is open without a passkey (so you are never locked
  out of your own LAN). **This is safe only because the Pi has no inbound
  ports open and the relay tunnel marks its requests as remote** (see §4.2
  and the gotcha in §6). Keep it `true`; never expose the Pi's `:8080`
  directly to the internet.

The Pi's `host_id` is generated once and persisted in `state.db`
(`deriveOwnerHostID`, key `owner_relay_host_id`) so it survives restarts —
important because the relay's `site_id → host_id` map is in-memory.

### 3.2 Deploy the new binary

The Pi must run a build that includes the Phase 1 auth-gate + LAN-PIN work
(§4). Cross-compile and ship the **dev binary** (the
`switching-ftw-deploy-mode` skill automates flipping a host between the
official Docker image and a raw dev binary; do it manually here):

```bash
# On your laptop, in the repo:
make build-arm64
# → bin/forty-two-watts-linux-arm64  (CGO_ENABLED=0, static)

scp bin/forty-two-watts-linux-arm64 \
  <PI_USER>@192.168.192.40:/tmp/forty-two-watts
```

On the Pi (adapt to your install layout / systemd unit name):

```bash
sudo systemctl stop forty-two-watts
sudo install -m0755 /tmp/forty-two-watts /usr/local/bin/forty-two-watts
# Put the four env vars in the unit's EnvironmentFile, e.g.
#   /etc/forty-two-watts/owner-access.env
sudo systemctl start forty-two-watts
journalctl -u forty-two-watts -f
```

Example `EnvironmentFile` drop-in:

```ini
# /etc/forty-two-watts/owner-access.env
FTW_RELAY_URL=https://relay.fortytwowatts.com
FTW_OWNER_ACCESS_RPID=relay.fortytwowatts.com
FTW_OWNER_ACCESS_ORIGINS=https://relay.fortytwowatts.com
FTW_OWNER_ACCESS_LAN_BYPASS=true
```

```ini
# in the [Service] block of forty-two-watts.service:
EnvironmentFile=/etc/forty-two-watts/owner-access.env
```

### 3.3 Confirm registration

In the Pi log you should see (from `runOwnerRelayRegistration`):

```
owner-access: registered with relay  site_id=site:<YourSiteName> host_id=owner-<yoursite>-xxxxxx
```

And from the relay side, the `503` from §2.5 should now become a tunneled
response:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' \
  https://relay.fortytwowatts.com/me/site:<YourSiteName>
# → 200 (the host's /owner-access/ landing page) — site is registered + reachable
```

---

## 4. First-enrollment — the LAN-PIN mechanism (built — Phase 1)

This is the subtle part. Read the *why* (§4.3) before the *what*.

### 4.1 The deadlock this solves

A passkey usable at `https://relay.fortytwowatts.com` **must be created
with origin `https://relay.fortytwowatts.com`** — WebAuthn refuses to run
on `http://192.168.192.40:8080` (no secure context, and the RP-ID would
be wrong). So the **enrollment ceremony has to come through the relay**.

But the bootstrap rule (`enrollAllowed`,
`go/internal/api/api_owner_access.go:273`) says: *when zero devices are
enrolled, allow enrollment with no auth* (trust-on-first-use). Through the
relay, that "first enrollment" window is **internet-exposed** — the first
stranger to reach `enroll/start` becomes the owner of your Pi (spec §8.3,
R-risk in §12).

You cannot resolve this by "just enroll on LAN first": a LAN ceremony runs
at the wrong origin/RP-ID and produces a passkey that **will not work** at
`relay.fortytwowatts.com`.

**Resolution:** prove *local presence* with a short-lived **PIN that only
someone on the LAN can read**, then let that PIN authorize the **first**
enrollment ceremony **even though it arrives through the relay** (so the
ceremony origin is correctly `relay.fortytwowatts.com`).

### 4.2 How it works (built — exact behaviour)

Three pieces on the Pi side (`go/internal/api`), all committed. The
forge-proof tunnel marker (`X-FTW-Tunnel`, spec §8.2) is a **per-process
random secret** generated at boot: the Pi's relay long-poll reverse-proxy
(`runOwnerLongPoll`'s `Director` in
`go/cmd/forty-two-watts/owner_relay_register.go`) stamps the secret on
every forwarded request, and `isTunneled` does a **constant-time compare**
against it. A LAN or remote client can't guess the secret, so it can
neither forge a tunnelled marker nor strip one the proxy added — a request
`isTunneled` iff `X-FTW-Tunnel` equals the boot secret.

**(a) PIN generator + endpoint.**

- The Pi holds a single in-memory enrollment PIN: 6 numeric digits, TTL
  **10 minutes**, regenerated on demand when expired/absent.
- Log it with `slog.Info` when generated, so it shows in the Pi console /
  `journalctl`:
  ```
  owner-access: LAN enrollment PIN  pin=123456  expires_in_s=600
  ```
- `GET /api/owner-access/enroll-pin` returns:
  ```json
  {"pin":"123456","expires_in_s":600}
  ```
- **Reachable ONLY from non-tunnelled (genuine LAN/loopback) requests.**
  If `isTunneled(r)` (carries the trusted `X-FTW-Tunnel` marker) →
  respond `403`. A remote attacker through the relay can never read the
  PIN.
- Add this path to `isOwnerAccessOpenPath` so the global auth-gate (spec
  §8.1) lets it through **on the LAN** without a session cookie (it is the
  one endpoint a not-yet-enrolled local user must reach).

**(b) PIN-gated first enrollment.** Change `enrollAllowed` for
`POST /api/owner-access/enroll/start`. **Only the `devices == 0`
(bootstrap) branch changes:**

- `devices == 0` **and NOT tunnelled** (genuine LAN) → allow, as today.
- `devices == 0` **and tunnelled** → require a valid, unexpired PIN passed
  as `?pin=XXXXXX`. **Constant-time compare** (`crypto/subtle`). Match →
  allow; mismatch/expired/absent → `403 "first enrollment requires the
  LAN PIN"`.
- `devices > 0` → **unchanged**: still requires an authenticated session
  cookie (no PIN path; you re-enroll additional passkeys while logged in).

**(c) Front-end (built).** `web/owner-access/enroll.html` has an optional
PIN input (blank for LAN / logged-in enrollment) and appends `?pin=` to the
start call via `apiBase()` (which routes through the relay to the same Pi).
A `403` mentioning the PIN surfaces a hint pointing at `/enroll-pin`.

### 4.3 Why the PIN — RP-ID origin requirement vs. bootstrap hardening

Two constraints collide:

1. **RP-ID origin requirement.** The passkey must be born at origin
   `https://relay.fortytwowatts.com` (so it works there forever). →
   ceremony must traverse the relay → request is **tunnelled / remote**.
2. **Bootstrap hardening.** A no-auth first enrollment exposed to the
   internet = anyone can claim your Pi. → first enrollment must prove
   you are physically present.

The PIN is the bridge: it is **generated locally, readable only on the
LAN** (the endpoint 403s tunnelled requests, and it is printed to the
Pi's console), yet it **authorizes a remote-origin ceremony**. You prove
LAN presence with a code, while the credential is correctly minted at the
central origin. Once one passkey exists, the PIN path is dead
(`devices > 0` ignores it) and all further enrollment requires a logged-in
session.

### 4.4 Operator flow (first passkey)

1. **On the home LAN**, open `http://192.168.192.40:8080/` and fetch the
   PIN:
   ```bash
   curl -sS http://192.168.192.40:8080/api/owner-access/enroll-pin
   # {"pin":"123456","expires_in_s":600}
   ```
   …or read it from the Pi's log:
   ```bash
   journalctl -u forty-two-watts | grep 'LAN enrollment PIN' | tail -1
   ```
2. **From any browser** (phone or laptop, on or off the home WiFi) open:
   ```
   https://relay.fortytwowatts.com/me/site:<YourSiteName>/owner-access/enroll.html
   ```
   Run the passkey enrollment (Face ID / Touch ID / Windows Hello),
   entering the 6-digit PIN when prompted. The browser creates the
   credential at RP-ID `relay.fortytwowatts.com`; the start call carries
   `?pin=123456`; the Pi validates it (tunnelled + valid PIN → allowed),
   `FinishRegistration` persists the credential, and the `ftw_owner`
   session cookie is set.
3. **Done — a passkey now exists.** Immediately enroll a **second**
   passkey while still logged in (spec §9: synced passkeys + a second
   device sharply raise recovery success). With a session cookie present,
   `enroll/start` no longer needs the PIN.

> PIN expired? It is 10 min. Just re-fetch `enroll-pin` on the LAN — it
> regenerates on demand.

---

## 5. End-to-end test

### 5.1 The real test — phone OFF the home WiFi

1. Turn off WiFi on the phone (use cellular) so there is **no LAN path**
   and the request must go through Cloudflare → relay → Pi.
2. Open:
   ```
   https://relay.fortytwowatts.com/me/site:<YourSiteName>/owner-access/login.html
   ```
3. The login page uses **Conditional-UI autofill** — tap the passkey
   chip / username field, the OS offers the credential, authenticate with
   **Face ID / Touch ID**.
4. The assertion tunnels to the Pi, which runs `FinishLogin`
   (`handleOwnerLoginFinish`) — verifies the signature, runs the
   sign-count clone-guard, sets `ftw_owner`, and you land in the live
   dashboard for **your own** Pi.

Success criteria: dashboard renders, live power data updates, and you
never typed a username or password.

### 5.2 curl smoke tests (relay + gate)

```bash
# (a) Pi is registered and reachable through the relay:
curl -sS -X POST https://relay.fortytwowatts.com/me/register \
  -H 'Content-Type: application/json' \
  -d '{"site_id":"site:<YourSiteName>","host_id":"smoke-probe"}' \
  -o /dev/null -w 'register: %{http_code}\n'      # → 204 (relay accepts upsert)
# NOTE: the Pi re-registers its real host_id every 60s; the smoke value
# above is harmless and is overwritten on the next Pi heartbeat.

# (b) Landing page (forwarded to the host's /owner-access/ landing):
curl -sS -o /dev/null -w 'landing: %{http_code}\n' \
  https://relay.fortytwowatts.com/me/site:<YourSiteName>    # → 200

# (c) THE GATE: an UNAUTHENTICATED REMOTE hit on the dashboard / a control
# endpoint must NOT serve data — it must redirect/401 to passkey login
# (spec §8.1). With no ftw_owner cookie and a tunnelled (remote) request:
curl -sS -o /dev/null -w 'gate(/): %{http_code} %{redirect_url}\n' \
  https://relay.fortytwowatts.com/me/site:<YourSiteName>/      # → 302 → login.html (NOT 200 dashboard)
curl -sS -o /dev/null -w 'gate(api): %{http_code}\n' \
  https://relay.fortytwowatts.com/me/site:<YourSiteName>/api/status  # → 401/302, never 200

# (d) PIN endpoint MUST be LAN-only: through the relay it is forbidden.
curl -sS -o /dev/null -w 'pin-remote: %{http_code}\n' \
  https://relay.fortytwowatts.com/me/site:<YourSiteName>/api/owner-access/enroll-pin  # → 403
# …but on the LAN it returns the PIN:
curl -sS http://192.168.192.40:8080/api/owner-access/enroll-pin   # → {"pin":"...","expires_in_s":...}
```

The gate **is** wired (PR #414, `api_owner_gate.go`), so (c) should return
`302`/`401` — this curl is your on-deploy confirmation. If it ever returns
`200` with real dashboard JSON for an unauthenticated remote request,
something is misconfigured (e.g. the deployed binary predates the gate) —
stop and fix before exposing the relay. That is risk R1 (a wide-open
dashboard that controls real power hardware).

---

## 6. Rollback / gotchas

**Rollback (Pi).** Remote access is fully gated behind `FTW_RELAY_URL`:

```bash
# Disable remote access instantly — Pi stops registering, /me/<site> 503s:
sudo systemctl stop forty-two-watts
# remove FTW_RELAY_URL from the EnvironmentFile (or comment it out)
sudo systemctl start forty-two-watts
```

To roll the **binary** back, re-deploy the previous
`forty-two-watts-linux-arm64`, or use the `switching-ftw-deploy-mode`
skill to return the host to the official Docker image. LAN access is
unaffected by any of this (LAN-bypass).

**Rollback (relay).** `sudo systemctl stop ftw-relay` takes the whole
remote path down; LAN dashboards keep working. The `OwnerRegistry` is
in-memory, so a restart self-heals within 60 s (the Pi re-registers).

**Gotchas:**

- **`FTW_OWNER_ACCESS_LAN_BYPASS=true` is safe ONLY behind the relay.**
  The bypass keys off a loopback/LAN request. It is sound because the Pi
  has no inbound ports and the relay tunnel stamps `X-FTW-Tunnel` on
  every forwarded request (the Pi strips client-supplied copies first),
  so a remote request can never masquerade as LAN (spec §8.2, the
  highest-value invariant). **If you ever port-forward the Pi's `:8080`
  to the internet, this bypass becomes a total auth bypass — never do
  that.** Keep the Pi reachable from outside only through the relay
  tunnel.
- **RP-ID must match the URL host exactly.** Passkeys enrolled under
  `relay.fortytwowatts.com` only work at `https://relay.fortytwowatts.com`.
  Logging in via the LAN IP, the apex, or a future `home.*` host will
  fail the WebAuthn origin check. Do not enroll real owner passkeys until
  the final RP-ID is pinned (spec §3 R3 — one-way door).
- **Cloudflare proxy + long-poll.** If you chose Option B (orange cloud),
  keep `ftw-relay -poll-timeout` at the default 25 s (well under CF's
  ~100 s `524` ceiling). Symptoms of a too-long poll: intermittent `524`
  on `/me/...` and a flapping presence indicator.
- **Never use Cloudflare SSL/TLS mode "Flexible".** It serves plain HTTP
  to the origin, which drops the `Secure` flag handling on `ftw_owner`
  and breaks the security model. Use **Full (strict)** (proxied) or
  DNS-only with a real cert.
- **Pre-existing pair e2e failures are unrelated.** The two failing
  `ftw-pair` e2e tests on this branch are the friend-delegation
  (`/h/<token>`) path, which is **separate and untouched** by owner
  remote access (spec §11 "Friend-delegation unification" is out of
  scope). They do not affect the `/me/<site>` owner flow deployed here;
  do not let them block this deploy.
- **Site name with spaces.** `site:My Home` → URL-encode the space in the
  browser (`site:My%20Home`) but keep it literal in `config.yaml`. The
  relay key and the Pi registration both use the raw `site.name`.

---

## Appendix — quick reference

| Thing | Value / path |
|---|---|
| Relay public URL | `https://relay.fortytwowatts.com` |
| Relay binary | `/usr/local/bin/ftw-relay` (`-addr :443 -cert … -key …`) |
| Relay health | `GET /healthz` → `OK` |
| Pi LAN dashboard | `http://192.168.192.40:8080/` |
| Pi `site_id` | `site:<YourSiteName>` (from `config.yaml` `site.name`) |
| Enroll (through relay) | `https://relay.fortytwowatts.com/me/site:<YourSiteName>/owner-access/enroll.html` |
| Login (through relay) | `https://relay.fortytwowatts.com/me/site:<YourSiteName>/owner-access/login.html` |
| LAN PIN (LAN only) | `GET http://192.168.192.40:8080/api/owner-access/enroll-pin` |
| RP-ID | `relay.fortytwowatts.com` (immutable once passkeys exist) |
| Disable remote access | unset `FTW_RELAY_URL`, restart `forty-two-watts` |

Source of truth: `go/cmd/ftw-relay/handlers.go` (relay routes),
`go/cmd/forty-two-watts/owner_relay_register.go` (Pi registration),
`go/cmd/forty-two-watts/main.go` (env wiring, `:1409`–`:1491`),
`go/internal/api/api_owner_access.go` (ceremony + `enrollAllowed`),
`docs/relay-deploy.md` (VM setup),
`docs/superpowers/specs/2026-06-03-home-route-passkey-design.md` (design,
§8 security must-fixes, §14 phasing).
