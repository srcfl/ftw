---
"ftw": patch
---

FTW now learns a thermal twin, in shadow, for each room sensor a driver declares (`sensors` with `asset = "space_heat"`). Every 30 minutes it fits the room's inertia, heat capacity, heat loss and other gains from the room temperature, the outdoor temperature and the heat pump's `hp_power_w`, and checks itself by predicting three hours ahead against what the sensor then measured. `GET /api/thermal/twins` shows each twin as learning, unproven, trusted or drifting, or says it has no heat input to learn from. A twin is trusted only when its error is small, it beats assuming the temperature stays as it is, and the heat input varied enough to learn from; a room a thermostat holds still stays unproven. `hp_power_w` is the pump's whole electrical power, so hot-water charging and defrost count as room heat until a driver can report space heat alone. A twin is deleted when its sensor is no longer declared. The twin commands nothing, and nothing plans from it yet.
