---
"ftw": patch
---

A box moving from 2.x keeps its older chart history: Core no longer deletes Parquet sample days that have no hourly summary yet, and removes each one only after the background rollup has summarized it. A restart during the first history import resumes where it stopped instead of counting the imported samples twice, and a power cut while Core replaces its history file no longer leaves a box that refuses to start. Full backups now carry a copy of the price cache, so savings and cost history for past days come back after a restore. `ftw update` and the native installer flush the release and its receipt to disk before FTW starts, so a power cut right after an update cannot leave the launcher unable to start or roll back.
