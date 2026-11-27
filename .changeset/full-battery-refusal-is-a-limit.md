---
"ftw": patch
---

A full battery that refuses more charge is a normal limit, not a fault. A FoxESS H3 refuses forced charge from 99 %; Core used to count three such refusals as a device that cannot act, drop the battery from dispatch and the plan, return it to its own default and warn that it was not following FTW. Now a refused charge order to a battery within two points of its full level (its own ceiling, or 100 %) is not counted, and the control view says the battery is full. A refused discharge, or a refusal below full, counts as before.
