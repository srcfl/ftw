---
"ftw": patch
---

Critical fix: bounded maintenance transactions (upgrade-lock incident).

The first history prune after upgrading a long-running install (v0.129.0's
Prune wiring) could hold SQLite's write lock for hours: the hot→warm
aggregation used a correlated subquery that re-scanned the hot table once
per 15-minute bucket, and the whole backlog ran as one transaction. Every
control-loop write failed with SQLITE_BUSY until it finished.

- Prune is now linear (single-pass bucket aggregation) and chunked: each
  transaction ages at most ~24 h of rows, so writers interleave. A 93-day
  backlog that previously locked the DB for 4+ hours now completes in
  seconds. Chunk and cutoff boundaries are bucket-aligned, which also fixes
  a pre-existing partial-bucket overwrite at the retention edge.
- Prune logs its result (rows aged, chunks, duration) — maintenance is no
  longer silent.
- Parquet rolloff deletes each day's rows right after that day's file is
  durable, in hour-sized transactions, instead of one giant end-of-run
  DELETE that could lose the race against live writers.
- Planner-diagnostics retention in SQLite reduced 30 → 7 days (measured
  485 MB at 30 days on a real site; Parquet keeps everything), deletes are
  batched, and the table is excluded from state snapshots (snapshots shrink
  from ~470 MB to ~20 MB).
