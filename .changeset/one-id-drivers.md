---
"ftw": patch
---

Every bundled driver now declares the same id and version as the signed driver
channel, for example `sungrow` instead of `sungrow-shx` and `easee_cloud` instead
of `easee-cloud`. Choosing a version under Settings → Devices now works for these
drivers, "Use bundled" finds the release's copy, and installing a channel version
reaches the running device without a restart. The driver inventory sent to
Sourceful reports these ids. The bundled ESPHome DSMR driver is its real source
again; its published file used to report a wrong id.
