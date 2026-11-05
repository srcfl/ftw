---
"ftw": patch
---

A battery that may not sell to the grid exports far less while it covers the
house. In planner_self, planner_cheap and passive arbitrage, a discharging
battery now holds the grid a dead band above 0 W instead of around it, so a
drop in house load no longer sells battery energy about half the time. The
site imports a little more instead; that energy stays in the battery.
