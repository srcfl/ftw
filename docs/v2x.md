# V2X Manual Pilot

V2X support is currently a manual pilot feature. The system can ingest V2X
telemetry, show it in status surfaces, and send operator-issued setpoints to a
configured charger. Automatic planner dispatch is intentionally not enabled
until the V2X policy envelope has been verified on hardware and wired into the
planner.

> **Read before you command hardware.** This is a manual/test surface with
> real safety caveats:
>
> - **`POST /api/v2x/command` bypasses the V2X policy envelope.** The manual
>   command surface does **not** honor the reserve floor, departure target, or
>   export / grid-charging limits described under [Policy Readback](#policy-readback).
>   It only applies the `+/-50 kW` API cap and the per-driver charger clamps. The
>   policy envelope is observability-only right now; manual commands are not
>   filtered through it.
> - **The API is unauthenticated**, like the rest of the local API. Anyone on
>   the LAN can command the EV to charge or discharge. Do not expose this
>   surface beyond a trusted network.
> - **The Ferroamp DC2 and Ambibox drivers are EXPERIMENTAL.** Their power sign
>   convention has not yet been verified against real hardware. Verify charge /
>   discharge direction on a real unit (see the
>   [Hardware Verification Runbook](#hardware-verification-runbook)) before
>   relying on either driver.

## Sign Convention

V2X uses the same site convention as batteries:

- `+W` means the vehicle is charging and acts as a load.
- `-W` means the vehicle is discharging into the site or grid.

The driver layer is the only place protocol-specific sign conversion should
happen. Everything above `host.emit("v2x_charger", ...)` uses the site
convention.

## Supported Pilot Drivers

The bundled pilot drivers are:

- `drivers/ferroamp_dc2_v2x.lua` for Ferroamp DC2 V2X over local MQTT.
- `drivers/ambibox_v2x.lua` for Ambibox V2X over MQTT.

Both drivers are marked experimental until their sign, limits, and command
semantics have been verified on live hardware.

## Configuration

Example configuration:

```yaml
drivers:
  - name: ferroamp_dc2
    lua: drivers/ferroamp_dc2_v2x.lua
    capabilities:
      mqtt:
        host: 192.168.1.70
        port: 1883
        username: dc2
        password: dc2mqtt!

  - name: ambibox
    lua: drivers/ambibox_v2x.lua
    capabilities:
      mqtt:
        host: sid-os.local
        port: 1883

v2x:
  enabled: false
  driver_name: ferroamp_dc2
  vehicle_capacity_wh: 77000
  min_reserve_soc_pct: 35
  departure_target_soc_pct: 80
  departure_time: "07:30"
  max_charge_w: 7000
  max_discharge_w: 5000
  export_allowed: false
  grid_charging_allowed: false
  cycle_cost_ore_kwh: 12
```

Configure one V2X driver for the first hardware run. If multiple V2X drivers
are configured, API calls must include the `driver` field.

The `v2x:` block is a policy readback surface for future automation. It is
default-off and does not make the planner send V2X commands. When
`enabled: true`, the policy endpoint can calculate the live safe power envelope
from reserve SoC, departure target, charger limits, grid import/export state,
and connected/SoC telemetry.

## Manual Commands

Send a signed setpoint with `POST /api/v2x/command`:

```bash
curl -X POST http://localhost:8080/api/v2x/command \
  -H 'content-type: application/json' \
  -d '{"driver":"ferroamp_dc2","power_w":3000}'
```

That asks the charger to charge the vehicle at `+3000 W`.

Discharge is negative:

```bash
curl -X POST http://localhost:8080/api/v2x/command \
  -H 'content-type: application/json' \
  -d '{"driver":"ferroamp_dc2","power_w":-3000}'
```

Stop:

```bash
curl -X POST http://localhost:8080/api/v2x/command \
  -H 'content-type: application/json' \
  -d '{"driver":"ferroamp_dc2","action":"v2x_stop"}'
```

The API rejects non-zero commands when the selected V2X driver is not reporting
live telemetry. Manual commands are capped to `+/-50 kW` at the API boundary,
and each Lua driver further clamps to its reported or configured charger
limits.

### Ferroamp DC2 MQTT details

The Ferroamp DC2 charger API does not take watts on its power control topic.
The driver accepts site-convention watts from FTW and translates
them at the driver boundary to the DC2 API:

- `dc2/ui/control/controller` gets JSON `{"timestamp": ..., "value": "MQTT"}`.
- `dc2/ui/control/power` gets JSON `{"timestamp": ..., "value": <percent>}`.

The percent value is computed from the requested watts, live DC voltage, the
charger 50 A current ceiling, and the 20 kW power ceiling. At 400 V or higher,
`-5000 W` is about `-25`; at 300 V it is about `-33.3` because 100% is only
15 kW. DC2 may still deliver much less than the requested percent if vehicle
limits, user limits, or DC-link droop/voltage limits are active. In Bjorn's
March test analysis, the small fixed charge/discharge power was caused by the
DC network voltage hitting its outer limits even though the MQTT commands
arrived on the correct topics.

## Policy Readback

`GET /api/v2x/policy` returns the configured policy and one live envelope per
V2X driver:

```json
{
  "policy": {
    "enabled": true,
    "driver_name": "ferroamp_dc2",
    "min_reserve_soc_pct": 35,
    "max_charge_w": 7000,
    "max_discharge_w": 5000,
    "export_allowed": false,
    "grid_charging_allowed": false
  },
  "drivers": {
    "ferroamp_dc2": {
      "enabled": true,
      "min_power_w": -1200,
      "max_power_w": 0,
      "max_discharge_w": 1200,
      "reasons": ["export_limited_to_import", "grid_charging_blocked"]
    }
  }
}
```

`min_power_w` is negative discharge. `max_power_w` is positive charge. Missing
SoC, unknown connection state, offline drivers, reserve floor, or stale grid
context collapse the relevant side of the envelope to `0 W`. `/api/status`
also includes the same object as `v2x_policy`.

## Dashboard

When a V2X driver reports telemetry, its driver card shows:

- charger state and online status;
- signed V2X power;
- vehicle SoC;
- DC voltage/current/power;
- session charge/discharge energy;
- charger limits and control mode;
- manual Charge, Discharge, and Stop controls.

Discharge commands require confirmation in the browser. Non-zero controls are
disabled when the driver is not live; Stop remains available.

## Hardware Verification Runbook

Use this sequence before enabling any broader rollout:

1. Configure one V2X driver and restart or reload the service.
2. Confirm `/api/status` has a driver entry with `v2x_w`.
3. Confirm the dashboard V2X card shows the driver as online.
4. Send `0 W` and verify the charger target is zero.
5. Send a small charge setpoint such as `+1000 W`.
6. Verify charger telemetry, site-meter direction, and MQTT payloads agree.
7. Send a small discharge setpoint such as `-1000 W`.
8. Verify site import drops or export increases as expected.
9. Send `v2x_stop`.
10. Disconnect or stop telemetry and verify non-zero commands are rejected.

Record charger model, firmware, MQTT topics, sign observations, and observed
limits before promoting a driver beyond experimental.

## Current Automation Boundary

The MPC planner and dispatch loop are V2X-aware for house-load accounting and
stationary-battery safety, but they do not command V2X automatically. A V2X
policy envelope exists for observability and future planner input. Automatic
V2X dispatch still requires:

- hardware-verified sign and command semantics;
- planner input/output fields for V2X as a separate dispatchable asset;
- fuse/stale-meter guards over combined stationary battery and V2X behavior;
- explicit opt-in that consumes the policy envelope;
- diagnostics showing why V2X is idle or constrained.

Until that integration exists, V2X commands must be operator initiated.
