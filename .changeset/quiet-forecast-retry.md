---
"ftw": patch
---

Retry frozen forecast archive writes after a timeout or temporary database lock, and prepare compressed model snapshots before taking the SQLite write lock.
