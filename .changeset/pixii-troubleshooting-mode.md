---
"forty-two-watts": patch
---

Add a global `site.troubleshooting_mode` for incident diagnostics. The mode exposes its state in `/api/status`, logs dispatch-decision snapshots without changing control behavior, and passes a reserved troubleshooting flag into Lua drivers. Pixii now uses that flag to emit calibration/control status and setpoint readback metrics, while still supporting its legacy per-driver troubleshooting flag. Invalid Pixii SoC values now omit `soc` from the battery emit instead of dropping the whole battery reading.
