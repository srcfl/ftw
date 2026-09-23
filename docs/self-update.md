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

Keep an existing Docker or earlier native site on its current version. Do not
use its Update button, an old Docker migration script, a moving image alias,
or a manual Core/updater swap to cross release lines. The old code may still
show a previously published update; it cannot be changed on a box that has
not installed new code. The scripts on `master` for Docker-to-Docker migration
and 2.x-to-3.x upgrades now exit before changing a site.

The planned guided installer will move 1.x, 2.x and 3.x sites directly to
native 0.x. It must first make and verify a full backup held off the box,
then stop the old Core, preserve config, history, identity and goals, install
the native service, check data and connected devices, and retain a tested way
back. It is not ready for users. The [fresh Linux installer](../scripts/install.sh)
is only for an empty 64-bit host and refuses a known existing site.

The [operator CLI](../scripts/ftwctl.py) now supports a **legacy native/systemd
pilot**. Run it on another computer, through an SSH tunnel to the box's API:

```bash
ssh -N -L 18080:127.0.0.1:8080 box.example
python3 scripts/ftwctl.py --url http://127.0.0.1:18080 status
python3 scripts/ftwctl.py --url http://127.0.0.1:18080 backup --output-dir ~/FTW-backups
python3 scripts/ftwctl.py --url http://127.0.0.1:18080 migrate-native \
  --host box.example --tag v0.131.0-beta.1 \
  --data-dir /srv/ftw/data --config /app/data/config.yaml \
  --user-drivers /app/data/drivers --check-only
```

Remove `--check-only` and pass `--backup ~/FTW-backups/<printed-name>.ftwbak`
only after checking the paths against that box. The CLI requires a backup from
the last 24 hours whose size and SHA-256 match Core's verified archive. It
checks that the same archive still exists under the service's data bind on the
box, so it does not copy a large archive back to `/tmp`. It stages the exact
release beside the old binary, then changes only the systemd
start override. It compares version, health, driver names and working driver
count. On failure it tries the old start command. If old Core cannot read the
data after a failed trial, it restores the verified archive before retrying
old Core. Keep the printed recovery-copy path if automatic recovery fails.
The CLI prints the active phase, elapsed time and completed/total bytes when
known; it says when a total is unknown. If LAN auth is on, set `FTW_API_TOKEN`
in the CLI process environment. Never paste that token into a command line.

This pilot accepts an older **direct native systemd** site only. Docker and
Home Assistant installations remain on their current line while their own
layout and recovery path are tested. A successful empty-data smoke test does
not prove migration of a live household.

GitHub `releases/latest` and the old Docker `:latest` aliases remain on the
2.x line for installed boxes. Native beta and stable releases use exact tags
without moving that global latest slot. An urgent safety repair may still
need an owner-approved 2.x release; it does not restart routine Docker
releases or provide a hop to 3.x. The old `beta.yml` and `release.yml`
workflows are guarded to that line. A release from either line does not deploy
itself to a box.

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

On a native site, Update Center can download and verify a 0.x package, create
a mandatory local settings/config rollback point, stage the new slot and
restart through the launcher. A trial only becomes current after readiness;
a failed trial falls back to the previous Core. A local rollback point does
not include history and stays on the same disk. A history-format change needs
a full backup made before the update. See
[full backup and restore](backup-and-restore.md).

The same operator CLI can show the native path from a terminal:

```bash
python3 scripts/ftwctl.py --url http://127.0.0.1:18080 update --channel beta
```

It asks Core to update through its normal API and follows the local rollback
point, download, restart and health result. If Core says a full backup is
required, pass `--backup-dir ~/FTW-backups` so the CLI first creates, verifies
and downloads one to this computer. The `update` command refuses old 1.x,
2.x and 3.x installs before changing their channel.

Core and the compiled Energyplan worker ship in one package. Core validates
plans and keeps its Go fallback. Signed Lua drivers follow their own beta and
stable channel and change one driver at a time; see
[device repository](device-repository.md). There is no optimizer sidecar in
the native install.

## Host and old release details

FTW's Core update does not update the host operating system, kernel or Docker
engine. The operator handles host updates; the old Raspberry Pi image has its
own host update setup, described in [the image guide](rpi-image.md).

The old Docker workflows and their release receipts remain for an exceptional
2.x repair. The native release workflow cannot publish Docker images or move
old aliases. New Core code refuses cross-line update requests, but that guard
cannot change an older installed binary. Do not use an old tag or script to
bypass the guided migration.

Release notes keep the old state-schema markers for the remaining Docker
line. The native package carries its own state-schema receipt; the launcher
checks it before staging and refuses automatic rollback across a schema
change. Source and tests in `go/internal/nativeupdate` define that behavior.
