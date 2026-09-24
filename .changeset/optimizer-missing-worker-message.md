---
"ftw": patch
---

When the optimizer worker is missing, FTW now says the release package's
Energyplan worker is missing and that the built-in Go planner is used. It no
longer tells operators to run an ftw-optimizer sidecar that is gone.
