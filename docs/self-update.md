# Updates and release channels

[ADR 0007](adr/0007-self-updating-binary.md) defines the move to a native
Core. `v0.131.0-beta.1` is the first published native beta. No native stable
release or guided migration for an existing box has shipped.

There are two channels:

| Channel | Tag | Use |
|---|---|---|
| `beta` | `v0.X.Y-beta.N` | Test each candidate on real sites |
| `stable` | `v0.X.Y` | Promote the same source commit after beta validation |

An old saved `edge` choice becomes `beta`. Native Core selects only published
0.x releases with a matching Linux package and checksum. Its launcher keeps
the current and previous releases for a same-schema rollback. A full backup
is still needed to recover older data or a failed disk.

## Existing 1.x, 2.x and 3.x boxes

Keep an existing Docker or earlier native site on its current version. On an
older Docker install, the web UI's Update and Restart buttons signal the
`ftw-updater` sidecar, which pulls a pinned image and recreates Core
([`go/internal/selfupdate`](../go/internal/selfupdate),
[`go/cmd/ftw-updater`](../go/cmd/ftw-updater)). That path is frozen with its
line. Do not use its Update button, an old Docker migration script, a moving
image alias, or a manual Core/updater swap to cross release lines. The old
code may still show a previously published update; it cannot be changed on a
box that has not installed new code. The scripts on `master` for
Docker-to-Docker migration and 2.x-to-3.x upgrades now exit before changing a
site.

