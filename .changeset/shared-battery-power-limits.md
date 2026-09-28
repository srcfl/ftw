---
"ftw": patch
---

Use the same battery power limits for planning and control. When a power limit
is missing, both use the existing 5 kW control default instead of planning at
half the battery's energy capacity. Keep configured limits and one-sided zero
limits in both paths.
