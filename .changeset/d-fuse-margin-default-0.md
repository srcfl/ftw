---
"ftw": patch
---

`fuse.safety_margin_a` now defaults to 0, so FTW works to the fuse rating unless you set a margin. A margin is opt-in, for an inverter whose own phase limiter trips at the rating. FTW keeps any margin you have saved. If yours is exactly 0.5 A, FTW's old default, the log says so once at startup; set 0 to work to the rating.
