---
"ftw": minor
---

Bundle the drivers that report what a battery can do now. Sungrow (1.7.0), Huawei, Kostal, Sigenergy, GoodWe, Solis, SolaX, SAJ, Deye, FoxESS, victron_mqtt and the Solis C&I and Konja drivers report the battery's temperature and, where the device documents one, its live charge and discharge limit; Sungrow also says when the inverter is on standby. No driver sends a battery temperature of 0 when the read fails any more. The bundle also carries the charger control from srcfl/device-drivers (go-e, OpenEVSE, Hardy Barth, Etrel, KEBA, Schrack; refused stand-downs for Alfen, ABB and Mennekes) that has so far been only in the drivers-beta channel. None of it is verified on hardware yet.
