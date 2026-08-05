---
"ftw": patch
---

A mode change from the app is confirmed as soon as it is applied. The forced replan runs off the command path: the Python optimizer can take longer than the app waits for a result, so a change that had already been applied and read back was reported "unconfirmed" purely because the planner was slow. The replan keeps running under a context that survives the session, so a phone dropping its socket right after tapping cannot abort it.
