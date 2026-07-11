---
"forty-two-watts": minor
---

**The system now understands when a device is reachable but faulted, and stops
relying on it.** Previously a driver that kept emitting telemetry was treated as
healthy even if the device had gone into a fault state it couldn't act on — so
the dispatcher kept commanding it and the MPC kept planning against it. A
Ferroamp EnergyHub in *Fault Mode* (relays open, can't charge/discharge) would
silently turn the battery's commanded share into **grid import**, while a healthy
second battery sat under-used.

- **New host capability `host.set_device_fault(faulted, reason)`** lets a driver
  flag a device-level fault. It's orthogonal to the watchdog (which only catches
  "stopped emitting") — the driver keeps emitting, but `IsOnline()` returns false.
- **Dispatch + MPC exclude a faulted driver** automatically (both already gate on
  `IsOnline()`), so the load is covered by the healthy battery instead of
  imported, and the plan stops counting capacity that isn't there.
- **`/api/status`** shows the driver as `"fault"` with `device_fault_reason`;
  `/api/health` reports `drivers_faulted`; the transition is logged to `/api/logs`.
- **The Ferroamp driver** now detects EnergyHub Fault Mode (`ehub.state` bit 15)
  and flags/clears the fault automatically.
