---
"ftw": patch
---

FTW no longer cuts a charging car when every phase has room. In 0.155.0 a car charging on one phase under auto was cut from 3.5 kW to 1.6 kW and then to 0 W, again and again, while the busiest phase read about 16 A on a 20 A fuse: dividing the fuse room between cars read the order as three-phase, gave the car's own current no credit, and cut it to what was left on that phase. Keeping a car's current order is now never a cut there; only a measured overage on the site meter cuts. An order that would move the same watts onto fewer phases is still held to the room on those phases.
