---
"ftw": patch
---

Keep durable settings and token writes within their caller's deadline while waiting for another state writer. Use FULL sync for the write, then restore the connection's prior busy timeout and sync settings.
