---
"ftw": patch
---

A slow planner solve now finishes after a restart. The missing-plan retry no longer starts a new solve every 5 s while one is still running. Each new request cancelled the one before it, so on a slow box no plan landed.
