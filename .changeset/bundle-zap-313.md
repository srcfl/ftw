---
"ftw": patch
---

Bundle zap 3.1.3. `read_pv` and `read_battery` work again on a Sourceful Zap with firmware 7.x, which renamed the PV and battery fields in its local API. A site that reads PV or battery through its Zap gets those readings back. Sites with a native inverter driver are not affected.
