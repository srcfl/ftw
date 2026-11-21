---
"ftw": patch
---

Keep a charge under the charger's load balancing at Limited when the driver
briefly stops reporting its limit (srcfl/ftw#2056).

An Easee charger read back FTW's 13 A order while its load balancing
signalled 8 A to the car, which took 5.44 kW. The EV status said "Limited by
the charger's load balancing", then "Not following" for 15 s, then Limited
again, though the 8 A pilot reading was still fresh. The test driver, built
on the open pilot change in srcfl/device-drivers#200, stops reporting its
limit for up to 30 s after its order reading moves. Core now lets a fresh
pilot below the order explain a draw that matches it. The status stays
Limited and reads "Limited by the charger" with the pilot current, because
only the driver can name the load balancing. A car that takes far less than
the pilot is still Not following.
