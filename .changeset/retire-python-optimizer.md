---
"ftw": minor
---

Remove the Python optimizer service, its release channel, update controls and runtime settings. Energyplan ships with Core and keeps Core DP as its validated fallback and comparison shadow. Older Python engine settings migrate to Energyplan. Core updates and fresh installations no longer need a Python sidecar; the updater can retire its old Compose wiring after Energyplan is healthy.
