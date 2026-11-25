---
"ftw": patch
---

Bundle teslamate_vehicle 0.2.2. It reads TeslaMate's `location` topic as
well as the older latitude and longitude topics. Without a geofence the
driver uses that position to tell whether the car is at the site, so a car
on a newer TeslaMate no longer loses it.
