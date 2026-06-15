---
"forty-two-watts": minor
---

Home Assistant integration: LWT availability, per-driver health sensors, peak/EV entities, emit_metric sensors, MPC plan sensor

- **Availability topic (LWT)**: HA entities now show as "Unavailable" when the EMS goes offline. The paho Last Will Testament publishes `offline` to `forty-two-watts/status` on unclean disconnect; a clean shutdown publishes it synchronously before disconnect.
- **Per-driver binary_sensor**: One `<driver>_online` entity per driver so HA dashboards can alert on a stale driver (e.g. stuck Modbus device).
- **`peak_limit_w` and `ev_charging_w`**: Both control setpoints are now published as `number` entities and writable from HA.
- **`emit_metric` sensors**: Any scalar diagnostic emitted by a Lua driver via `host.emit_metric(name, value)` is automatically discovered as a HA sensor with the unit and device_class inferred from the name suffix (`_w` → W/power, `_c` → °C/temperature, `_v` → V/voltage, `_a` → A/current, `_hz` → Hz/frequency, `_soc` → %/battery, `_wh` → Wh/energy).
- **MPC plan sensor**: A `plan_action` text sensor shows the current slot's action (charge / discharge / idle), with JSON attributes containing `battery_w`, `grid_w`, `soc_pct`, slot start/end times, and a full 24 h schedule array — so you can see what the EMS plans to do throughout the day directly in HA.
