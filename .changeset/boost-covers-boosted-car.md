---
"ftw": patch
---

A battery boost now makes the home battery cover the car. In the planner
modes a boost was only a permission: a plan that kept the battery for later,
or charged it, left the boosted car on the grid. While the charger reports
the car drawing and a battery is above the boost's reserve, the battery now
discharges to cover the house and that car, down to zero import, and never
exports for car power the meter no longer shows. A planned battery charge
from the grid waits until the boost ends.
