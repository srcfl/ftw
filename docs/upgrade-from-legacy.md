# Older Docker installations

The Docker-to-Docker migration from Forty Two Watts to Sourceful FTW is
retired. `scripts/migrate-legacy-compose.sh` now exits before reading or
changing a site. Do not run an older copy of that script to move an existing
box to another Docker release line.

Keep a 1.x, 2.x or 3.x site on its current version. The planned guided
migration will take any of these sites directly to native 0.x after it has
been tested. The fresh native installer is for an empty host; it refuses an
existing site and does not preserve its data. To try 0.x beside an older
site now, see [Coming from an older FTW](native-beta.md#coming-from-an-older-ftw).

Calendar support is gone from 0.x. Move future calendar charging events to
loadpoint targets and ready-by schedules first. A `caldav` section in an old
config is ignored with a warning, and the old calendar tables stay unused in
`state.db`.

Before any manual recovery, make and verify a full backup, then copy it off
the box. See [backup and restore](backup-and-restore.md) and the current
[update policy](self-update.md).
