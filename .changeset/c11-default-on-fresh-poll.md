---
"ftw": patch
---

Send a driver's autonomous default on the device's first good poll after an
outage, when a default failed while the device was away. Before, the retry
waited for its backoff timer (up to 30 s), so the device could keep running
FTW's last forced battery command after it came back. FTW retries once per
return, not on every poll.
