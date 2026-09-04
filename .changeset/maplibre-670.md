---
"ftw": patch
---

The vendored MapLibre GL JS is now 6.7.0 (was 6.0.0). Same four-file
code-split layout, and the per-request `referrerPolicy` the OSM tiles rely on
still works; the weather map, the building picker and the PV array drawing
tool were re-verified in a browser on the new build.
