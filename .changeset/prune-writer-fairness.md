---
"ftw": patch
---

Yield between maintenance chunks so live writers are never starved.

v0.130.2 bounded every maintenance transaction, but back-to-back chunks
still starved the control loop during a large backlog migration: SQLite's
busy handler retries without fairness, so the maintenance loop re-acquired
the write lock before any waiting writer won its retry. Prune chunks,
rolloff deletes, and diagnostics batch-deletes now pause (250/100 ms)
between transactions, guaranteeing every waiting writer a window.
