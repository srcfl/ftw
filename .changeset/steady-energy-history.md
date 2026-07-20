---
"ftw": minor
---

Add a versioned energy history ledger keyed by stable asset identity, with separate grid and battery directions, hardware-counter preference, marked integration fallback, data-quality provenance, bounded system/asset APIs, CSV export, and a read-only History view.

Existing hot/warm/cold history and Parquet roll-off remain unchanged. XLSX export is deferred to a later phase to avoid adding a heavyweight runtime dependency.

Five-minute ledger detail is retained for 30 days, then atomically rolled into honest hourly buckets and bounded to the two-year API horizon.
