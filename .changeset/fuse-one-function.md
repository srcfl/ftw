---
"ftw": patch
---

Fuse protection now reads the main fuse the same way for the car and the home battery: the site meter's current on each phase, with its direction, under one margin (your safety margin, at least 1 A). A battery's charge or a car may rise only into the room the phases show. When a phase goes over, the car is cut and the battery gives back only what the car's cut does not cover, so the two never answer the same overage twice. The meter's total no longer counts against the fuse when the meter reports phase currents, which a Pixii total that disagrees with its phases used to trip. A car now starts when its minimum fits, a second car plugging in no longer stops the first, and fuse relief no longer discharges a battery below its soc_min (or the planner's, 10 % by default).
