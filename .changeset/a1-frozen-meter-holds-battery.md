---
"ftw": patch
---

Stop a frozen site meter from driving the home battery. When the meter has no reading newer than the one the last battery order used, for example while its reads fail inside the watchdog window, the battery keeps its last order and may only move toward 0 W. Before, a held +2 kW import reading wound the battery to 10 kW discharge in ten ticks while the site exported 8 kW.
