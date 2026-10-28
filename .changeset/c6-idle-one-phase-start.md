---
"ftw": patch
---

Start Charge now on one phase once there is room for 6 A on that phase. An idle charger set to `1p` or `auto` on a three-phase site waited for room for the three-phase minimum (4,140 W) on a single phase, so it never started on a 16 A fuse and rarely on 25 A. It now waits for room for its own least order, 1,380 W.
