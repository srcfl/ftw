---
"ftw": patch
---

A backup taken right after an update no longer fails on the state snapshot's
journal. Every file of the snapshot family, cache.db and state.db's journal stay
out of the archive.
