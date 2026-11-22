---
"ftw": minor
---

Read a hybrid's inverter stream, and say when its inverter has no grid
headroom left.

The driver contract lets a battery or hybrid driver report its AC stage with
`host.emit("inverter", …)`: its power, phases, rating, and
`available_import_W` / `available_export_W`, how much more the inverter will
take from or give to the grid now. Core refused that emit, so the Solis C&I
drivers lost it on every poll. Core now keeps the latest inverter reading per
driver. `/api/status` shows it while the driver is live (`inverter_w`,
`inverter_available_import_w`, `inverter_available_export_w`,
`inverter_updated_at_ms`), and `/api/drivers/{name}` shows the last one with
its age (`inverter`). It is not a device type: no site total, dispatch, fuse
guard or energy ledger reads it, and none of it is written to history. A
field that is not a usable number is left out and the rest kept.

A battery that gives part of its order while its inverter reports, in the
same driver poll, no more import headroom while charging, or no more export
headroom while discharging, now reads "Limited by the inverter", with words
such as "The inverter reports that it can take no more from the grid now.
FTW asked for 5.0 kW charge." A battery that gives nothing keeps its warning,
since a dead battery on a site that may not export sees no export headroom
either; only the battery's own report of 0 available explains it. Headroom
above 100 W, headroom from an earlier poll, and a stale, offline or faulted
driver explain nothing. Wrong direction, power above the order or while
idle, a changed setpoint and lost readings keep their own reasons. Limited
verdicts never send the "not following" notice.
