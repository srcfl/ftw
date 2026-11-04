---
"ftw": patch
---

A battery whose Max SoC was saved as 0 charges again. Settings used to save a
blank number field as 0, and since 0.157.1 that 0 was the battery's charge
ceiling, so charging stopped at any level, manual Charge included. Core now
treats a battery soc_max of 0 or less as unset and uses the planner's ceiling,
and Settings saves a cleared number field as unset.
