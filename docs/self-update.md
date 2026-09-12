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
Version Packages PR and updates [`package.json`](../package.json) plus
[`CHANGELOG.md`](../CHANGELOG.md).

After that PR merges:

1. run [`beta.yml`](../.github/workflows/beta.yml) with `vX.Y.Z-beta.N`;
2. validate that immutable build on real sites;
3. manually dispatch [`release.yml`](../.github/workflows/release.yml) from that
   same commit and set `source_beta` to the exact candidate tested on sites;
4. stable promotion verifies that the selected beta tag resolves to the exact
   stable candidate commit;
5. release assets publish `vX.Y.Z` and move the stable aliases.

A hand-pushed stable tag does not publish assets. Use `release.yml`; its
explicit dispatch binds the chosen beta before `release-assets.yml` can run.

Stable therefore cannot be the first public channel for new code. Beta and
stable use different tags for the same source commit and the same Core and
updater image digests. The beta prerelease records both index digests; stable
fails if either beta tag moves and promotes only those recorded digests. The
first stable promotion also records the chosen beta and both digests on the
stable release. Asset reruns must reuse that receipt and cannot select a newer
beta from the same commit. The candidate image already contains the stable
product version. Compose passes
its pinned `FTW_IMAGE_TAG` into Core so status, update
checks and fleet reports still show the exact beta tag during site validation.
Core accepts only that build-bound beta tag or the baked stable version; another
environment value cannot invent a release identity. Stable promotion only adds
stable aliases to that validated manifest.

## Immutable update targets

The checker uses GitHub Releases to select a released version and GHCR to prove
that its exact image tag exists. The updater installs the immutable tag, never
the moving `:latest` or `:beta` alias. This avoids the race where a release
exists before a moving image alias has advanced.

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

Core updates include the compiled Energyplan worker. They require no Python
service. Core DP remains available if the worker fails or returns an invalid plan.

Portable `.ftwbak` archives include the complete persistent directory, cold
history, custom/managed drivers and component inventory. They are independently
verified before publication and can be downloaded off-device. Safe restore
retains the pre-restore directory and automatically reactivates it when the
restored Core fails health. See [backup-and-restore.md](backup-and-restore.md).

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

## Scope: the host is not updated here

