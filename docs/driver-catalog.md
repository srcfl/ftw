# Driver catalog

forty-two-watts' driver library at a glance. Each driver is a Lua file
in `drivers/` that follows the v2.1 host API (see
[`docs/writing-a-driver.md`](writing-a-driver.md)).

## At a glance

22 drivers covering 17 manufacturers across 3 protocols (Modbus TCP,
MQTT, HTTP/REST). Read-only: 13. With control: 9.

Protocols in use: Modbus TCP, MQTT, HTTP/REST.

## Drivers

Alphabetized by manufacturer. "Control" marks drivers that translate
EMS dispatch commands (charge / discharge / curtail / self-consumption)
back to the device; read-only drivers only emit telemetry.

| Driver | Manufacturer | Protocol | Capabilities | Control | Tested models | File |
|---|---|---|---|---|---|---|
| CTEK Chargestorm (API v1) | CTEK | Modbus | ev | yes | Chargestorm Connected 2/3 (CSOS ≥ 4.9.3) | `drivers/ctek.lua` |
| CTEK Chargestorm (API v2) | CTEK | Modbus | ev | yes | Chargestorm Connected 2/3 (CSOS ≥ 4.9.3) | `drivers/ctek_v2.lua` |
| Deye hybrid inverter | Deye | Modbus | battery, meter, pv | yes | SUN-5K-SG03LP1-EU, SUN-8K-SG04LP3-EU, SUN-12K-SG04LP3-EU | `drivers/deye.lua` |
| Easee Cloud | Easee | HTTP | ev | yes | Home, Charge | `drivers/easee_cloud.lua` |
| Eastron SDM630 / SDM72D-M | Eastron | Modbus | meter | no | SDM630-Modbus, SDM72D-M | `drivers/sdm630.lua` |
| Ferroamp EnergyHub (MQTT) | Ferroamp | MQTT | battery, meter, pv | yes | EnergyHub XL, EnergyHub Wall | `drivers/ferroamp.lua` |
| Ferroamp EnergyHub (Modbus) | Ferroamp | Modbus | battery, meter, pv | yes | EnergyHub XL, EnergyHub Wall | `drivers/ferroamp_modbus.lua` |
| Fronius Symo / Primo GEN24 | Fronius | Modbus SunSpec | battery, meter, pv | no | Symo GEN24 Plus 10.0, Primo GEN24 Plus 6.0 | `drivers/fronius.lua` |
| Fronius Smart Meter | Fronius | Modbus | meter | no | Smart Meter 63A-3, Smart Meter TS 65A-3 | `drivers/fronius_smart_meter.lua` |
| GoodWe ET-Plus / EH | GoodWe | Modbus | battery, meter, pv | no | GW10K-ET, GW8K-EH | `drivers/goodwe.lua` |
| Growatt SPH / MOD | Growatt | Modbus | battery, meter, pv | no | SPH6000, MOD9000TL3-XH | `drivers/growatt.lua` |
| Huawei SUN2000 | Huawei | Modbus | battery, meter, pv | yes | SUN2000-10KTL-M1, SUN2000-5KTL-L1 | `drivers/huawei.lua` |
| Kostal Plenticore Plus / Piko IQ | Kostal | Modbus | battery, meter, pv | no | Plenticore Plus 10, Piko IQ 7.0 | `drivers/kostal.lua` |
| Pixii PowerShaper | Pixii | Modbus | battery, meter | no | PowerShaper 2, PowerShaper 20 | `drivers/pixii.lua` |
| SMA Sunny Tripower / Sunny Boy Storage | SMA | Modbus | battery, meter, pv | no | Sunny Tripower 10.0, Sunny Boy Storage 3.7 | `drivers/sma.lua` |
| Sofar HYD-ES / HYD-EP | Sofar | Modbus | battery, meter, pv | no | HYD 6000-ES, HYD 20KTL-3PH | `drivers/sofar.lua` |
| SolarEdge HD-Wave / StorEdge | SolarEdge | Modbus | meter, pv | no | SE10K, StorEdge SE7600A-US | `drivers/solaredge.lua` |
| SolarEdge inverter (PV only) | SolarEdge | Modbus | pv | no | HD-Wave, StorEdge | `drivers/solaredge_pv.lua` |
| SolarEdge legacy K-series (display) | SolarEdge | Modbus | pv | no | SE7K, SE10K, SE17K, SE25K | `drivers/solaredge_legacy.lua` |
| Solis hybrid inverter | Solis | Modbus | battery, meter, pv | yes | RHI-6K-48ES-5G, S6-EH3P10K-H | `drivers/solis.lua` |
| Sungrow SH Hybrid Inverter | Sungrow | Modbus | battery, meter, pv | yes | SH5.0RT, SH6.0RT, SH8.0RT, SH10RT | `drivers/sungrow.lua` |
| Victron Cerbo GX / Venus GX | Victron | Modbus | battery, meter, pv | no | Cerbo GX, Venus GX | `drivers/victron.lua` |

Deye detects HV vs LV battery packs automatically on init and picks the
correct register map.

## Writing your own driver

- [`docs/writing-a-driver.md`](writing-a-driver.md) — canonical guide
  to the lifecycle, host API, sign convention, and catalog metadata.
- [`docs/writing-a-driver-with-claude-code.md`](writing-a-driver-with-claude-code.md)
  — a Claude Code workflow for porting device specs into a driver
  (added by a sibling unit).
- [`docs/testing-drivers-live.md`](testing-drivers-live.md) — running
  drivers against the built-in simulators and against real hardware on
  a Zap (added by a sibling unit).

Copy `drivers/sungrow.lua` (Modbus) or `drivers/ferroamp.lua` (MQTT)
as a starting template.

## Out of scope

The following device families aren't in this batch. They're slated for
separate batches once the corresponding host APIs land:

- **HTTP / REST drivers** — waiting on an `host.http_*` capability
  (no outbound HTTP client in the sandbox today).
- **Serial / P1 smart-meter drivers** — waiting on a `host.serial_*`
  capability for direct UART access on the Zap.
- **EV chargers** (OCPP 1.6 / 2.0.1) — waiting on an OCPP WebSocket
  capability and matching dispatch vocabulary in the control layer.

Until those host APIs exist, integrations for those device classes go
through MQTT bridges (e.g. an OCPP-to-MQTT proxy) or through a sibling
tool that publishes the same `meter` / `battery` / `pv` shapes.
