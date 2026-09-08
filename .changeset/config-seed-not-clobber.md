---
"ftw": patch
---

A missing leftover config.yaml no longer starts the setup wizard over live
Settings. Core reloads settings/config_v1 from the sibling state.db and rewrites
the locator YAML. An edited leftover seed or a wizard document cannot replace
Settings already stored in SQLite; only an old-Core rollback save is imported.
