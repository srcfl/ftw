---
"ftw": patch
---

Bundle the driver fixes from the battery review: Growatt and victron_mqtt now report battery power with the right sign (charging positive); victron, ferroamp_modbus and growatt leave a failed read out instead of sending 0, and ferroamp_modbus drops NaN and infinite values; GoodWe, Fronius GEN24, AlphaESS and Kstar read the registers their vendors document; victron_mqtt sends the Venus OS keep-alive and declares it as a read request. None of it is verified on hardware yet.
