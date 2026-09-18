# ADR 0007: Core updates itself as a static binary

- Status: proposed
- Date: 2026-09-18
- Issue: [#1308](https://github.com/srcfl/ftw/issues/1308)
- Replaces, when accepted: the `ftw-updater` sidecar, the update IPC volume
  and the Compose and `.env` pinning described in
  [self-update.md](../self-update.md). That document stays the description of
  what ships until this one is implemented.

## Context

The update path is the largest piece of non-control code in Core, and it is
where testers' boxes break. Measured on `master` on the date above:

| Measure | Value |
|---|---|
| Go in the update path (sidecar, checker, API, rollback point) | ~6 000 lines |
| Tests for it | ~4 300 lines |
| CI for beta, stable and assets | ~2 000 lines |
| Install, migration and paired-upgrade scripts | ~2 100 lines |
| Of which one-time transition code | ~2 800 lines |
| Commits touching the update path, last 90 days | 73 of 867 |
| Betas published 7–18 September | 20 |
| Open updater issues | 10 |

Three things keep a box safe through an update: a rollback point of
`state.db` and config before it, a readiness check after it, and immutable
release tags. Almost none of the code above is that. It is delivery mechanics
that follow from one early choice: Docker is the way in.

**Docker needs a privileged helper.** Core must not hold the Docker socket, so
a sidecar does. The sidecar then has to replace itself after every Core
update, pin the tag in `.env`, see the Compose project directory at the host's
own path, derive the same project name, and answer a capability handshake
before Core may open its state. The downgrade on 5 September (#1073), the
restart that undid a rollback (#989), the image-only rollback during a long
migration (#1164) and the images that are never pruned (#1305) all come from
this shape.

**Transition code never left.** Retiring Python, migrating legacy Compose
layouts, the paired 2.x → 3.x upgrade and the old product-name alias are
still built, tested and documented for boxes that have already moved.

**Data migration landed inside the updater.** The SQLite → DuckDB change made
the rollback point a copy of the whole history (#1302) and gave the sidecar a
six-hour readiness budget. That is a storage concern living in delivery code.

**Every merge became a release.** Changesets bumps the version on each Version
Packages merge, and each bump is a beta. A tester's box sees a new update
almost daily.

Two facts make a simpler shape available now. Core is already a static binary:
[`scripts/build-core.sh`](../../scripts/build-core.sh) builds with
`CGO_ENABLED=0` and `-tags=netgo,osusergo`. And every release already
publishes `ftw-linux-{amd64,arm64}.tar.gz` with the binary, `ftw-backup`,
`web/`, `drivers/`, the Energyplan bundle and a sha256 file
([`release-assets.yml`](../../.github/workflows/release-assets.yml)).

Comparable projects do less. evcc ships no in-app update at all: `apt` or
`docker compose pull`, and the UI only says a new version exists. Syncthing,
Caddy and Tailscale download their next binary, verify it, replace themselves
and restart. The Home Assistant add-on model lets Supervisor own updates.

## Decision

**Core downloads its next release, verifies it and swaps to it. No process on
the box holds the Docker socket, and no Docker engine is required.**

1. **The release artifact is the existing tarball.** Core fetches
   `ftw-linux-<arch>.tar.gz` and its `.sha256` from the GitHub Release the
   checker already resolves, verifies the digest, and unpacks into
   `releases/<tag>/` under the install root. Only `vX.Y.Z` and
   `vX.Y.Z-beta.N` tags are accepted, as today.

2. **Two slots and a launcher.** The install root holds `current`, and during
   an update `next`. systemd starts a launcher, not Core. On each start the
   launcher does one of three things: if `trial` exists, the previous start
   did not commit, so it moves `trial` aside, records the fallback and starts
   `current`; else if `next` exists, it renames `next` to `trial` and starts
   `trial`; else it starts `current`. A Core started from `trial` must reach
   readiness (API up, state open, drivers loaded) within a bounded time and
   then commits by renaming `trial` to `current` and the old `current` to
   `previous`. If it crashes or times out, it exits and systemd restarts the
   launcher, which falls back. Directory renames are atomic on one file
   system, so there is no half state.

3. **Rollback is the same swap in reverse.** The UI offers `previous`. Core
   renames it to `next`, restores the state rollback point when the schema
   requires it, and exits. The launcher runs `previous` as a trial. The
   rollback point stays what #1302 made it: `state.db` and config, taken before
   every Core update, never a copy of history.

4. **Restart is an exit.** The unit has `Restart=always`. "Restart" in the UI
   makes Core shut down cleanly and exit; systemd starts the launcher, which
   starts `current`. No image, tag or file is consulted.

5. **No privilege for updates.** The install root belongs to the `ftw` user.
   Downloading, unpacking, renaming and exiting need nothing else. The host
   keeps patching itself with `unattended-upgrades` on the Pi image.

6. **The Pi image runs the binary.** [`deploy/ftw.service`](../../deploy/ftw.service)
   becomes the shipped unit. The image drops the Docker engine and Compose.
   Mosquitto comes from `apt`, on the same host port as today.

7. **Docker becomes a plain image.** The Compose file has one Core service and
   Mosquitto, no sidecar and no `update-ipc` volume. Self-update is off, the
   version notice stays, and `docker compose pull` is the documented path. The
   Home Assistant add-on stays on Supervisor.

8. **Betas are weekly.** `beta.yml` runs on a schedule, not after every
   Version Packages merge. A hotfix beta needs a `release-blocker`. Stable
   promotes after one week on the home box and at least one other site.

9. **Transition code is deleted once every known box runs 3.5 or later.**
   Python retirement, legacy Compose migration, the paired upgrade script and
   the old product-name alias go. The legacy state-schema marker path goes
   when no 3.5.x box remains.

## What is lost

- **In-app update on Docker installs.** They keep the version notice and get a
  command. Anyone who wants the button installs the binary.
- **In-app update on Windows and macOS.** Manual replacement stays until a
  launcher exists for those platforms.
- **The capability handshake and the six-hour readiness budget.** Both existed
  to protect Core from an older sidecar. There is no sidecar.
- **The Compose and `.env` pin.** The launcher reads a directory, not a file.

## What is kept, and why

- **The rollback point.** `state.db` and config before every Core update, in
  the #1302 form. It is what makes a schema step reversible.
- **The readiness gate.** A trial that does not become ready does not become
  `current`. The existing `/api/status` check is the signal.
- **Immutable tags and the release checker.** `selfupdate` keeps resolving
  GitHub Releases and refusing moving aliases. Only the trigger changes.
- **The signed driver channel and `.ftwbak` full backups.** They are
  independent of how Core is delivered and do not change.
- **`history-migrate`.** It links DuckDB with CGO. It ships in the tarball as
  a second, dynamically linked binary, or moves into Core behind a build tag.
  That choice is made in its own PR.

## Consequences

- **Deleted:** `go/cmd/ftw-updater`, `go/internal/updateipc`,
  `Dockerfile.updater`, the Compose and `.env` pinning in `selfupdate`, the
  sidecar half of `api_selfupdate`, the `update-ipc` volume, and after step 9
  the scripts and guides listed there. Rough size: two thirds of the lines in
  the table above.
- **Added:** download and verify, the slot swap and commit, a launcher script
  and its tests, and installer support for the binary layout. Rough size:
  a few hundred lines of Go and under a hundred of shell.
- **Existing Docker boxes.** One last Docker release shows a banner that points
  at the installer. The installer stops the Compose project, leaves `./data`
  where it is, unpacks the tarball, writes the unit with that data directory
  and starts it. No data moves.
- **Disk.** Two or three release directories, tens of megabytes each, instead
  of gigabytes of images (#1305).
- **RAM on the Pi.** No Docker engine, no containerd, no sidecar.
- **Risk: the launcher is new and small, and it must be right.** It gets a
  shell test suite that runs every branch of its decision, and the home box
  runs an induced crash during a trial before this is accepted.
- **Risk: integrity rests on a sha256 fetched over HTTPS from the same
  release.** That matches what the image digest gives today. Signing the
  tarball with the driver channel's key is a natural follow-up, not a
  precondition.
- **Risk: users with customised Compose files.** They keep the plain image and
  lose only the button.

## Alternatives considered

- **Keep Docker, move the updater to a host systemd path unit.** Core writes a
  request file, a host script runs `compose pull` and `up`. Fewer lines than
  today, but it keeps Compose, `.env`, the project-path coupling and the Docker
  engine on the Pi. Rejected.
- **Debian package and `apt`, the evcc model.** Good for servers and easy to
  add later by packaging the same tarball. It gives no UI rollback and no
  health-gated commit, so it does not replace the slot model. Deferred.
- **Moving tags with a container watcher.** No immutability, no rollback.
  Rejected.

## Evidence required before acceptance

- Update and rollback on the home box (Raspberry Pi 4, 7.5 GB history) with
  timings for download, swap and readiness.
- An induced crash during a trial falls back to `current` with no operator
  action, and the UI reports it.
- Disk use after ten updates stays at the retained slots.
- The Docker-to-binary installer path on a box with data in `~/ftw/data`.
