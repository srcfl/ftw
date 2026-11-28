---
"ftw": patch
---

"Use the plan" on the Plan tab now turns the planner on when it is off. It saves the same setting as Settings → Planner → Enabled, restarts FTW so the planner starts, hands the house to the plan and shows the plan when the box has one, about 8 s later on a test box. If prices or a battery are missing, the card says so where the button is.

A new box with the planner on in its config, and no stored control mode, starts in the planner mode its battery-export answer maps to, so the plan drives from the first start. A stored mode is kept. The first plan no longer waits up to 15 minutes when the startup replan ran before the first battery reading or prices: FTW replans once as soon as they are in, about 5 s after start on a test box.

The planning style, battery export and Manual… wait for the first plan. A home without a battery gets none of them and no longer reads about a battery. While FTW still learns the home, the plan's forecast says so, and a plan without the extra forecast margin is not shown as a warning then. A box that never sold from the battery no longer asks whether selling may continue: a new box stores "not allowed", and turning the planner on from off records that answer for a box that stored "not checked". A box that sold through active arbitrage before its upgrade is still asked.
