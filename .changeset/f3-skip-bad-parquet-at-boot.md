---
"ftw": patch
---

Start FTW when one old history file cannot be read. FTW renames the file with an `.unreadable` suffix and keeps it, imports the other days, keeps that day's older copy for history views, and lists the file under `unreadable_cold_files` in the history health report.
