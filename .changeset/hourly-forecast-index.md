---
"ftw": patch
---

Speed up hourly forecast calibration reads with a partial SQLite index. Prepare the index during startup before Core begins writing live data.
