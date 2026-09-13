---
"ftw": patch
---

Retry a failed charger resume while the same plug session still needs power. Count measured EV energy across power changes so a completed slot budget stays stopped, and keep snapped current below the fuse allocation.
