# Sourceful Zap

FTW reads Sourceful Zap through its local API. The integration remains
operational without Sourceful cloud and is currently telemetry-only.

## Configuration

```yaml
drivers:
  - name: sourceful-zap
    lua: drivers/zap.lua
    is_site_meter: true
    battery_telemetry_only: true
    capabilities:
      http:
        allowed_hosts: ["zap.local"]
    config:
      host: zap.local
```

Use the Zap's LAN IP in both places when mDNS does not cross the network.
`battery_telemetry_only` allows battery display/history without admitting Zap
to the dispatch pool. Do not add `battery_capacity_wh` to this read-only
driver.

When FTW must control an inverter or charger behind Zap, configure that native
driver and suppress duplicate Zap telemetry:

```yaml
    config:
      host: zap.local
      disable_pv: true
      disable_battery: true
      disable_v2x: true
```

With several meters, `meter_serial` pins the site meter; otherwise the first
P1 device is preferred.

## Data and identity

The driver refreshes Zap devices without restarting and emits:

- selected meter power, phases, voltage/current/frequency and energy totals;
- aggregate PV power/energy;
- aggregate battery power/energy and capacity-weighted SoC when possible;
- aggregate V2X power, vehicle SoC, status and diagnostics.

Per-device values remain diagnostic metrics. The FTW device identity is based
on Zap's gateway serial from `/api/crypto`, with a lower-confidence meter
serial fallback for older firmware.

Zap's per-DER `enabled` flag controls its Nova publishing, not whether local
FTW may read the device.

## Safety

The driver converts every value to FTW's site convention and does not invent
zero when a required reading is absent. Silence lets the watchdog and
stale-site-meter guard act. Physically impossible values are rejected only
when reported nameplate data provides a quantified bound.

`driver_default_mode` performs no write because the driver is read-only.
Zap's current local command responses acknowledge queueing rather than verified
hardware execution and do not provide the expiring, observable command lease
required for unattended dispatch. Native drivers remain the control path.

## Verification

```bash
cd go
go test ./internal/drivers -run 'Zap|zap'
```

## Troubleshooting

- not found: confirm Zap is on Wi-Fi and reachable at
  `http://zap.local/api/system` from the FTW host;
- no meter: inspect Zap's `/api/devices` and pin `meter_serial` when needed;
- duplicate PV/battery: disable the overlapping Zap DER;
- visible battery is not controlled: expected for the telemetry-only driver.
