---
"ftw": patch
---

A safety default now rejects older driver commands still waiting in the queue. Those commands can no longer take control again after the driver returns to its default mode. New commands after the default still work.
