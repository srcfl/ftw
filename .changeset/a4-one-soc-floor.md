---
"ftw": patch
---

Keep the battery above its minimum level when no plan exists, during fuse protection and under the hourly import limit. The floor is the highest of 5 %, the battery's `soc_min` and the planner's `soc_min` (10 % by default). Below it, the car is the lever for the fuse. The hourly import limit also no longer discharges below a battery-boost reserve.
