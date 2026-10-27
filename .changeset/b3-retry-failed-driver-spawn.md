---
"ftw": patch
---

Retry a driver that fails to start, for example a USB P1 meter that is not
ready at boot. FTW tries again after 10 s and doubles the wait up to 5 min
until the driver starts, leaves the config or is removed. Before, the site
stayed without control until a restart or a settings save.
