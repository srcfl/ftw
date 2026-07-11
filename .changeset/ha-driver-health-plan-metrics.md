---
"forty-two-watts": minor
---

Expand the Home Assistant MQTT bridge with reliable availability, driver health, live diagnostics, plans, prices, forecasts, phase data, EV/vehicle data, and daily energy totals.

- **Availability topic (LWT)**: HA entities now show as "Unavailable" when the EMS goes offline. The paho Last Will Testament publishes `offline` to `forty-two-watts/status` on unclean disconnect; a clean shutdown publishes it synchronously before disconnect.
- **Per-driver binary_sensor**: One `<driver>_online` entity per driver so HA dashboards can alert on a stale driver (e.g. stuck Modbus device).
- **`peak_limit_w` and `ev_charging_w`**: Both control setpoints are now published as `number` entities and writable from HA.
- **`emit_metric` sensors**: Any scalar diagnostic emitted by a Lua driver is automatically discovered with its explicit unit preserved and a device class inferred when possible.
- **MPC plan sensor**: A `plan_action` text sensor shows the current slot's action (charge / discharge / idle), with JSON attributes containing `battery_w`, `grid_w`, `soc_pct`, slot start/end times, and a full 24 h schedule array — so you can see what the EMS plans to do throughout the day directly in HA.
- **Richer energy data**: Publish current price and forecasts, per-phase meter values, EV and vehicle telemetry, and daily import/export/PV/battery/load energy totals.
