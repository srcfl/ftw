# Older paired Docker upgrades

The operator-led 2.x-to-3.x Core and updater procedure is retired.
`scripts/upgrade-paired-release.sh` now exits before reading or changing a
site. Do not use an older copy of the script or the orange Update button to
move a 1.x or 2.x box to 3.x.

Keep an existing site on its current version. The planned guided migration
will take 1.x, 2.x and 3.x sites directly to native 0.x after it has been
tested. The fresh native installer is for an empty host and cannot migrate
an existing box.

Before any manual recovery, make and verify a full backup, then copy it off
the box. See [backup and restore](backup-and-restore.md) and the current
[update policy](self-update.md).
