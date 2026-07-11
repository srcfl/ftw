---
"forty-two-watts": patch
---

main: classify EV / vehicle drivers via Lua DRIVER catalog instead of filename prefix sniffing

`driverCapacitiesFrom` and `warnIfEVHasBatteryCapacity` previously fell back
to a hard-coded filename-prefix allowlist (`easee`, `ocpp`, `ctek`,
`chargestorm`, `tesla_vehicle`, `vehicle`) to decide whether a YAML driver
entry pointed at an EV charger or vehicle telemetry source. That coupled
the controller surface to vendor names and required code edits whenever a
new EV-charger Lua driver was added.

Each Lua driver already self-declares its kind via the `DRIVER = { …
capabilities = { … } }` table at the top of the file. EV chargers
declare `"ev"`; vehicle telemetry drivers declare `"vehicle"`. This PR
adds `drivers.IsEVOrVehicleDriver(catalog, luaPath)` which consults that
self-declaration, and routes both pre-flight call sites through it. The
filename sniffer is deleted.

The catalog is parsed once at startup (text scan, no Lua VM) and re-
scanned on every config hot-reload so a hot-edited driver's capability
change is picked up without a service restart.

No new schema. No operator action required — every EV/vehicle driver
that shipped with the repo already declares the right capability.
