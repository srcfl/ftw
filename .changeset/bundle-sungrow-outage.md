---
"ftw": patch
---

Bundle sungrow 1.6.1. After the inverter drops off the network and comes back, the driver writes the release instead of reporting it held, so a forced charge or discharge no longer keeps running. It also keeps reading the battery's direction and PV, instead of dropping them for the session.
