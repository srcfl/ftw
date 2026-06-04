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
runs unattended. Tier 2 is not built yet — it's the next step.

## Files

- `docker-compose.e2e.yml` — the two services (bridge net, published ports).
- `Dockerfile.relay` — standalone `ftw-relay` build (not in the main image).
- `deploy/local-e2e/config.yaml` — minimal Pi config; `site.name: "Home"` →
  `site:Home`, matching the relay's `-home-site`.
