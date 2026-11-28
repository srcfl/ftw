---
"ftw": minor
---

Home Assistant gets lifetime energy totals for its Energy dashboard: Total grid import, Total grid export, Total PV generation, Total battery charged, Total battery discharged and Total load, in kWh. They come from the box's stored energy history since it began, never go down, never return to zero at midnight and count nothing twice across a restart. FTW's entities now have short names in sentence case that Home Assistant shows after the device name, such as "FTW Grid power"; a new install gets entity IDs such as `sensor.ftw_grid_power`. The sensors that show the grid target, peak limit and EV charging power end in "setpoint", so they no longer share a name with the number entities that set them. Unique IDs do not change, so an updated install keeps its entity IDs, history and the names you set yourself.
