---
"ftw": patch
---

Speed up hourly forecast calibration reads with a partial SQLite index. Prepare the index during startup before Core begins writing live data. If the build fails, record the failure so later starts skip the build and reads continue using the existing index.
