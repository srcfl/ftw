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
4. stable promotion verifies the published beta pair manifest, both live
   immutable digests and the exact candidate commit;
5. stable is created as a draft; release assets publish `vX.Y.Z` and move the
   stable aliases only after the Core/updater pair has been re-verified.

Stable therefore cannot be the first public channel for new code. Beta and
stable may have different tags but identify the same source commit.

## Immutable update targets

Every Core release carries `ftw-control-plane.json`. It names the release and
source commit and pins the immutable manifest digests for both
`ghcr.io/srcfl/ftw:<tag>` and `ghcr.io/srcfl/ftw-updater:<tag>`. The checker
announces an update only when the public GitHub Release contains this manifest
and both live tag digests still match it. A missing updater tag, missing
manifest or moved digest is fail-closed.

The updater installs the immutable tag for both services, never the moving
`:latest` or `:beta` alias. Existing exact tags are not overwritten by release
reruns. Stable remains a draft and is invisible to update checkers until the
pair gate succeeds.

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

Core and updater expose a versioned control-plane handshake. When self-update
is enabled, Core verifies the updater protocol and exact release version before
opening or migrating `state.db`; health and readiness remain 503 until they
match. This is the compatibility barrier that prevents an older updater from
installing a new Core by itself.

Before every Core update, Core creates a mandatory, consistent local rollback
point for `state.db` and configuration. An older client request cannot skip it.
These bounded points remain on the same disk and are deliberately labelled
**Local rollback points**, not full backups. Older incomplete snapshots are
visible but cannot be restored.

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

A Core update captures both running image IDs, pulls both immutable targets,
then delegates the replacement to a detached transaction helper. The helper
survives updater recreation, starts the matching updater before Core, requires
full Core readiness, and writes `done` only after both running image references
match the requested release. Failure restores the previous updater first and
then the previous Core from the two retained image IDs.

The updater accepts only known components and `vX.Y.Z` or
`vX.Y.Z-beta.N` targets.

## Operator use

The version badge selects `stable` or `beta`, checks availability and starts
an update. Changing channel does not deploy anything. A skipped version remains
hidden only until a newer version appears.

For manual Core + updater reconciliation, select one verified release and use
the same immutable tag for both services (replace the example tag):

```bash
cd ~/ftw
export FTW_IMAGE_TAG=v1.4.1-beta.1
export FTW_UPDATER_IMAGE_TAG="$FTW_IMAGE_TAG"
docker compose pull ftw ftw-updater
docker compose up -d --no-deps ftw-updater
docker compose up -d --no-deps ftw
curl -fsS http://127.0.0.1:8080/api/status
```

Manage Optimizer and Drivers independently in Update Center. A blanket
`docker compose pull` is intentionally not the documented upgrade procedure.

Use the [legacy upgrade guide](upgrade-from-legacy.md) before updating an older
Compose layout with hard-coded or pre-FTW image names.

## Compatibility and first rollout gate

| Running Core | Running updater | Result |
|---|---|---|
| v1.3.1 | v1.3.1 | Legacy behavior; cannot install the paired release atomically |
| v1.3.1 | first fixed release | Pair-capable updater can install the next matched pair |
| first fixed release | v1.3.1 | Core stays unready and does not migrate; the v1.3.1 update attempt times out and restores old Core |
| fixed version A | fixed version B | Core stays unready until both versions match |
| same fixed version | same fixed version | Paired in-app update, readiness gate and two-image rollback are supported |

The first fixed release is a bootstrap boundary. Before stable promotion:

1. publish a beta whose pair manifest and both digests pass the release gate;
2. verify a v1.3.1 in-app attempt restores old Core without migrating data;
3. manually reconcile a test site to the same beta tag for Core and updater;
4. test beta-to-beta in-app update with injected updater, Core and readiness
   failures and confirm both prior image IDs are restored;
5. promote the exact tested beta commit. Do not make stable public when any of
   these gates is missing.

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
- Signed Lua drivers are versioned independently. Master publishes
  `drivers-beta`; `drivers-stable` is an explicit promotion and retains signed
  version history. See [device-repository.md](device-repository.md).
