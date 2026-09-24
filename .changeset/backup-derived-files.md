---
"ftw": patch
---

Backups leave out `state.db.snapshot`, its temp file and `cache.db`. The snapshot
is a local copy of state.db whose rows the backup already holds, and a snapshot
being rewritten during a backup made it fail with "no such file or directory".
Archives also get smaller.
