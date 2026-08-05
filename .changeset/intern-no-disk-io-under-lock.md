---
"ftw": patch
---

Recording a metric name for the first time no longer stalls the API. The time-series intern cache held its exclusive lock across the allocating SQLite INSERT, so on a slow SD card every reader of the metric catalog, metric and driver lists, or any chart series queued behind a disk write issued from inside the control tick — the same shape as the 2026-07-16 prune incident, expressed as a lock instead of a channel. Allocation now writes with no cache lock held and takes the lock only to publish the resulting id, and the boot-time hydrate scans the two intern tables into local maps before swapping them in. Concurrent allocation of the same name still resolves to one row and one id.
