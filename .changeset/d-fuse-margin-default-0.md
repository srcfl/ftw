---
"ftw": patch
---

`fuse.safety_margin_a` now defaults to 0. On a site with no saved margin, FTW works to the fuse rating: the car, the home battery's charge and fuse relief, and the site's total import and export limits each move up 0.5 A per phase, to the rating. A margin is opt-in, for an inverter whose own phase limiter trips at the rating. FTW keeps any margin you have saved. If yours is exactly 0.5 A, FTW's old default, the log says so once at startup; set 0 to drop it.
