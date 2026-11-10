---
"ftw": patch
---

A driver can now say what a control does to heat. A control may declare `asset` (space_heat, dhw, cooling or buffer), `strategy` (curve_offset, setpoint, on_off or discrete_mode), `zone`, `min_interval_s`, and for on_off `min_on_s` and `min_off_s`. A driver may also list `sensors`: the metrics that are a heat store's temperature. `/api/drivers/{name}` shows both. Nothing plans for or sends a heat command because of them yet.

A string control may declare a closed set of `options`. The control API then refuses any other value instead of passing it to the driver. A control that declares these fields wrongly is left out, as an unrenderable control is today. Drivers that declare none of them are unchanged.
