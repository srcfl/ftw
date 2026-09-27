---
"ftw": patch
---

Native installs update and roll back as before with `ftw update`, `ftw rollback` and the API, and Docker 0.x still updates by changing `FTW_VERSION`; the code for the old Docker update sidecar is gone from Core. A native update now refuses a release that changes stored data before it downloads anything, and a download that stops receiving data fails after a minute instead of being reported failed while it keeps running. The version check reports `install_ready` instead of `sidecar_ready`, `FTW_SELFUPDATE_ENABLED` no longer has any effect, and the unused `POST /api/version/snapshots` and `POST /api/version/rollback` endpoints are removed; `ftw status` still lists rollback points an older Core left. Release packages no longer carry the `forty-two-watts` alias or the direct-layout `deploy/ftw.service` unit.
