# ADR 0007: Core updates itself as a static binary

- Status: accepted as direction on 2026-09-23; amended on 2026-09-24 so the
  owner runs updates from the command line or API and native installs have
  no update UI (decisions 2–4, 6, 9 and 10–14); rollout pending
- Date: 2026-09-18
- Issue: [#1308](https://github.com/srcfl/ftw/issues/1308)
- Also decides the version scheme, tracked in
  [#1314](https://github.com/srcfl/ftw/issues/1314). The stable-channel defect
  is tracked separately in [#1313](https://github.com/srcfl/ftw/issues/1313).
- Replaces, when shipped: the `ftw-updater` sidecar, the update IPC volume
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
still built, tested and documented. Existing 1.x and 2.x boxes will wait for
the new migration path; their support code cannot be removed merely because
the 3.x line has moved on.

**Data migration landed inside the updater.** The SQLite → DuckDB change made
the rollback point a copy of the whole history (#1302) and gave the sidecar a
six-hour readiness budget. That is a storage concern living in delivery code.

**Every merge became a release.** Changesets bumps the version on each Version
Packages merge, and each bump is a beta. A tester's box sees a new update
almost daily.

**The version number stopped measuring anything.** FTW was 0.x from `v0.1.0`
on 12 April to `v0.130.4` on 16 July. On 17 July one PR (#574, a removal of
the legacy remote-access stack) merged with a `major` changeset, and
Changesets turned 0.130.4 into 1.0.0. Nobody decided FTW was 1.0. The Home
Link removal (ADR 0006) made 2.0.0, and the calendar removal made 3.0.0.
Every major is a removal. Of the 437 changesets merged so far, 3 are major,
97 minor and 337 patch, because
[`.changeset/README.md`](../../.changeset/README.md) maps every new driver,
flag, endpoint or UI piece to `minor`. No client compares Core's version; the
state schema, the app protocol and the driver host API carry their own
numbers. The only code that reads it is the update checker's ordering. And
the stable channel resolves to `v2.3.2` from 29 August, a major behind beta,
while the docs call it the default (#1313).

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

**On native installs, Core downloads its next release, verifies it and swaps
to it. No native update needs a Docker socket or Docker engine.**

1. **The release artifact is the existing tarball.** Core fetches
   `ftw-linux-<arch>.tar.gz` and its `.sha256` from the GitHub Release the
   checker already resolves, verifies the digest, and unpacks into
   `releases/<tag>/` under the install root. Only `vX.Y.Z` and
   `vX.Y.Z-beta.N` tags are accepted, as today. The beta workflow must
   publish the complete native package before any native beta can install
   itself; stable must promote the exact package tested on beta.

2. **Two slots and a launcher.** The install root holds `current`, and during
   an update `next`. systemd starts a launcher, not Core. On each start the
   launcher does one of three things: if `trial` exists, the previous start
   did not commit, so it moves `trial` aside, records the fallback and starts
   `current`; else if `next` exists, it renames `next` to `trial` and starts
   `trial`; else it starts `current`. A Core started from `trial` must reach
   readiness (API up, state open, drivers loaded) within a bounded time and
   stay ready for a short settling period. It then commits by renaming
   `trial` to `current` and the old `current` to `previous`. If it crashes
   or times out before that, it exits and systemd restarts the launcher,
   which falls back. Each rename is atomic on one file system, but
   the series is not one atomic operation. The launcher needs durable intent
   and recovery for a crash between renames.

3. **Rollback is the same swap in reverse.** `ftw rollback` and its API call
   offer `previous` only when that binary can read the current state schema.
   Core renames it to `next` and exits; the launcher runs it as a trial. When
   `current` cannot start at all, the launcher selects `previous` offline
   under the same schema rule. A native update keeps current data in place and
   takes no separate rollback point; binary rollback keeps that data, and a
   release that changes the state schema is covered by decision 13. A full
   data restore is an offline operation.

4. **Restart is an exit.** The unit has `Restart=always`. When Core needs a
   restart, for example after a settings change, it shuts down cleanly and
   exits; systemd starts the launcher, which starts `current`. No image, tag
   or file is consulted.

5. **No privilege for updates.** The install root belongs to the `ftw` user.
   Downloading, unpacking, renaming and exiting need nothing else. The host
   keeps patching itself with `unattended-upgrades` on the Pi image.

6. **The Pi image runs the native install.** It uses the same installer,
   launcher and unit as any other host,
   [`deploy/ftw-native.service`](../../deploy/ftw-native.service). The image
   drops the Docker engine and Compose. Mosquitto comes from `apt`, on the
   same host port as today. The monthly image built from `master` still
   installs Docker 2.x with the sidecar; it moves to native early, so new
   Pi users stop arriving on the old line.

7. **New Docker packaging becomes a plain image.** The Compose file has one
   Core service and Mosquitto, no sidecar and no `update-ipc` volume. The
   version notice stays, and `docker compose pull` is the documented path.
   Existing 1.x, 2.x and 3.x Docker installs remain in place until their owners
   use the guided migration. The Home Assistant add-on stays on Supervisor.

8. **Betas aim for a weekly cadence.** The owner dispatches
   `native-release.yml` after a Version Packages merge. Once the first
   native pilot is proven, it may run weekly; a merge alone does not publish.
   A hotfix beta needs a `release-blocker`. Stable promotes after one week
   on the home box and at least one other site.

9. **Transition code is deleted only after the affected boxes have migrated
   or left support.** A 3.5 version check is not enough: 1.x and 2.x boxes
   stay on their line until the new installer is proven. Remove each old path
   after checking the box inventory and its recovery need. Keep the legacy
   state-schema marker while any supported reader still needs it. Code on
   `master` that only an installed 1.x, 2.x or 3.x box would run is not part
   of their migration: those boxes run their installed binaries, and a 2.x
   repair builds from its own branch. `master` keeps what the migration and
   its way back need.

10. **The owner operates the host.** FTW's own work is the EMS and the
    Energy Planner. The service manager, when to update, copies of backups
    off the box and logs belong to the owner, by hand or through their own
    automation or agent. The project documents the steps; it does not run
    the owner's host.

11. **`ftw` is the operator command.** The installer puts `ftw` on `PATH`.
    It talks only to the local Core's HTTP API, needs no root, asks no
    questions and never starts Core:

    - `ftw status`: version, channel, published release, the last update's
      result and health, plus where to look next, such as `journalctl -u ftw`
    - `ftw update [--channel beta|stable] [--backup-dir DIR]`: install the
      next release on the saved channel; already current exits 0
    - `ftw rollback`: return to `previous` under decision 3
    - `ftw backup [--output-dir DIR]`: make, verify and optionally copy a
      full backup
    - `ftw support`: write the redacted support file
    - `ftw help`

    Each command prints its phases and elapsed time, bounds every request and
    exits non-zero only when its step failed. Every step is also an API call,
    so a script or agent can wrap either. Starting, stopping and logs stay
    with systemd.

12. **Native installs have no update UI.** The web UI shows the running
    version and channel, whether a newer release is published, the command
    that installs it and the last update's result. It has no update,
    rollback, restore, channel, snapshot or backup controls, and the setup
    wizard offers no update. Full backups come from `ftw backup` or the API.

13. **A release that changes the state schema is still one `ftw update`.**
    Core first makes and verifies a full backup of the current data, then
    installs the release; `--backup-dir` also copies that backup off the
    box. The way back across that step is an offline restore of the backup
    with the previous release. Until that path exists and is tested, the
    native release workflow must refuse a release whose state schema
    differs from the one before it.

14. **Install-time files are part of the release contract.** The installer
    writes the launcher, the unit and `ftw`; self-update replaces only
    `releases/<tag>`. These files must be complete before the first native
    user. A release that needs a newer launcher says so and refuses to
    prepare, rather than failing its trial. The installer can refresh the
    files on an existing native box. Installation, migration and the Pi
    image produce one layout: `/opt/ftw` with `ftw-native.service`.

## Versions

**The version returns to 0.x on the binary line.** Semver's major 0 means
anything may change, and that is the true state of FTW.

1. **The first binary release is `v0.131.0`.** It continues the counter that
   stopped at `v0.130.4`; those tags exist and cannot be reused. The old
   Docker lines stop receiving routine releases. A critical safety fix may
   still need an old-line release before a site can migrate.

2. **The reset happens at the native cutover, and nowhere else.** No Docker
   box uses Update Center to move between the 1.x/2.x, 3.x and native 0.x
   lines. Every old Docker box, whether on 1.x, 2.x or 3.x, moves directly to
   native 0.x through the same guided installer. It performs the move after
   a verified full backup off the box and checks that the new service, data
   and devices work. It must retain a way back to the old installation if
   that check fails. No epoch rule or
   transitional release is needed.

3. **There is no major bump in 0.x.** `major` leaves the changeset rules, and
   the changeset check rejects it.

4. **Minor means a person notices it or must act:** a removal, a changed
   default, new hardware support. Patch is everything else and the default.
   The owner approves a minor in the PR; the author does not choose it.

5. **Betas stay `v0.Y.Z-beta.N`**, weekly, as decided above.

6. **Old discovery stays on the old line.** The published GitHub
   `releases/latest` response and the old Docker `:latest` aliases remain on
   safe 2.x releases while old stable clients read them. A native 0.x stable
   release must use exact tags without taking over that global latest slot.
   New Core code has a cross-major guard, but that cannot change a binary
   already installed on a box. Old stable boxes may still see an already
   published 2.x release, and old betas may still offer 3.x. Those buttons
   are not the migration path.

## What is lost

- **In-app update on Docker installs.** Old installations stay where they are.
  An owner who wants a new version uses the guided move to native 0.x.
  The old code cannot have its update button removed retroactively.
- **In-app update on Windows and macOS.** Manual replacement stays until a
  launcher exists for those platforms.
- **The capability handshake and the six-hour readiness budget.** Both existed
  to protect Core from an older sidecar. There is no sidecar.
- **The Compose and `.env` pin.** The launcher reads a directory, not a file.
- **Update, rollback, snapshot and backup controls in the web UI on native.**
  The owner, or their agent, runs `ftw` or the API. The UI keeps the version
  and the notice that a release exists.
- **The pre-update rollback point on native.** Nothing on native could restore
  it, and a schema step now takes a full backup instead (decision 13).

## What is kept, and why

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
  a few hundred lines of Go and under a hundred of shell. The `ftw` command
  is a thin HTTP client beside them.
- **Removed from the native UI:** the Updates dialog, its component, channel,
  snapshot and backup sections, and the setup wizard's update offer.
- **Existing Docker boxes.** The same guided installer accepts 1.x, 2.x and
  3.x without an intermediate update. It first creates and verifies a full
  backup kept off the box. It preserves config, history, site identity and
  device goals, checks the new service and connected devices, and can return to the
  prior installation on failure. Whether it reuses or copies the data path is
  an installer choice that must be tested on real layouts.
- **Disk.** Two or three release directories, tens of megabytes each, instead
  of gigabytes of images (#1305).
- **RAM on the Pi.** No Docker engine, no containerd, no sidecar.
- **Changeset rules change.** `.changeset/README.md` and
  `changeset-check.yml` lose `major`, and the README states the minor rule
  above.
- **Release publication must preserve the old stable slot.** #1313 remains a
  user-visible gap. Native 0.x publication must not move GitHub latest or old
  Docker latest away from the 2.x maintenance line.
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
  add later by packaging the same tarball. It gives no health-gated commit
  and no automatic fallback, so it does not replace the slot model. Deferred.
- **Calendar versions (`2026.9.x`).** They sort after 3.x with the existing
  comparator and claim nothing about maturity, so they could land on the
  Docker line today. They drop the signal a minor carries in 0.x: that a
  person must act. Not chosen.
- **Reset to 0.x before the cutover.** Needs an epoch rule in the checker and
  a transitional release every box must pass through, the paired pattern
  that #1164 and #1302 made painful. Rejected.
- **Moving tags with a container watcher.** No immutability, no rollback.
  Rejected.

## Evidence required before rollout

- Update and rollback on the home box (Raspberry Pi 4, 7.5 GB history) with
  timings for download, swap and readiness.
- An induced crash during a trial, and one just after readiness, falls back
  to `current` with no operator action, and `ftw status` reports it.
- `ftw update` on a box whose new Core takes longer than five minutes to
  become ready keeps reporting progress and does not report a failure while
  the trial is inside its deadline.
- A release that changes the state schema installs with one `ftw update`,
  and the way back from it is tested.
- A script that runs `ftw update` unattended gets the same result and exit
  code as a person at the terminal.
- Disk use after ten updates stays at the retained slots.
- The Docker-to-binary installer path on a box with data in `~/ftw/data`.
- A freshly flashed Pi image comes up as the same native layout and passes
  these checks.
- A verified full backup copied off the box before cutover, plus a tested
  restore or return to the old installation after a failed cutover.
- Checks that history, site identity, device goals and live device state
  survive the move without two Core processes controlling the same site.
