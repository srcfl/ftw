---
"forty-two-watts": minor
---

Database hardening and richer history access.

**Durability & SD-card resilience**

- Parquet rolloff now fsyncs day files (and their directory) before the atomic rename — a power cut can no longer lose a day of rolled-off samples whose SQLite rows were already deleted.
- The rolloff streams one UTC day at a time instead of buffering the whole backlog in memory (a multi-week backlog could OOM a Pi and then never complete).
- Planner-diagnostics day files are merged instead of overwritten — the hourly rolloff previously discarded every earlier hour of the same day from cold storage.
- One-time `VACUUM` at boot when a large share of state.db is freelist (reclaims the high-water mark left by big prunes), guarded by a free-disk check.
- Truncating WAL checkpoint after every hourly rolloff; each control-loop tick (history + metrics) is now persisted in a single transaction, halving the WAL commit rate.

**Faster history queries**

- Downsampled `/api/history` and `/api/series` queries aggregate per bucket in SQL instead of fetching every raw row into Go (a month view used to materialize >1M rows per request), and buckets carry a min/max envelope so short power spikes stay visible zoomed out.

**Richer history over REST**

- `/api/series` windows older than 14 days transparently include the cold Parquet tier.
- `/api/series` supports comma-separated multi-metric queries, absolute `since`/`until` bounds, and `format=csv` export.
- `/api/series/catalog` reports each metric's display unit; units from `host.emit_metric` are now persisted across restarts.

**Growth control**

- New `state.cold_retention_days` config bounds the cold Parquet tier (default 0 = keep everything).
- Low-disk warning (log + event feed) when free space drops below 500 MB.
