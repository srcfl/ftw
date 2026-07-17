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

## Runtime architecture

The main process checks versions and exposes update status. A separate
`ftw-updater` container owns the Docker socket and performs the pull/recreate
over a Unix-socket command from core. Core never mounts the Docker socket.

```text
core ── update request/status ── Unix volume ── updater ── Docker socket
  └── state/config snapshot                         └── pull immutable tag
```

Before update or rollback, core creates a consistent bounded snapshot of state
and configuration. Status is written atomically to the shared volume and
survives recreation of core. Optimizer-only updates recreate and health-check
the optimizer without replacing core; failure restores the previous optimizer
image.

The updater accepts only known components and `vX.Y.Z` or
`vX.Y.Z-beta.N` targets.

## Operator use

The version badge selects `stable` or `beta`, checks availability and starts
an update. Changing channel does not deploy anything. A skipped version remains
hidden only until a newer version appears.

For manual Compose operation:

```bash
cd ~/ftw
docker compose pull
docker compose up -d
```

Use the [legacy upgrade guide](upgrade-from-legacy.md) before updating an older
Compose layout with hard-coded or pre-FTW image names.

## Enabling

The shipped Linux Compose topology sets `FTW_SELFUPDATE_ENABLED=1` and mounts
the updater socket/status volume. Native deployments normally omit the flag and
use their package or service manager.

When self-update is disabled, production UI controls and handlers are disabled.
An unstamped `dev` build keeps the probe visible so the restart flow can be
tested locally.

## Driver releases

Signed Lua drivers are versioned independently from core but follow the same
policy: master changes publish `drivers-beta`; `drivers-stable` is an
explicit promotion. See [device-repository.md](device-repository.md).
