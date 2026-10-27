---
"ftw": patch
---

Bundle sungrow 1.6.1. After the inverter drops off the network and comes back, the driver writes the release instead of reporting it held. A forced charge or discharge left over from before the outage now usually ends within about a minute of the return. Before, it ran for three to eight minutes, or did not end. The driver also keeps reading the battery's direction and PV, instead of dropping them for the session.
