---
"forty-two-watts": patch
---

**Fix: `planner_passive_arbitrage` no longer absorbs live PV surplus into the
battery on a plan-idle slot.** When the DP picked idle for a slot
(`battery_w = 0`) and live conditions turn out to have more PV (or less
load) than the forecast assumed, the dispatcher now holds the battery at
0 and lets the surplus export — rather than collapsing to
self-consumption and chasing `grid = 0` by ramping the charge up.

The DP picks idle slots deliberately, often to preserve export revenue
at the current spot when future PV is plentiful and future prices are
lower. The old behaviour reactively swallowed that surplus because the
fallback path was symmetric with self-consumption ("balance to zero"),
which discards the DP's intent. The gate is the mirror of
`plannerSelfExportSurplusGate`, but triggered on the **live** baseline
grid (`grid_meter − Σ battery_w`) rather than the plan's forecasted
grid — for the slot we're already in, live measurements override the
(possibly-stale) forecast.

Reactive discharge on live import is unchanged: a passive-arbitrage
idle slot with the meter importing still allows the battery to cover
the load. The change is one-sided — block reactive charging when the
meter shows export potential the forecast missed.

Found 2026-05-28 on a sunny May afternoon with a wildly over-estimated
load forecast: planner expected ~2.8 kW load vs. actual ~0.5 kW, picked
idle on net-≈0 forecast, and the dispatcher charged 2.6 kW into the
battery despite high current spot (160 öre), low future spot (95 öre),
and abundant forecast PV in upcoming slots.
