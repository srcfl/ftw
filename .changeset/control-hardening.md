---
"ftw": patch
---

The driver Restart button, applying or reverting a driver draft, and a MyUplink token refresh now start the driver with the battery's configured `soc_min` and `soc_max`, as startup does, instead of the driver's own defaults (95 % on a Ferroamp). A battery that keeps reporting power but no state of charge for five minutes now shows its SoC as unknown, and FTW stops discharging it until a fresh reading arrives. A manual V2X setpoint is refused while the site meter is stale; Stop still works. Replacing the site-meter driver no longer leaves its old phase-current readings behind, which could block all dispatch until FTW restarted. The control loop's safety default no longer waits for a slow operator control command on the same driver.
