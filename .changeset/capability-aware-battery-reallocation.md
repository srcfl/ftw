---
"forty-two-watts": minor
---

Capability-aware battery reallocation: when one battery can't move in the
demanded direction this cycle (e.g. a Ferroamp ESO floored at its discharge
SoC limit), the dispatcher now hands its share to a capable sibling instead
of leaking it to the grid. Drivers signal this with two optional battery-emit
fields, `discharge_capable` / `charge_capable`; absent → assumed capable, so
every existing driver is unaffected. The Ferroamp driver reports both from its
per-ESO floor/ceiling counts. Symmetric for charge (a full battery is excluded
from the charge split).
