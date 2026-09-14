---
"ftw": patch
---

Build hourly history in small batches that follow the primary index. Save progress with each committed batch, resume after timeouts and restarts, and expose unfinished work separately from raw-history migration. Keep live writes and late samples ahead of background work.
