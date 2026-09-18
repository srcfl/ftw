---
"ftw": patch
---

On the Home Assistant app, **Restart now** re-execs Core in-process after a clean shutdown instead of exiting. Supervisor does not restart a stopped app unless Watchdog is on, so the old exit left FTW stopped until someone pressed Start.
