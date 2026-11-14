---
"ftw": patch
---

FTW now learns a thermal twin, in shadow, for each room sensor a driver declares (`sensors` with `asset = "space_heat"`). Every 30 minutes it fits the room's inertia, heat capacity, heat loss and other gains from the room temperature, the outdoor temperature and the heat pump's `hp_power_w`, and checks itself by predicting three hours ahead against what the sensor then measured. `GET /api/thermal/twins` shows each twin as learning, trusted or drifting, or says it has no heat input to learn from. The twin commands nothing, and nothing plans from it yet.
