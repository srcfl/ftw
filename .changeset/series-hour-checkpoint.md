---
"ftw": patch
---

Long-range history charts use an hourly DuckDB rollup so a year of samples returns within the request budget. Live ticks add to that rollup instead of rebuilding the hour, commit in 15 s batches to cut SD fsyncs, and reopen DuckDB only when RSS is high or it ran out of memory. Hourly work otherwise just checkpoints. `state.cold_retention_days` is read live so a cap can take effect without a restart.
