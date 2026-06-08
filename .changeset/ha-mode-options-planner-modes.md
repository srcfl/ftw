---
"forty-two-watts": patch
---

Fix Home Assistant logging "Invalid option for select.forty_two_watts_mode"
for the planner modes. The MQTT discovery for the Mode `select` only
advertised six modes, but the bridge publishes the live mode as state — and
the default UI choices (`planner_passive_arbitrage` / `planner_arbitrage`)
weren't in the advertised list, so HA rejected them every cycle. The discovery
options and the API mode validator now both derive from a single
`control.AllModes` source of truth, so all ten modes are advertised and the
two lists can't drift again.

Also fixes the matching command path: selecting a planner mode from the Home
Assistant dropdown previously returned "unknown mode" because the HA `SetMode`
callback (and the boot-time mode restore) carried their own hand-maintained
mode lists that omitted the planner modes. Both now validate through
`control.IsValidMode`, and the HA setter mirrors the full `/api/mode`
side-effects (battery manual-hold clear, PI reset, MPC strategy propagation)
via a shared `control.PlannerMPCMode` mapping — so a planner mode picked in HA
behaves identically to one picked in the web UI.
