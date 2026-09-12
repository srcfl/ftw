---
"ftw": patch
---

Lua driver host: a missing `driver_command` is an error, a battery/PV/EV/V2X/heat-pump driver that can be commanded must implement `driver_default_mode`, and fingerprint probes cannot write hardware. A read-only declaration blocks dispatch before the command hook runs. The catalog now reads `auth_post_path`.

Update the recovery bundle to the companion driver audit, including corrected telemetry freshness, read-only declarations, and the Easee safe default. Include the read-only Zaptec Cloud and Tesla Wall Connector drivers so their setup paths also resolve offline.
