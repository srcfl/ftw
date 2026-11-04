---
"ftw": patch
---

Bundle zap 3.1.2. A Sourceful Zap works as the site meter again after it updates to firmware 7.x. That firmware renamed the fields in its local API, so the driver found no power reading and FTW lost its site meter and stopped dispatching. The driver now reads both the old and the new names. `read_pv` and `read_battery` on a Zap with firmware 7.x are not covered yet.
