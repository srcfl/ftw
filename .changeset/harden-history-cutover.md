---
"ftw": patch
---

Multi-day history charts use the hourly DuckDB rollup even when buckets are finer than one hour, so a 30-day series no longer scans raw samples. Shutdown flushes queued ticks. Forecast archive accepts a 256 KiB compressed issue so a 48-hour villa forecast can be stored.
