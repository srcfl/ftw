---
"ftw": patch
---

Plan only the time left in the current price interval. Core, Energyplan 0.3.1,
EV budgets and plan projections now share the same execution interval. Reject
plans that cross a slot boundary before publication and retry from live state.
Compare measured slot energy with the decisions that applied during each part
of the interval so a late replan cannot rewrite the whole quarter's target.
Support reports use the same observed intervals and distinguish execution-budget
credits from measured battery energy.
