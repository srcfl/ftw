---
"ftw": patch
---

Save Zap PV and battery reads as off when the boxes are unchecked. Before, setup and Settings left `read_pv` out, so Core waited for PV readings the Zap never sends. Open Settings → Devices and save once to fix a Zap added before this release.
