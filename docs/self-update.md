# Updates and release channels

FTW has two channels:

| Channel | Tag form | Purpose |
|---|---|---|
| `beta` | `vX.Y.Z-beta.N` | Every new candidate and real-site validation |
| `stable` | `vX.Y.Z` | Promotion of the exact commit already tested as beta |

Stable is the default. Persisted installations that still say `edge` are
migrated to `beta`; no edge releases are published or accepted.

## Release progression

User-visible changes land with a Changeset. The Changesets workflow opens the
Version Packages PR and updates `package.json` plus `CHANGELOG.md`.

After that PR merges:

1. run `beta.yml` with `vX.Y.Z-beta.N`;
2. validate that immutable build on real sites;
3. manually dispatch `release.yml` from that same commit;
4. stable promotion checks that the beta tag and pair manifest match the exact
   stable candidate commit;
5. release assets keep the stable release as a draft while they build Core and
   updater;
6. the workflow checks both exact tag digests, adds `ftw-control-plane.json`,
   checks the uploaded file, then publishes the draft and moves stable aliases.

Stable therefore cannot be the first public channel for new code. Beta and
stable may have different tags but identify the same source commit.

## Immutable update targets

The checker uses GitHub Releases to select a release. It accepts a Core release
only when `ftw-control-plane.json` names that release and pins both
`ghcr.io/srcfl/ftw` and `ghcr.io/srcfl/ftw-updater` by digest. It resolves both
exact tags again and rejects a missing or changed digest. The updater installs
the exact `vX.Y.Z[-beta.N]` tag, never `:latest` or `:beta`.

Release notes are best-effort UI data. Failure to fetch notes does not weaken
tag resolution or image verification.

## Update Center and component boundaries

The Update Center reports and records Core, Optimizer and Driver operations
separately. A component history survives Core container recreation. Core stays
the safety authority regardless of which optional component is being updated.

```text
Core + updater    paired control plane; Core owns state, dispatch and safety
Optimizer         independent image and version; protocol handshake; optional
Drivers           signed Lua artifacts; one driver/version activated at a time
```

The main process checks versions and exposes update status. A separate
`ftw-updater` container owns the Docker socket and performs immutable
pull/recreate operations over a Unix socket. Core never mounts the Docker
socket.

Before every Core update, Core creates a mandatory, consistent local rollback
point for `state.db` and configuration. An older client request cannot skip it.
These bounded points remain on the same disk and are deliberately labelled
**Local rollback points**, not full backups. Older incomplete snapshots are
visible but cannot be restored.

Core and updater form one release pair. Updater `/status` must report protocol
2 or later, capability `control-plane-pair-v1`, and the exact Core release.
Core checks this before it opens or migrates state. A v1.3.1 updater lacks this
contract, so a new Core stays unready and does not open state with it.

The updater saves both prior image IDs before it pulls the pair. A detached
helper starts the matching updater first, checks its handshake, then starts
Core. Core must pass `/api/status` after state and migrations are ready. The
helper marks the update done only after both containers report the exact target
image refs. On failure it restores updater first and then Core. The final state
remains failed even when rollback succeeds.

The updater also requires a running, healthy `ftw-optimizer` service before it
updates Core. If the merged Compose files lack that service, or its health
check fails, the update stops before pulling or replacing the pair. The updater
does not edit operator override files; use the
[legacy upgrade guide](upgrade-from-legacy.md) to add the sidecar safely.

Portable `.ftwbak` archives include the complete persistent directory, cold
history, custom/managed drivers and component inventory. They are independently
verified before publication and can be downloaded off-device. Safe restore
retains the pre-restore directory and automatically reactivates it when the
restored Core fails health. See [backup-and-restore.md](backup-and-restore.md).

Optimizer-only updates use `optimizer-vX.Y.Z[-beta.N]`, recreate and
health-check only `ftw-optimizer`, and never replace Core. Optimizer has its
own SemVer line, which starts at 1.3.2. Core checks `name=ftw-optimizer`, exact
protocol 1, plan schema 1, and required features. It does not compare optimizer
and Core versions. The old optimizer releases 1.3.1-beta.1 and 1.3.1 remain
compatible through protocol 1. A request for recourse or multistage also needs
that feature; auto transport falls back when it is missing. Failure restores
the prior Optimizer image while Core continues on its Go fallback.

A Driver update downloads one signed artifact, verifies hash, metadata and host
API compatibility, then atomically activates exactly that version. Core puts
the affected device in its safe default mode during restart and accepts the new
driver only after fresh telemetry reports the same stable hardware identity.
Failure automatically reactivates the previous artifact; no other driver or
system component changes.

Status is written atomically to the shared volume and is reconciled into the
persistent component history after Core recreation.

The updater accepts only known components and `vX.Y.Z` or
`vX.Y.Z-beta.N` targets.

## Operator use

The version badge selects `stable` or `beta`, checks availability and starts
an update. Changing channel does not deploy anything. A skipped version remains
hidden only until a newer version appears.

For manual Core + updater operation, set one exact release for both:

```bash
cd ~/ftw
RELEASE=vX.Y.Z # replace with an approved beta or stable release
FTW_IMAGE_TAG="$RELEASE" FTW_UPDATER_IMAGE_TAG="$RELEASE" \
  docker compose pull ftw ftw-updater
FTW_IMAGE_TAG="$RELEASE" FTW_UPDATER_IMAGE_TAG="$RELEASE" \
  docker compose up -d --no-deps ftw-updater
FTW_IMAGE_TAG="$RELEASE" FTW_UPDATER_IMAGE_TAG="$RELEASE" \
  docker compose up -d --no-deps ftw
```

Do not start Core first. Do not mix two release tags. For old layouts, use the
migration script below instead of these commands.

Manage Optimizer and Drivers independently in Update Center. A blanket
`docker compose pull` is intentionally not the documented upgrade procedure.

Use the [legacy upgrade guide](upgrade-from-legacy.md) before updating an older
Compose layout with hard-coded or pre-FTW image names.

## Enabling

The shipped Linux Compose topology sets `FTW_SELFUPDATE_ENABLED=1` and mounts
the updater socket/status volume. Native deployments normally omit the flag and
use their package or service manager.

When self-update is disabled, production UI controls and handlers are disabled.
An unstamped `dev` build keeps the probe visible so the restart flow can be
tested locally.

## Independent release progression

- Core and the updater sidecar are built from Core `vX.Y.Z[-beta.N]` releases.
- Optimizer uses its own `optimizer-vX.Y.Z[-beta.N]` GitHub tags and
  `ftw-optimizer:vX.Y.Z[-beta.N]` images. Stable promotion requires the exact
  beta commit.
- Signed Lua drivers are versioned independently in `srcfl/device-drivers`.
  Main publishes `drivers-beta`; `drivers-stable` promotes the exact signed
  beta commit and retains per-driver version history. See
  [device-repository.md](device-repository.md).

## Compatibility and rollout gate

| Pair | Result |
|---|---|
| New Core + same-release updater, protocol 2+, pair capability | Allowed |
| New Core + v1.3.1 updater or updater without the pair capability | Core stays unready before state opens |
| New Core + different updater release | Core stays unready before state opens |
| Core/updater release with one missing or changed image digest | Not shown as an update and not published |
| Core + optimizer 1.3.1-beta.1, 1.3.1, or 1.3.2+ with protocol 1 | Allowed; optimizer version need not match Core |

Do not merge or roll out a Core/updater pair until the release manifest checks
pass, the cross-version updater test passes, migration restores both old image
IDs after a readiness failure, `make verify` passes, and the active beta pilot
reports PASS.
