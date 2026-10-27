---
"ftw": patch
---

Treat a site meter that repeats one reading as stale. When FTW moves its own battery or charger by more than 500 W, the device's own reading follows, and the meter's total and phase values still stay the same for 15 s and 3 polls after that, FTW stops dispatch, returns devices to their own control and raises no EV, as for a silent meter. A quiet site is never marked stale, nor is a meter that sends a new reading only every 10 s, and the first changed reading reopens dispatch. Before, such a meter counted as fresh because each poll brought a new timestamp, and in a rig run the battery swung from +5 kW to -5 kW on 40 s of repeated data.
