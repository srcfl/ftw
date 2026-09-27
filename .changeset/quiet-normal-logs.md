---
"ftw": patch
---

The log no longer fills with warnings about normal operation. The meter clamp, which holds the grid at its target and stops the battery from exporting while covering the house, is logged once when it engages instead of as a warning every control tick. On the home box that was about 3,000 warnings a night. A failed fetch of tomorrow's prices before they are published, around 13:00, is logged at debug level; a failure for today, or for tomorrow after publication, is still a warning. Real warnings, such as a cloud charger going offline, stand out again in `journalctl` and in the support report.
