---
"ftw": patch
---

Keep Modbus drivers offline when a poll has a failed register read instead of accepting zero-filled telemetry as fresh data, and recover mute TCP sessions with a non-blocking reconnect cooldown.
