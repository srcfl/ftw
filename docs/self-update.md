# Self-update

In-app "Update" and "Restart" buttons trigger `docker compose pull` +
recreate of the selected component. Core retains the snapshot flow; optimizer
updates recreate only `ftw-optimizer`, health-check it, and automatically
restore its previous image on failure. The mechanism is split across three
processes so the main container never touches the Docker socket.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                         host (Pi / server)                          │
│                                                                     │
│  ┌─────────────────────────┐     ┌──────────────────────────────┐   │
│  │  FTW        │     │  ftw-updater (sidecar)       │   │
│  │  ───────────────        │     │  ─────────────               │   │
│  │  selfupdate.Checker     │     │  net/http on UDS             │   │
│  │  - resolves stable,     │◀────┤  POST /update {action,target}│   │
│  │    beta, or edge        │ UDS │  GET /status                 │   │
│  │  - verifies immutable   │     │                              │   │
│  │    GHCR targets         │     │  shells out (with            │   │
│  │  - reads release notes  │     │   FTW_IMAGE_TAG=<target>):   │   │
│  │  - serves /api/version  │     │   docker compose pull        │   │
│  │    /* endpoints         │     │   docker compose up -d       │   │
│  │  - reads state.json     │     │     [--force-recreate]       │   │
│  └─────────┬───────────────┘     └──────────┬───────────────────┘   │
│            │                                │                       │
│            │    update-ipc (Docker volume)  │    /var/run/docker.sock │
│            └────────────────┬───────────────┘    (bind mount)       │
│                             │                                       │
│                      state.json + sock                              │
└─────────────────────────────────────────────────────────────────────┘
```

## Release channels

The selected channel is persisted under `update.channel` in `state.db`.
Changing it only changes the version probe; pulling an image remains a
separate operator action protected by the normal pre-update snapshot.

| Channel | Source | Immutable install target | Intended use |
|---|---|---|---|
| `stable` | GitHub's latest non-prerelease Release | `vX.Y.Z` | Production installations |
| `beta` | Newest published `vX.Y.Z-beta.N` or promoted stable Release | `vX.Y.Z-beta.N` / `vX.Y.Z` | Opt-in testing on real sites |
| `edge` | Newest timestamped GHCR edge build | `edge-YYYYMMDDTHHMMSSZ-<sha>` | Maintainer test rigs and rapid hardware validation |

Stable is always the default. A binary stamped with a beta or edge tag
infers its matching channel on first boot; a previously persisted operator
choice takes precedence. The moving `:beta` and `:edge` aliases are useful
for discovery and manual inspection only. The updater never installs them.

## Why immutable tags, not moving aliases

A GitHub Release is published when the Changesets version PR merges;
the GitHub Actions workflow that builds and pushes the image to GHCR
runs *after* that. The `:latest` tag is also retagged after the build,
which means there's a window where:

- the GH Release exists,
- the immutable `vX.Y.Z` tag is in GHCR,
- but `:latest` still aliases the previous image.

Earlier we polled GH Releases and pulled `:latest` — `docker compose
pull` happily fetched the previous digest, `up -d` recreated the same
container, and the sidecar wrote `state=done` against the new tag while
the running version didn't actually move.

The fix is to ignore `:latest`, `:beta`, and `:edge` as pull targets:

- Stable and beta first resolve a published GitHub Release, then verify
  that its exact tag exists in GHCR. Edge lists GHCR tags and chooses the
  newest timestamped immutable edge tag, never the moving alias.
- The dispatch payload to the sidecar carries the resolved version as
  `target`. The sidecar passes `FTW_IMAGE_TAG=vX.Y.Z` to docker, and
  `docker-compose.yml` uses `image: ghcr.io/.../ftw:${FTW_IMAGE_TAG:-latest}`
  — so the pull resolves a *specific*, immutable tag and the recreate
  is guaranteed to move the digest. No race possible.

The sidecar accepts only allowlisted `core` and `optimizer` components and only `vX.Y.Z`, `vX.Y.Z-beta.N`, or
`edge-YYYYMMDDTHHMMSSZ-<sha>`. Falling through to a moving alias is exactly
what this boundary prevents.

### Compatibility with older Compose files

An older Compose file cannot gain a new service merely by replacing the core
image. Core therefore keeps its local Python and Go-DP fallbacks, while the
Linux migration command (or rerunnable macOS installer) performs the one-time,
rollback-safe creation of `docker-compose.override.yml`. The updater then
auto-discovers that persistent override for every selective optimizer update.
It does not write the host project itself because its Compose mount remains
read-only by design.

Some installations created before the Sourceful migration — and some developer
deployments — have a hard-coded main image such as
`forty-two-watts:optimizer-…`. Exporting `FTW_IMAGE_TAG` cannot affect such a
file. For update, restart, and state rollback jobs the sidecar therefore
appends an updater-owned Compose override from the updater container's private
temporary filesystem:

```yaml
services:
  forty-two-watts: # or ftw; detected from the persistent /app/data mount
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}
```

The override is applied after the base file and user overrides, and exists only
for the active updater job. Immutable updates pin the requested tag, restart
uses the normal `latest` default, and state rollback temporarily tags the exact
running image ID so the rollback cannot also change application version. The
host Compose file remains read-only and unchanged; its project name, service
name, networking, volumes, and data bind are preserved. The override is
deliberately not stored in the shared, main-container-writable status volume.
Missing or ambiguous `/app/data` ownership still fails closed.

The GH Releases API is still consulted, but only as a best-effort
lookup for the changelog body shown in the upgrade modal. A 404 there
(release not yet published, or never paired with the image) doesn't
block the upgrade.

`restart` is the dev-convenience action — no target required, no tag env
override, falls through to compose's `:latest` default and
`--force-recreate`s. It exists so the full pull→recreate→reload flow
can be exercised locally without waiting for a real release.

## Why a sidecar

The main container can't restart itself mid-request — killing its own
process during `docker compose up -d` would drop the HTTP response and
leave the UI polling into the void. A separate container that outlives
the main service's recreate cycle handles this cleanly.

Giving the main container access to `/var/run/docker.sock` would also
grant it root-equivalent access to the host. The sidecar localizes that
privilege to one purpose-built binary with no network listener, a small shared
status volume, and a read-only bind of the Compose project.

## State transitions

`state.json` is written to the shared `update-ipc` Docker volume. Every
step rewrites the whole file atomically (`tmp → rename`). Both ends
treat it as authoritative.

```
idle → starting → snapshotting → pulling → restarting → done
          │             │            │          │
          │             │            └──────────┴→ failed
          │             │                               │
          └─────────────┴───────────────────────────────┘

rollback: starting → snapshotting → restoring → done | failed
```

The shared volume outlives the recreate of either container, so the new
main container reads `done` on startup and serves it to the UI that's
still polling in the browser — which then hard-reloads.

## The channel workflows

`.github/workflows/edge.yml` publishes multi-arch main and updater images
after every merge to `master`. Maintainers can also dispatch the workflow
against a selected feature branch to move the public edge channel for an
explicit hardware test. Each run publishes a new immutable timestamped tag.

`.github/workflows/beta.yml` is manual and requires a tag matching
`vX.Y.Z-beta.N`. It anchors that tag to the selected commit, builds both
ARM64 and AMD64 images, and only then creates the GitHub prerelease. This
ordering prevents the checker from offering a beta whose image is not ready.

Stable continues through Changesets and `release.yml`. The stable asset
workflow explicitly ignores beta tags so a prerelease can never retag
`:latest` or emit a stable Discord announcement. The RPi installer image is
published independently under the permanent `rpi-installer` prerelease.

## Bootstrapping an older updater

The updater does not recreate itself during a main-service update. For an
older deployment with a hard-coded, local, or pre-Sourceful main image, do not
refresh only the sidecar: that leaves the stale host Compose reference in place.
Run the one-time [legacy migration](upgrade-from-legacy.md), which validates the
data bind, preserves the Compose identity and moves both services to canonical
images with rollback.

Only an installation whose main service already uses the canonical dynamic
`ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}` form can refresh its sidecar alone:

```bash
cd ~/ftw  # or the existing ~/forty-two-watts directory
docker compose pull ftw-updater
docker compose up -d --no-deps ftw-updater
```

This does not restart the main service or touch `/app/data`. For an intentional
beta or edge bootstrap, pin both `FTW_IMAGE_TAG` and
`FTW_UPDATER_IMAGE_TAG` to the same currently published immutable tag; do not
reuse the historical beta.1 tag.

## HTTP endpoints

| Endpoint | Purpose |
|---|---|
| `GET  /api/version/check?force=1` | Cached GH Releases probe. `?force=1` bypasses the 3 h cache. |
| `POST /api/version/channel` `{channel}` | Persist `stable`, `beta`, or `edge` and clear the cached target. Does not deploy. |
| `POST /api/version/skip` `{version}` | Persist a dismissed version. Hides the badge until something newer ships. |
| `POST /api/version/unskip` | Clear the skip so the current latest resurfaces. |
| `POST /api/version/update` | Trigger `pull` + `up -d` for the currently-latest tag. |
| `POST /api/version/restart` | Trigger `pull` + `up -d --force-recreate`. Exists so the full flow can be exercised locally without waiting for a real release. |
| `GET  /api/version/update/status` | Pass-through of the sidecar's `state.json`. Polled every 2 s by the UI during the countdown. |
| `GET  /api/version/snapshots` | List retained pre-update and pre-rollback snapshots. |
| `DELETE /api/version/snapshots/{id}` | Delete one retained snapshot. |
| `POST /api/version/rollback` `{snapshot_id}` | Capture a safety snapshot, restore the selected state/config snapshot, and restart the service. |

## Testing locally

```bash
# Bring the stack up with the sidecar
docker compose up -d

# Verify both services are running
docker compose ps

# Open the UI, click the version text in the top-right header
# → modal opens → click "Restart"
# → the overlay counts down while the sidecar runs pull + up -d --force-recreate
# → the new main container writes state=done
# → UI hard-reloads into the (same) version
```

If the sidecar isn't running, the Update/Restart buttons return 502 and
the badge still works as a notify-only indicator.

## Skip semantics

`update.skipped_version` (in the `state.db` `config` KV) holds at most
one version string. `Checker.Info` reports `skipped=true` only when the
persisted value equals the currently-latest release. That means a newer
release automatically re-surfaces the banner without asking the user to
un-skip — we never silently hide something the user didn't explicitly
dismiss.

"Check for updates" in the UI (shown when you open the modal while
already on the latest version) also clears the skip before probing, so
a version you hid earlier resurfaces as soon as you ask about it.

## Hardening options (not v1)

- **Restrict socket access**: put `tecnativa/docker-socket-proxy` in
  front of the socket mount and whitelist only `POST /images/create`
  and `POST /containers/*/restart`.
- **Image signature verification**: call `cosign verify` inside the
  sidecar before `up -d`, rejecting images that don't match the
  release-signing key.

State/config snapshot rollback and automatic previous-image rollback are
already implemented; signature verification remains the primary hardening gap.

## Enabling and disabling

The feature is gated on `FTW_SELFUPDATE_ENABLED=1` for production
builds. The shipped `docker-compose.yml` sets this on the main service;
production deploys that don't use the sidecar (bare-metal binary,
native OS image, etc.) leave it unset and the UI hides the badge
entirely. Handlers under `/api/version/*` return `503 self-update
disabled` when the flag is off.

**Dev exception:** when the binary's compile-time `Version` is `"dev"`
(i.e. no `-ldflags Version=v…` was applied — `make dev`, `go run`, an
unstamped local build), the probe runs implicitly so the operator can
click the "dev" version label in the header and exercise the full flow
without ceremony. The Update button still POSTs to `/api/version/update`
which calls into the sidecar — without a sidecar socket the call 502s
and the modal shows the failure cleanly. That's intentional: dev users
get the visibility, production users get the gate.

Finer-grained knobs, only relevant once the feature is enabled:

- `FTW_UPDATER_SOCKET=""` — keep the GH probe and the "Update available"
  banner but disable the Update/Restart buttons. The UI shows the
  notification dot and release notes; clicking Update surfaces a 502.
- Remove the `ftw-updater` service block from `docker-compose.yml` — the
  main container ignores the missing socket gracefully and behaves the
  same as the previous option.

Future native / OS-image builds will ship their own update mechanism and
either leave this flag off (and wire their own gate) or reuse the same
name with a different backend.
