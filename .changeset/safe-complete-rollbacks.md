---
"ftw": patch
---

Make update rollback backups complete and compressed, preserve the FTW file owner during restore, clear stale SQLite WAL files, health-check the restored service, and automatically recover the pre-rollback state if the selected backup cannot boot. Incomplete legacy snapshots are no longer restorable because they omitted history.

Publish Raspberry Pi Imager metadata on stable version releases as well as the permanent installer channel, and harden the legacy Compose migration so it preserves custom project identities and updates or restores an existing optimizer container.
