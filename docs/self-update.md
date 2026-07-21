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
4. stable promotion verifies that a matching beta tag resolves to the exact
   stable candidate commit;
5. release assets publish `vX.Y.Z` and move the stable aliases.

Stable therefore cannot be the first public channel for new code. Beta and
stable may have different tags but identify the same source commit.

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

The updater also requires a running, healthy `ftw-optimizer` service before it
updates Core. If the merged Compose files lack that service, or its health
check fails, the update stops before pulling or replacing Core. The updater
does not edit operator override files; use the
[legacy upgrade guide](upgrade-from-legacy.md) to add the sidecar safely.

Portable `.ftwbak` archives include the complete persistent directory, cold
history, custom/managed drivers and component inventory. They are independently
verified before publication and can be downloaded off-device. Safe restore
retains the pre-restore directory and automatically reactivates it when the
restored Core fails health. See [backup-and-restore.md](backup-and-restore.md).

Optimizer-only updates use `optimizer-vX.Y.Z[-beta.N]`, recreate and
health-check only `ftw-optimizer`, and never replace Core. Failure restores the
previous Optimizer image while Core continues on its Go fallback.

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

For manual Core + updater operation:

```bash
cd ~/ftw
docker compose pull ftw ftw-updater
docker compose up -d --no-deps ftw ftw-updater
```

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
