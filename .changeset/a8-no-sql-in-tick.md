---
"ftw": patch
---

Keep the control loop free of database reads. The PV curtailment check now
skips device lookups when nothing is curtailed, and device lookups read the
in-memory device list, so a busy state database can no longer stall a tick.
