# Driver Catalog

FTW drivers are Lua files in [`drivers/`](../drivers/). Each
file declares a `DRIVER = { ... }` metadata block that the catalog loader
parses without starting a Lua VM. The Settings UI and
`GET /api/drivers/catalog` use the same metadata.

This document is a human-readable snapshot of the bundled catalog. When a
driver is added or removed, update this table from the `DRIVER` block rather
than hand-inventing metadata here.

For the planned non-breaking move from bundled-only drivers to an external
device repository, see [`device-repository.md`](device-repository.md). That
document is a plan, not the current install path.

## Protocols

Bundled drivers currently use:

- Modbus TCP
- MQTT
- HTTP
- WebSocket
- raw TCP

Every configured driver must be granted its protocol capability in
`config.yaml`; see [`configuration.md`](configuration.md).

## Bundled Drivers

| Driver | Manufacturer | Protocols | Capabilities | Tested models | File |
|---|---|---|---|---|---|
| Ambibox V2X | Ambibox | MQTT | v2x_charger | V2X Charger | `drivers/ambibox_v2x.lua` |
| CTEK Chargestorm (API v1) | CTEK | Modbus | ev | Chargestorm Connected 2/3 | `drivers/ctek.lua` |
| CTEK Chargestorm (API v2) | CTEK | Modbus | ev | Chargestorm Connected 2/3 | `drivers/ctek_v2.lua` |
| CTEK Chargestorm (Modbus + MQTT) | CTEK | Modbus, MQTT | ev | Chargestorm Connected 2/3 | `drivers/ctek_hybrid.lua` |
| Deye hybrid inverter | Deye | Modbus | meter, pv, battery | SUN-SG03LP1, SUN-SG04LP3 | `drivers/deye.lua` |
| Easee Cloud | Easee | HTTP | ev | Home, Charge | `drivers/easee_cloud.lua` |
| Eastron SDM630 meter | Eastron | Modbus | meter | SDM630 Modbus, SDM72D-M | `drivers/sdm630.lua` |
| Ferroamp DC2 V2X | Ferroamp | MQTT | v2x_charger | DC2 V2X 20 kW | `drivers/ferroamp_dc2_v2x.lua` |
| Ferroamp EnergyHub | Ferroamp | MQTT | meter, pv, battery | EnergyHub XL | `drivers/ferroamp.lua` |
| Ferroamp EnergyHub (Modbus) | Ferroamp | Modbus | meter, pv, battery | EnergyHub XL | `drivers/ferroamp_modbus.lua` |
| Fronius GEN24 | Fronius | Modbus | pv, battery | Symo GEN24, Primo GEN24 | `drivers/fronius.lua` |
| Fronius Smart Meter | Fronius | Modbus | meter | Smart Meter 50kA-3, 63A-3, TS 65A-3 | `drivers/fronius_smart_meter.lua` |
| GoodWe hybrid inverter | GoodWe | Modbus | meter, pv, battery | ET-Plus, EH series | `drivers/goodwe.lua` |
| Growatt hybrid inverter | Growatt | Modbus | meter, pv, battery | SPH, MOD | `drivers/growatt.lua` |
| Huawei SUN2000 Hybrid Inverter | Huawei | Modbus | meter, pv, battery | SUN2000L1, SUN2000-LUNA2000 | `drivers/huawei.lua` |
| Kostal Plenticore | Kostal | Modbus | meter, pv, battery | Plenticore Plus, Piko IQ | `drivers/kostal.lua` |
| Pixii PowerShaper | Pixii | Modbus | battery, meter | PowerShaper | `drivers/pixii.lua` |
| Pixii PowerShaper (PV + meter) | Pixii | MQTT | pv, meter | PowerShaper PV telemetry | `drivers/pixii_pv.lua` |
| SMA hybrid inverter | SMA | Modbus | meter, pv, battery | Sunny Tripower, Sunny Boy Storage | `drivers/sma.lua` |
| SMA PV inverter (non-hybrid) | SMA | Modbus | pv, meter, pv_curtail | Sunny Tripower CORE1/CORE2 | `drivers/sma_pv.lua` |
| Sofar hybrid inverter | Sofar Solar | Modbus | meter, pv, battery | HYD-ES, HYD-EP | `drivers/sofar.lua` |
| SolarEdge inverter + meter | SolarEdge | Modbus | meter, pv, pv-curtail | HD-Wave, StorEdge | `drivers/solaredge.lua` |
| SolarEdge inverter (PV only) | SolarEdge | Modbus | pv, pv-curtail | HD-Wave, StorEdge | `drivers/solaredge_pv.lua` |
| SolarEdge legacy (K-series with display) | SolarEdge | Modbus | pv, pv-curtail | SE17K display firmware | `drivers/solaredge_legacy.lua` |
| Solis hybrid inverter | Ginlong Solis | Modbus | meter, pv, battery | S6-EH, S5-GR, S6-GR | `drivers/solis.lua` |
| Solis string inverter | Ginlong Solis | Modbus | pv | S5-GC, S6-GR1P, 3P-G4, 1P-G4 | `drivers/solis_string.lua` |
| sonnenBatterie (local API) | sonnen | HTTP | battery | sonnen JSON API v2 | `drivers/sonnen.lua` |
| Sourceful Zap | Sourceful | HTTP | meter, pv | Zap local JSON gateway | `drivers/zap.lua` |
| Sungrow SH Hybrid Inverter | Sungrow | Modbus | meter, pv, battery | SH5.0RT, SH6.0RT, SH8.0RT, SH10RT | `drivers/sungrow.lua` |
| Tesla Vehicle (BLE Proxy) | Tesla | HTTP | vehicle | Model Y, Model 3 | `drivers/tesla_vehicle.lua` |
| Tibber Pulse | Tibber | WebSocket, HTTP | meter | Pulse IR, Pulse HAN, Pulse P1 | `drivers/tibber.lua` |
| Victron Energy GX | Victron Energy | Modbus | meter, pv, battery | Cerbo GX, Venus GX | `drivers/victron.lua` |
| Zuidwijk P1 Reader Ethernet | Zuidwijk | raw TCP | meter | Sagemcom T210-D, Kaifa MA105/MA304, Iskra ME382 | `drivers/zuidwijk_p1.lua` |

## Adding a Driver

Use [`writing-a-driver.md`](writing-a-driver.md) as the canonical guide.
The short version:

1. Copy a nearby driver: `sungrow.lua` for Modbus, `ferroamp.lua` for MQTT,
   `zap.lua` for HTTP, or `zuidwijk_p1.lua` for raw TCP.
2. Fill in the `DRIVER` metadata block.
3. Convert all telemetry to the site sign convention before `host.emit`.
4. Call `host.set_make` and `host.set_sn` as soon as identity is known.
5. Run the driver tests:

   ```bash
   cd go
   go test -count=1 ./internal/drivers/
   ```

Read-only drivers are useful and welcome. Control support can follow once
the native command path has been verified on real hardware.
