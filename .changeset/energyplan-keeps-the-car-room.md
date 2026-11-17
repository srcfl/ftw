---
"ftw": patch
---

The bundled Energyplan worker now keeps a battery that may not cover the car
out of the car's import room, as Core requires. Until now Core rejected such a
plan: a site with one battery and one car got Core's own plan, and a site with
several batteries or cars got the worker's plan with a warning. The worker's
plan now passes without one.