The planned guided installer will move 1.x, 2.x and 3.x sites directly to
native 0.x. It must first make and verify a full backup held off the box,
then stop the old Core, preserve config, history, identity and goals, install
the native service, check data and connected devices, and retain a tested way
back. It is not ready for users. The [fresh Linux installer](../scripts/install.sh)
is only for an empty 64-bit host and refuses a known existing site. Until
then, [Try the 0.x beta](native-beta.md#coming-from-an-older-ftw) shows how
to test 0.x beside an old site.

GitHub `releases/latest` and the old Docker `:latest` aliases remain on the
2.x line for installed boxes. Native beta and stable releases use exact tags
without moving that global latest slot. An urgent safety repair may still
need an owner-approved 2.x release; it does not restart routine Docker
releases or provide a hop to 3.x. The old `beta.yml` and `release.yml`
workflows are guarded to that line. A release from either line does not deploy
itself to a box.

### Pilot: an older native systemd site

This is not the user migration path. `migrate-native` in the
[operator CLI](../scripts/ftwctl.py) moves an older **direct native systemd**
site, one whose unit runs `/opt/ftw/ftw` as the `ftw` user, onto 0.x release
slots. The owner's home box was moved with it. It stages the release under a
separate root, `/opt/ftw-native` by default, and switches the unit with a
drop-in, so such a box keeps a different layout from a fresh install. Run it
on another computer, through an SSH tunnel to the box's API:

```bash
ssh -N -L 18080:127.0.0.1:8080 box.example
python3 scripts/ftwctl.py --url http://127.0.0.1:18080 status
python3 scripts/ftwctl.py --url http://127.0.0.1:18080 backup --output-dir ~/FTW-backups
python3 scripts/ftwctl.py --url http://127.0.0.1:18080 migrate-native \
  --host box.example --tag v0.X.Y-beta.N \
  --data-dir /srv/ftw/data --config /app/data/config.yaml \
  --user-drivers /app/data/drivers --check-only
```

Remove `--check-only` and pass `--backup ~/FTW-backups/<printed-name>.ftwbak`
only after checking the paths against that box. The CLI requires a backup from
the last 24 hours whose size and SHA-256 match Core's verified archive. It
checks that the same archive still exists under the service's data bind on the
box and that its site identity matches the SSH host. This avoids copying a
large archive back to `/tmp` or mixing up two boxes on the same version. It
then changes only the systemd start override and compares version, health,
driver names and working driver count. On failure it tries the old start
command. If old Core cannot read the data after a failed trial, it restores
the verified archive before retrying old Core. Keep the printed recovery-copy
path if automatic recovery fails. The CLI prints the active phase, elapsed time and completed/total bytes when
known; it says when a total is unknown. If LAN auth is on, set `FTW_API_TOKEN`
in the CLI process environment. Never paste that token into a command line.

The pilot refuses Docker and Home Assistant installations; they remain on
their current line while their own layout and recovery path are tested. A
successful empty-data smoke test does not prove migration of a live
household.

## Native 0.x releases

User-visible changes need a Changeset. The Version Packages PR updates
[`package.json`](../package.json) and [`CHANGELOG.md`](../CHANGELOG.md).
After it merges, the owner dispatches
[`native-release.yml`](../.github/workflows/native-release.yml) on `master`
for an exact `v0.X.Y-beta.N` tag. The workflow checks the source commit,
runs `make verify`, builds ARM64 and AMD64 packages, verifies their hashes
and publishes a prerelease. A `dry_run` checks the build without creating a
tag or release. A retry may keep a published asset only if its bytes match.

A native beta must run for a week on the home box and at least one other real
site with no open `release-blocker`. Only then can the owner dispatch the same
workflow for `v0.X.Y` stable, naming the tested `source_beta`. Stable checks
the published beta receipt and package hashes and uses the same source
commit. Beta and stable contain different embedded version strings, so each
package has its own hash and receipt. A tag, green CI run or published package
alone is not field validation.

On a native site the owner runs updates on the machine, by hand or from their
own timer or agent. [Try the 0.x beta](native-beta.md) is the tester's
guide. The installer puts the `ftw` command on `PATH`:

```bash
ftw status                   # version, published release, last update, health
ftw update                   # install the next release on the saved channel
ftw update --channel stable  # change the channel first
ftw rollback                 # return to the previous release
```

`ftw update` first checks that the disk has room: three times the current
release, for the archive, the unpacked release and some margin for the data
on the same disk. Core then downloads and verifies the 0.x package, stages
the new slot and restarts through the launcher. A native update keeps the
data in place and takes no local rollback point. On a terminal each step
shows a bar with size, rate and time left and ends as one line with its
duration; in a log or script it is one line per step. Core records every
finished step with its own timing, so a step that ends between two reads is
still shown. `ftw update` waits through the restart, including a history
migration, and reports the version and health that result. A trial only
becomes current after readiness;
a failed trial falls back to the previous Core, and `ftw update` names the
Core that runs. Already current exits 0, so a script can run the step
unattended; a failed step exits 1. The same steps are Core API calls. A
release that changes stored data (the state schema) cannot be installed
natively yet; `ftw update` stops before it changes anything.

`ftw rollback` returns to the previous release when it reads the same data.
`ftw status` also shows free space for releases and backups, and any local
rollback points an older Core left; native Core does not use them, so they
can be deleted.
The web UI on a native install shows the running version, a published
release and the command; it has no update controls. See
[ADR 0007](adr/0007-self-updating-binary.md) and
[full backup and restore](backup-and-restore.md).

Docker 0.x runs the same package without the launcher. Change `FTW_VERSION`
in `.env` and run `docker compose up -d --build` to update or go back; there
is no automatic fallback. See [Docker](native-beta.md#docker).

Core, the compiled Energyplan worker and the Lua drivers pinned in
`drivers/BUNDLED_SOURCE.json` ship in one package, so `ftw update` and
`ftw rollback` move the drivers with Core. Core validates plans and keeps its
Go fallback. A newer driver installed from the signed channel runs until a
release catches up, and an older one chosen on purpose stays; see
[device repository](device-repository.md). There is no optimizer sidecar in
the native install.

## Host and old release details

FTW's Core update does not update the host operating system, kernel or Docker
engine. The operator handles host updates.

The old Docker workflows and their release receipts remain for an exceptional
2.x repair. The native release workflow cannot publish Docker images or move
old aliases. New Core code refuses cross-line update requests, but that guard
cannot change an older installed binary. Do not use an old tag or script to
bypass the guided migration.

Release notes keep the old state-schema markers for the remaining Docker
line. The native package carries its own state-schema receipt; the launcher
checks it before staging and refuses automatic rollback across a schema
change. Source and tests in `go/internal/nativeupdate` define that behavior.
