---
"ftw": patch
---

Read hourly history summaries before taking SQLite's write lock. Bound each read and write so a slow backfill cannot monopolize live telemetry commits; incomplete backfills keep using raw history.
