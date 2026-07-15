# Self-update

In-app "Update" and "Restart" buttons trigger `docker compose pull` +
recreate of the main service. The mechanism is split across three
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
│            │    update-ipc (tmpfs volume)   │    /var/run/docker.sock │
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

A GitHub Release is published the moment release-please's PR merges;
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

The sidecar accepts only `vX.Y.Z`, `vX.Y.Z-beta.N`, or
`edge-YYYYMMDDTHHMMSSZ-<sha>`. Falling through to a moving alias is exactly
what this boundary prevents.

The GH Releases API is still consulted, but only as a best-effort
lookup for the changelog body shown in the upgrade modal. A 404 there
(release not yet published, or never paired with the image) doesn't
block the upgrade.

`restart` is the dev-convenience action — no target required, no env
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
privilege to one small binary (~250 LOC, no network, no persistent
storage, a read-only bind of `docker-compose.yml`).

## State transitions

`state.json` is written to the shared `update-ipc` tmpfs volume. Every
step rewrites the whole file atomically (`tmp → rename`). Both ends
treat it as authoritative.

```
idle → pulling → restarting → done
                ↘
                  failed (on error, with stderr tail)
```

The tmpfs volume outlives the recreate of either container, so the new
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
`:latest` or emit stable RPi/Discord artifacts.

## Bootstrapping the first beta

An updater released before channel support rejects beta and edge targets by
design. The first beta installation therefore pins both containers once:

```env
FTW_IMAGE_TAG=v0.128.0-beta.1
FTW_UPDATER_IMAGE_TAG=v0.128.0-beta.1
```

Run `docker compose pull` and `docker compose up -d` with those values in
the deployment's `.env`. After that pair is running, channel selection and
future main-image updates work in the dashboard. The updater protocol is
kept backward compatible; updating the sidecar itself remains an explicit
compose operation.

## The seven endpoints

| Endpoint | Purpose |
|---|---|
| `GET  /api/version/check?force=1` | Cached GH Releases probe. `?force=1` bypasses the 3 h cache. |
| `POST /api/version/channel` `{channel}` | Persist `stable`, `beta`, or `edge` and clear the cached target. Does not deploy. |
| `POST /api/version/skip` `{version}` | Persist a dismissed version. Hides the badge until something newer ships. |
| `POST /api/version/unskip` | Clear the skip so the current latest resurfaces. |
| `POST /api/version/update` | Trigger `pull` + `up -d` for the currently-latest tag. |
| `POST /api/version/restart` | Trigger `pull` + `up -d --force-recreate`. Exists so the full flow can be exercised locally without waiting for a real release. |
| `GET  /api/version/update/status` | Pass-through of the sidecar's `state.json`. Polled every 2 s by the UI during the countdown. |

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
- **Rollback**: snapshot the pre-update image digest so a subsequent
  "Rollback" button can retag and recreate.

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
