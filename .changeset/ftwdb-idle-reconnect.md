---
"ftw": patch
---

Close the FTWDB shadow connection after each durable batch so the next batch does not reuse a socket closed by the sidecar's idle timeout. This avoids repeated transport errors and delayed shadow copies during normal beta collection.
