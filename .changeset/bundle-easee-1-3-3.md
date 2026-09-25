---
"ftw": patch
---

The bundled Easee driver (1.3.3) no longer reports an old change time for a
steady charging power, so FTW sees the reading as current. The bundled drivers
move to srcfl/device-drivers 489c937, which also brings nibe_local 1.2.0 with
its optional, off-by-default solar surplus feed.
