---
"ftw": patch
---

Release retained DuckDB buffers after a failed telemetry commit runs out of memory, then retry the same queued tick. This lets live collection recover before the normal maintenance threshold and lets background history import resume.

Increase the primary DuckDB memory budget to 256 MB so a full telemetry tick can reconcile the observed startup gap while history queries run.
