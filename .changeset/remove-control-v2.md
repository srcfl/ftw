---
"ftw": patch
---

The unused signed control v2 driver runtime is gone. Nothing could reach it after Device Support packages were retired, so drivers are started, commanded and returned to their default mode exactly as before. A read-only driver installed from the signed channel may now `host.sleep` for longer than 100 ms, the same as its bundled copy. The `driver_command_results` table stays untouched in existing databases and is no longer created in new ones.
