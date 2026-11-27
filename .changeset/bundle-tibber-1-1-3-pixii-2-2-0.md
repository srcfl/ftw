---
"ftw": patch
---

Bundle three driver changes from srcfl/device-drivers:

- Tibber 1.1.3 (srcfl/device-drivers#282) declares its WebSocket host. A box
  set up from the UI now opens Tibber's live stream instead of logging
  `ws: capability not granted`. Core grants it from the driver's `ws_hosts`
  (srcfl/ftw#1822).
- Pixii 2.1.10 (srcfl/device-drivers#228) detects the battery's own
  calibration from Pixii's running-service register. Core suspends control
  and keeps measurements live while it lasts (srcfl/ftw#2017).
- Pixii 2.2.0 (srcfl/device-drivers#281) reports the battery's own charge
  ceiling as `max_soc_fract`. Core plans and charges no higher than it
  (srcfl/ftw#2083).
