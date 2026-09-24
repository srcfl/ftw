---
"ftw": patch
---

`ftw backup` no longer reports "EOF" when a backup takes more than two minutes.
Backup creation, verification and download now outlast the server's write
timeout; before, the archive was made but the reply was cut off.
