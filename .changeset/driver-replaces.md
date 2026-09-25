---
"ftw": patch
---

A device that runs the retired ESPHome DSMR driver entry `esphome-dsmr` moves to
the release's `esphome_dsmr` at start, and its settings are saved; the two were
the same driver under two names. An operator's own driver file of the old name is
left alone. The bundled drivers come from srcfl/device-drivers 92adaf0.
