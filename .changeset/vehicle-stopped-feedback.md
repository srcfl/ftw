---
"ftw": patch
---

Explain a charger-reported vehicle stop that lasts 90 seconds while FTW offers power, without reporting a no-power fault or assuming full SoC. A brief stop during the session handshake no longer hides a failed start or blocks the resume and wake after FTW's own pause. FTW's own 0 W stop for a car that stopped asking counts as following again.
