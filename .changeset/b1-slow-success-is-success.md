---
"ftw": patch
---

A device command that succeeds after the control tick stops waiting now counts as a success. Before, a driver that took longer than half the control interval to answer (1 s by default) was put back in its own default mode after every accepted command, so a slow battery or cloud charger switched between forced and autonomous mode on every tick. A started command now gets at least 5 s to finish; it still falls back to the default mode if it fails, runs out of that time or is cancelled.
