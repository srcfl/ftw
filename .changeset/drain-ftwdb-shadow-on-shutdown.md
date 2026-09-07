---
"ftw": patch
---

Drain queued FTWDB shadow history for up to two seconds on normal shutdown and update, after hardware stops. Retry lost acknowledgements with the same commit and report any unconfirmed ticks when the sidecar cannot complete the copy.
