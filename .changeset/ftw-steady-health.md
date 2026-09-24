---
"ftw": patch
---

`ftw update` and `ftw rollback` report health once it has held for five seconds. Right after a restart a device is briefly marked offline while Core replaces the reading from before the restart, so an immediate "Health: ok" could be followed by a degraded `ftw status`.