Self-update covers Core, the updater sidecar, the Optimizer and signed
drivers — never the host operating system, kernel or Docker engine. The
Raspberry Pi appliance image keeps its own host patched with
`unattended-upgrades` ([rpi-image.md](rpi-image.md#host-os-security-updates));
on every other deployment the host and engine belong to the operator's own
package and service management.

## Operator use

The version badge selects `stable` or `beta`, checks availability and starts
an update. Changing channel does not deploy anything. A skipped version remains
hidden only until a newer version appears.

**Restart** stops and starts the existing Core container, then checks its health.
It keeps that container's image and environment, even if `.env` or Compose now
names a different release. It does not apply changes to Compose; use the update
flow for a new image. Core sends `restart_existing` so an older updater refuses
before it can pull or replace anything. If FTW reports that safe restart needs a
newer updater, update Core and updater together using the paired commands below.
A normal Core update also asks the updater to replace itself with the same tag
after Core passes its health check.

A 2.x site that wants a published 3.x pair must not use orange Update. That
click moves Core only. Use the [paired upgrade script](upgrade-paired-release.md)
so the updater is installed first. The commands below are the same ordering
if you prefer to type them by hand.

For manual updates, install the updater first while the existing Core still
runs. Set `FTW_UPDATER_IMAGE_TAG` in the project's `.env` to the published
immutable release tag. Keep the current `FTW_IMAGE_TAG` until the updater is
installed. Use your installation's Compose project and override files:

```bash
cd ~/ftw
docker compose pull ftw-updater
docker compose up -d --no-deps ftw-updater
docker compose images ftw-updater
docker compose logs --tail 20 ftw-updater
```

Check that the running updater uses the intended release image and has started
its socket listener. Then use Update Center to update Core. A manual Core
replacement must first create and verify a full backup; pin `FTW_IMAGE_TAG` to
the same tag, then pull and recreate only the Core service.

### First DuckDB upgrade

The [paired upgrade script](upgrade-paired-release.md) is the supported
operator path from a 2.x Compose site. The updater shipped before the fix for [#1164](https://github.com/srcfl/ftw/issues/1164)
waits only 30 minutes and can revert the Core image without its matching data.
It replaces itself only after Core becomes ready. **Installing a new Core does
not fix the old updater before that first upgrade.** Install an updater release
that contains the fix using the updater-only steps above before starting the
DuckDB upgrade. `v3.2.0-beta.1` does not contain this fix.

When `FTW_SELFUPDATE_ENABLED=1`, the new Core checks `GET /capabilities` on the
updater Unix socket before loading config or opening state. It requires protocol
1 and `preserve_core_on_readiness_failure: true`. It waits up to 30 seconds for
the socket to start. An old, unknown or unavailable updater makes Core exit with
`update ftw-updater first`, before it changes any data. The old updater can then
revert that refused image safely. This guard cannot protect a Core already
running `v3.2.0-beta.1`; use the stopped-updater procedure below for that case.
Native installations with self-update disabled do not need an updater.

Core first imports the catalog, site history and energy accounting, including
the latest counters. It then starts live collection and serves its API while
the large SQLite sample table and older Parquet files import in the background.
The startup page and history status show progress. Historical coverage stays
incomplete until all sources pass verification; a missing sample during this
phase does not mean zero consumption. Unknown time bounds apply to the whole
historical view. New writes go to DuckDB throughout the background import.

An interrupted import resumes from committed progress on the next Core start.
An import error leaves live collection running and reports incomplete history.
Keep the original sources and the verified pre-update full backup. Core rejects
a new full-history export until import finishes. Fix the reported source or
storage problem before restarting Core to resume the import.

Raw-history retention waits until the full import passes. Site-history rollups
remain available after the initial seed. Import scratch directories belong to
the importer; it removes leftovers on resume while keeping original sources.

The fixed updater allows six hours for Core startup. This is a waiting budget,
not a promise that every migration will finish within it. Core still needs a
working `/api/status` before the updater reports success. A failed readiness
check leaves the new container and data in place and reports failure. To return
to an older version, stop Core and restore a verified full backup with its
matching image; changing only the image can lose history.

### An updater stopped during migration

Leave the running Core alone until its import and API readiness have been
checked. Save the updater's existing `state.json`, the pre-update full backup,
snapshot directory and verified previous Core image identity before starting
the updater again. Older updaters hold the current job's previous image ID only
in memory until success; a saved `previous_image_id` may belong to an earlier
update. Use the backup inventory and pre-update Docker evidence to resolve it.

After readiness, start or replace **only** `ftw-updater` with `--no-deps` using
the fixed release. Its startup recovery marks an interrupted update as failed;
it does not restart Core or replay an image rollback. This records the lost
supervision honestly even if Core has since finished. Keep that record and
check Core's installed image and history status separately. Do not repeat the
Core update or overwrite the job as `done` merely to clear its failed status.

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
- Energyplan updates and rolls back with the Core image. There is no separate
  optimizer channel, image update or rollback.
- Signed Lua drivers are versioned independently in `srcfl/device-drivers`.
  Main publishes `drivers-beta`; `drivers-stable` promotes the exact signed
  beta commit and retains per-driver version history. See
  [device-repository.md](device-repository.md).

## Retiring the Python service

After installing this Core/updater pair and checking that Energyplan is healthy,
run the updater binary with `-retire-python` and the installation's `-compose`
path. The command starts a short-lived helper from the exact running updater
image, with the project mounted writable. It backs up each changed Compose file, removes only the old planner service
and FTW socket wiring, validates the merged files, and removes the retired
container from the same Compose project, including an orphan left by an earlier
Compose edit. Custom services and persistent data
stay intact. Recreate Core at its pinned image to release the old socket mount.
An older updater can install this release while Python still runs; retire the
service only after the new updater is installed.
