# Local development

Two patterns for running FTW on your dev machine:

1. **Host-native `go run`** — fastest feedback loop; full LAN access
   from the binary, which matters when you want to point the UI at a
   remote instance via the `/api/*` proxy (see below).
2. **`docker compose up`** — mirrors production layout (sidecar, volumes,
   health-checks). Good for testing the update / restart flow.

The docker-compose override (`docker-compose.override.yml`) is intentionally
not tracked — it is a per-machine development knob. Create it when you need a
local image build. The committed `docker-compose.yml` is the reference
production topology.

## Host-native `go run`

For the complete simulator loop, use the fresh-clone-safe target from the repo
root. On first run it creates the ignored `config.local.yaml` from
`config.local.example.yaml`:

```bash
make dev
```

To run only the app against that local configuration:

```bash
cd go
go run ./cmd/ftw \
  -config ../config.local.yaml \
  -web ../web
```

Opens on `:8080` by default (override with `api.port` in `config.yaml`).
Fast Go builds mean edit → `Ctrl-C` → re-run → ready in ~2 s.

## API proxy to a live instance

When iterating on the web UI, you want real PV / battery / driver
data without (a) running drivers locally or (b) mutating the live
instance's config by accident. Set `FTW_PROXY_UPSTREAM` and the
local server forwards every `/api/` request to that upstream while
continuing to serve static assets from your working tree.

```bash
# .env.local — gitignored per-machine
FTW_PROXY_UPSTREAM=http://192.168.1.139:8080
FTW_SELFUPDATE_ENABLED=
```

```bash
cd go
set -a; source ../.env.local; set +a
go run ./cmd/ftw -config ../config.local.yaml -web ../web
```

On boot you'll see:

```
WARN proxy enabled — /api/* forwards upstream  upstream=http://192.168.1.139:8080  read_only=true
```

### What's forwarded vs. local

| Path | Handler |
|---|---|
| `/api/*` | Forwarded to `FTW_PROXY_UPSTREAM` |
| `/`, `/index.html`, `/legacy`, `/setup` | Local `web/` files |
| `/style.css`, `/components/*`, `/app.js`, … | Local `web/` files |

Editing `web/index.html` or `web/components/ftw-modal.js` shows up on
the next browser refresh. `/api/status` still shows live SoC from the
Pi.

### Read-only gate

Mutating methods (`POST`, `PUT`, `DELETE`, `PATCH`) under `/api/` are
blocked with a 403 by default so a stray Save / Set / Reset click in
the dev UI can't touch the live instance:

```
{ "error": "proxy read-only: POST /api/mode blocked (set FTW_PROXY_READONLY=0 to allow)" }
```

Need to exercise write paths for a specific debugging session? Set
`FTW_PROXY_READONLY=0` and you're on the honour system.

### Caveats

- **Upstream unreachable**: the proxy returns a 502 JSON body
  (`{"error":"proxy: upstream unreachable (<host>)"}`). The local
  server keeps serving static assets — only the `/api/*` fetches
  fail. Likely causes: Pi offline, network partition, wrong URL.
- **Docker Desktop on WSL2**: containers often can't reach LAN IPs
  from the default bridge network — `FTW_PROXY_UPSTREAM` appears
  unreachable from inside the container even though the host can
  reach it. Workarounds: run host-native (`go run`, recommended),
  or switch the service to `network_mode: host`, or use Linux /
  Docker Engine directly.
- **No streaming**: the proxy forwards request and response whole;
  Server-Sent Events / WebSockets wouldn't work through it today.
  The project doesn't use either yet (`clients poll`).

## Side-by-side UI: `/` vs `/legacy`

The web-component rewrite is now the default at `/`. The original
layout is still served at `/legacy` for comparison / regression
checks while the old file set is wound down:

| URL | HTML | JS | Purpose |
|---|---|---|---|
| `/` | `index.html` | `next-app.js` | Web-components dashboard (default) |
| `/legacy` | `legacy.html` | `app.js` | Pre-redesign layout — do not touch |

`/next` stays registered as a 301 to `/` so older bookmarks still
land on the right page. Both pages share the same proxy, the same
`style.css`, and the theme tokens in `/components/theme.css`.

## docker compose dev

For full-stack checks (sidecar, update flow, volumes):

```bash
docker compose up -d --build
docker compose logs -f ftw
```

The override template builds both images from your tree (`pull_policy:
never`), exposes the UI on `localhost:8080`, and passes
`FTW_UPDATER_SKIP_PULL=1` to the sidecar so Update / Restart goes
straight to `compose up -d` without trying to pull your locally-built
image over itself.

If you want the proxy with compose, add to your override:

```yaml
services:
  ftw:
    build: .
    image: ftw:dev
    pull_policy: never
    env_file:
      - .env.local
    network_mode: host   # needed for LAN reachability on WSL2
  ftw-updater:
    build:
      context: .
      dockerfile: Dockerfile.updater
    image: ftw-updater:dev
    pull_policy: never
    environment:
      FTW_UPDATER_SKIP_PULL: "1"
```

…and set `FTW_PROXY_UPSTREAM` in `.env.local` as above.
