# Older Docker installations

The Docker-to-Docker migration from Forty Two Watts to Sourceful FTW is
retired. `scripts/migrate-legacy-compose.sh` now exits before reading or
changing a site. Do not run an older copy of that script to move an existing
box to another Docker release line.

Keep a 1.x, 2.x or 3.x site on its current version. The planned guided
migration will take any of these sites directly to native 0.x after it has
been tested. The fresh native installer is for an empty host; it refuses an
existing site and does not preserve its data.

Before any manual recovery, make and verify a full backup, then copy it off
the box. See [backup and restore](backup-and-restore.md) and the current
[update policy](self-update.md).
