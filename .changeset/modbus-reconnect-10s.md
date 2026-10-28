---
"ftw": patch
---

A Modbus device that comes back after an outage is reached within 10 s. Core used to wait up to 60 s between reconnect attempts, so an inverter could hold its last forced battery mode for a minute after it returned.
