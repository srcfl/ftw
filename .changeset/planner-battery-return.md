---
"ftw": patch
---

The Core planner now counts the energy tax a Swedish household gets back when the battery buys from the grid and later sells to it. In active arbitrage it credits stored grid energy it sends back, counted as History counts it: solar first, a proportional mix of grid and other energy, no refund for losses. It uses the lowest tax any Swedish household pays (26.4 öre/kWh in 2026, after the northern deduction), never more than the grid tariff, and only for a Swedish zone priced in SEK with one battery of known size. It leaves the refund out while a car is plugged in and in a plan that crosses New Year. Every plan, whichever engine made it, shows the refund it earns in `battery_return_ore` and each slot's `battery_return_wh`. Energyplan does not plan for the refund yet.
