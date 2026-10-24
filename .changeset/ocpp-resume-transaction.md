---
"ftw": patch
---

An OCPP 1.6 charger that keeps charging while Core restarts now has its session energy counted again. Before, Core forgot the running transaction, so session energy stayed at 0 until the car was unplugged, and a stop then reported the charger's lifetime meter total as the session. Core now takes up the transaction from the first meter value the charger sends for it and counts energy from that reading. Energy from before the restart is still lost. Transaction ids now start from the clock, so a restart no longer hands out an id that a charger may still hold.
