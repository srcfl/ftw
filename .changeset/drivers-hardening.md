---
"ftw": patch
---

An `observe_only` driver is no longer handed back to its default mode when FTW stops, restarts, updates or reloads it, so a battery that a retailer's VPP controls keeps the VPP's settings. When FTW stops a driver it controls, the driver's own cleanup runs again after its default mode, so a PV curtailment on a Sungrow inverter, which only that cleanup releases, no longer stays latched. A driver that fails to start now closes the MQTT, Modbus and other connections it opened, so its retries do not fight each other for the same MQTT client ID. A driver restart or settings save that the browser abandons partway still starts the driver again instead of leaving it stopped until the next reload. Changing a driver's HTTP or WebSocket grant (revoking `allow_write` or setting a new TLS pin, for example) or its `supports_pv_curtail` flag now restarts that driver so the change takes effect.
