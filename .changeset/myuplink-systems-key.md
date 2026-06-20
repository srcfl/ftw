---
"forty-two-watts": patch
---

Fix MyUplink "connects but finds nothing": the driver read the wrong JSON
keys from the MyUplink API. Device auto-detection used `systems.objects`
but the real `/v2/systems/me` response keys the array as `systems`, so no
device was ever found. Also read the points unit from `parameterUnit`
(the real field name) instead of `unit`, so kW→W conversion works.
