---
"ftw": patch
---

A surplus-only EV can take leftover PV while the home battery buys from the grid. Surplus-only is an EV policy, not a site-wide import ban: the car still cannot import, and the home battery still cannot feed the car.

Ship a new optimizer image with this core. HiGHS still plans under the leftover constraint; an old optimizer image will keep idling the car on cheap sun and never produce the combo ValidatePlan now accepts.
