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
driver and shows it in `/api/status` (`inverter_w`,
`inverter_available_import_w`, `inverter_available_export_w`) and in
`/api/drivers/{name}` (`inverter`). It is not a device type: no site total,
dispatch, fuse guard or energy ledger reads it, and none of it is written to
history.

A battery short of its order while its inverter reports, in the same poll,
no more import headroom while charging, or no more export headroom while
discharging, now reads "Limited by the inverter", with words such as "The
inverter reports that it can take no more from the grid now. FTW asked for
5.0 kW charge." Headroom above 100 W, headroom from an earlier poll, and a
stale, offline or faulted driver explain nothing. Wrong direction, power
above the order or while idle, a changed setpoint and lost readings keep
their own reasons. Limited verdicts never send the "not following" notice.
