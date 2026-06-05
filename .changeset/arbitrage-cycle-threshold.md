---
"forty-two-watts": minor
---

Arbitrage cycle threshold: a new planner knob, **Min arbitrage spread
(öre/kWh)** (`planner.min_arbitrage_spread_ore_kwh`), stops the battery
cycling for marginal gains. The planner won't cycle for grid arbitrage
unless the price gain beats this many öre/kWh on top of round-trip losses.
It applies only to the arbitrage modes (`planner_arbitrage` /
`planner_passive_arbitrage`) — self-consumption is never affected — and
biases the planner's decision only, so the savings statistics stay on real
spot economics. Default 0 = off. Configurable from the Planner settings tab.
