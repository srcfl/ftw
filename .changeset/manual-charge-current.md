---
"forty-two-watts": minor
---

**Manual charge-current slider for EV loadpoints (Tesla-style).** You can now set
the charge current (amps) to the car from the dashboard instead of fiddling with
the car or wallbox. The Loadpoints panel gets a per-loadpoint **Charge current**
slider (6 A → the charger's max) with an **Auto** button.

- Setting a current is a manual override that **persists until you pick Auto or
  the car unplugs**, and it overrides surplus-only/schedule (you asked to charge
  now) — but the fuse guard still applies.
- The controller converts amps→watts at the loadpoint's phase count and commands
  that explicit phase mode, so the per-phase current the wallbox sets equals the
  slider. Above the charger's max it clamps.
- New endpoints: `POST/GET/DELETE /api/loadpoints/{id}/charge_current`;
  `/api/loadpoints` now reports `manual_current_a` + `manual_current_max_a`.
