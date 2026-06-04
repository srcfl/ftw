---
"forty-two-watts": patch
---

ui: dashboard banner when the database auto-recovered from corruption

The dashboard now reads the `storage` field from `GET /api/health` (added in
the two-tier storage work) and shows a dismissible amber banner when
`state.db` or `cache.db` was found corrupt and healed at boot — e.g. "cache.db
was corrupt — rebuilt empty, re-fetching" or "state.db … restored from last
snapshot". Closes the loop so DB corruption is visible at a glance instead of
only in the logs.
