---
"ftw": patch
---

Slow cloud chargers no longer get paused for being slow. A driver can ask for
up to 30 s per call with `host.set_command_timeout_s`; the control tick still
waits only its short deadline, and the call finishes in the driver's own
queue. Before, a Zaptec command that took longer than 5 s failed, and the safe
default then paused the charger at 0 A.

Removing or disabling a driver now calls its optional `driver_release`, so a
charger goes back to the owner's own current instead of staying at FTW's 0 A.
A restart, a shutdown or a stale reading still uses `driver_default_mode`.
