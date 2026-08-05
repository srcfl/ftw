---
"ftw": patch
---

The slew limiter can no longer re-open a charge block another stage closed. Because the limiter anchors each target on the battery's measured output rather than on the previous command, a battery physically charging pulled its own command back up after the tick had already pinned the fleet to 0 W — so a passive-arbitrage idle slot with the meter exporting 2000 W and the battery at +2000 W commanded 1500 W of charging on a tick that forbids charging, and a battery reporting `charge_capable=false` was commanded to charge anyway while a capable sibling absorbed the same share. A charge floor now runs after the limiter, mirroring the discharge-side floor already there: the site-wide charge block (planner_self's export-surplus gate, planner_self's stale plan, the arbitrage-family idle live-export gate) and a driver's own charge-capability report both survive to the hardware.
