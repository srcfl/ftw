---
"ftw": patch
---

Keep issued forecasts and quarter-hour load and PV measurements in a local archive for up to two years, separate from planning. The archive keeps every run for 30 days, then one run per hour, and drops its oldest days to stay under 1 GiB.

The archive is informational. A problem in it does not mark the box degraded, and a full backup stores a consistent copy rather than the live file.
