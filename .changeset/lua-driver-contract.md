---
"ftw": patch
---

Lua driver host: a missing `driver_command` is an error, a battery/EV/V2X/heat-pump driver that can be commanded must implement `driver_default_mode`, and fingerprint probes cannot write hardware. The catalog now reads `auth_post_path`.
