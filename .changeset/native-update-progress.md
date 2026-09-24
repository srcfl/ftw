---
"ftw": minor
---

A native update no longer saves a local rollback point first, which took minutes on a Raspberry Pi with a large settings database; `ftw rollback` keeps the current data. `ftw update`, `ftw rollback` and `ftw backup` now show each step on a terminal with a bar, size, rate and time left, and close it with its duration; in a log they write one line per step. Core records every finished step, so a fast download still appears. Core refuses a download when the disk lacks room for the next release, and `ftw status` shows free space for releases and backups and any old rollback points that can be deleted. A new Core that migrates history while starting shows that migration's progress.
