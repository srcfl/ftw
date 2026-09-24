---
"ftw": patch
---

Planner diagnostics are kept for seven days again. Since history moved to plain
buckets nothing pruned them, so state.db grew by about 30 MB a day and every
backup copied the growth. Maintenance now deletes older snapshots in small
batches.
