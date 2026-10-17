---
"ftw": patch
---

A failed full backup now says why. Core writes when a backup starts, ends and fails to the journal, with the phase, table and row count it reached. A backup stopped by the two-hour limit for a live copy names that limit instead of only "context deadline exceeded". GET /api/backups keeps the reason, table and row count until the next backup starts, and `ftw backup` shows the last failure before it begins. When the raw copy does not fit in /dev/shm, Core warns that it is copying on the data disk and reports how much space earlier backups' scratch files still take there.
