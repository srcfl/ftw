---
"ftw": patch
---

Keep the control loop on time while the forecast saves its device identity. The save no longer holds the lock the control loop reads, so a busy database can no longer delay control by up to 10 seconds. After a failed save, wait 30 seconds before trying again instead of retrying every second.
