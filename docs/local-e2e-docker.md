# Local end-to-end harness (relay + Pi in Docker)

Run the **whole home-route stack on one machine** — the standalone `ftw-relay`
plus a `forty-two-watts` "Pi" wired to dial it — with no real Pi, no relay VM, no
Cloudflare, and no certificates. This is the fast feedback loop for anything that
touches owner-access, the relay tunnel, the pair flow, or P2P.

## Quick start

```bash
make e2e-docker-up      # build images + start relay + Pi
make e2e-docker-logs    # follow both services
make e2e-docker-down    # stop + wipe the state volume
```

Then open:

| View | URL | What it exercises |
|---|---|---|
| Pi dashboard (LAN/loopback) | <http://localhost:8080/> | the local owner view — owner-access **LAN-bypass** path |
| Home route (remote via relay) | <http://home.fortytwowatts.localhost/> | the **remote** view — relay tunnel + the `X-FTW-Tunnel` remote/LAN gate |

`*.localhost` resolves to `127.0.0.1` in every modern browser and counts as a
**secure context**, so WebAuthn/passkeys work without a cert. The relay listens on
host port **80** so the browser's `Host` header carries no port and matches the
relay's `home.fortytwowatts.localhost/` mux pattern exactly like prod's `:443`.

## What's real here

The Pi dials `FTW_RELAY_URL=http://relay:7378`, signs its **ES256 `/me/register`**,
the relay **TOFU-pins** the key (`-home-allow-tofu`), and the home route tunnels
the browser to the Pi. The owner-access gate, the remote/LAN discriminator, the
pair flow, and the P2P **signaling** all run for real against the merged master
code — so this reproduces the `#424`/`#429` security behavior and is the bench for
the upcoming P2P-only work.

> **TOFU is testing-only.** `-home-allow-tofu` trusts the Pi's key on first
> registration. Production pins it with `-home-pubkey`. Never run the public home
> route in TOFU mode.

## P2P (WebRTC) caveat — and tier 2

From your **Mac browser → app container**, the direct DTLS DataChannel often does
not form: Docker Desktop runs containers in a Linux VM, so the container's ICE
**host candidates** (its `172.x` address) are unreachable from macOS, and there's
no STUN/TURN in this harness. Traffic then transparently falls back to the relay
tunnel — perfectly fine for testing auth / gate / registration / the dashboard.

To test the **real P2P DataChannel** (the signed-fingerprint handshake, the
fail-closed gate over P2P, the browser verify code), use **tier 2**: a
headless-Chrome (Playwright) container on this same bridge network. Container ↔
container P2P connects **directly** — on the docker net there is no NAT, so direct
ICE always succeeds, which is exactly what you want for a deterministic P2P test
(no CGNAT/TURN variables). Playwright also exposes a **virtual WebAuthn
authenticator** (CDP `WebAuthn.addVirtualAuthenticator`) so the passkey ceremony
runs unattended.

## Tier 2 — automated container-side P2P + passkey proof

```bash
make e2e-docker-tier2   # build relay + Pi + browser, run the test, tear down
```

One command brings up the tier-1 stack **plus** a headless-Chromium
(Playwright) container on the same bridge net, runs a single test, and exits
non-zero if it fails. The test:

1. installs a **CDP virtual WebAuthn authenticator** (`WebAuthn.enable` +
   `WebAuthn.addVirtualAuthenticator`, `ctap2`/`internal`/resident-key/UV) so
   the passkey enroll + login run with no human prompt;
2. **enrolls** a passkey over the relay tunnel (first enrollment needs the LAN
   PIN — the test mints it straight from the Pi's bridge port
   `forty-two-watts:8080`, a genuine private-range source, exactly the
   local-presence proof the PIN exists for), then **logs in** with it;
3. asserts the real P2P `RTCDataChannel` reaches **`direct`** (polls
   `window.ftwP2P.state()`), proving container-to-container WebRTC forms with no
   relay fallback; and
4. makes one **authenticated owner API call** (`window.p2pFetch('/api/status')`)
   over that DataChannel and asserts `200`.

### How the wiring lines up

- **RP-ID / origin.** The Pi defaults its WebAuthn RP-ID to the production host
  (`home.fortytwowatts.com`). The tier-2 override
  (`docker-compose.e2e-tier2.yml`) sets
  `FTW_OWNER_ACCESS_RPID=home.fortytwowatts.localhost` and
  `FTW_OWNER_ACCESS_ORIGINS=http://home.fortytwowatts.localhost[,:7378]` on the
  Pi, so `clientDataJSON.origin` (the harness home host over plain HTTP) passes
  the RP-ID check. `*.localhost` is a secure context, so WebAuthn + WebCrypto
  work without TLS; WebAuthn ignores the port for both RP-ID and origin.
- **Home host → relay.** The browser must reach the home route *as* the home
  host (or WebAuthn refuses). A docker network **alias**
  (`home.fortytwowatts.localhost` on the relay) lets the bridge DNS resolve it
  for the Node test process, and Chromium's `--host-resolver-rules=MAP
  home.fortytwowatts.localhost relay` does the same for the page — so the
  request bytes go to the relay while the page origin stays the home host.
- **STUN.** The harness has no WAN, so resolving public STUN would just burn the
  ICE-gather budget. `FTW_P2P_STUN=none` (a knob that defaults to the production
  STUN set when unset) makes the Pi gather **host candidates only** — correct
  and fast on a shared-L2 bridge, where host pairs route directly.

### Files

- `docker-compose.e2e-tier2.yml` — the `playwright` service (profile `tier2`) +
  the Pi RP-ID/STUN patch + the relay home-host alias.
- `deploy/local-e2e/tier2/` — the Playwright project: `package.json`,
  `playwright.config.ts`, and `tests/home-route-p2p.spec.ts`.

## Files

- `docker-compose.e2e.yml` — the two services (bridge net, published ports).
- `Dockerfile.relay` — standalone `ftw-relay` build (not in the main image).
- `deploy/local-e2e/config.yaml` — minimal Pi config; `site.name: "Home"` →
  `site:Home`, matching the relay's `-home-site`.
