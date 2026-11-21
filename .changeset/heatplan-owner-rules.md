---
"ftw": patch
---

The heat planner no longer stops and starts for a sliver of price: a change
of value must earn a share of what one slot at full output costs, so a price
that wobbles by a couple of percent gives 11 changes a day instead of 74 at
about 0.4 % higher planned cost. Two rules the owner sets are added to the
planner, both off at zero and with no default: a least saving a day's plans
must reach together before they are acted on, and a short price peak a zone
may sit through at the bottom of its band. Nothing calls them yet; the
planner still only proposes.
