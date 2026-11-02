---
"ftw": patch
---

Bundle tesla_wall_connector 0.1.2. An idle Tesla Wall Connector Gen 3 no longer shows charging power. With the contactor open, the unit still reads 0.5–1 A of sensor offset on its legs, and the driver multiplied that by the grid voltage, so an idle charger showed about 180–280 W in the flow and in history. The driver now reports 0 W while the contactor is open, and a leg that reads 0 V carries no power.
