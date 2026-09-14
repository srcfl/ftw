---
"ftw": patch
---

Stop history admission and background work before draining the accepted queue. Give the whole queue a separate shutdown budget, finish deferred cleanup on restart, and report an incomplete drain through a failed process exit. Allow 60 seconds for container shutdown during updates and restarts.
