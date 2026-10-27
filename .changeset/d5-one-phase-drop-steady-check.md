---
"ftw": patch
---

Stop a heavy load on one phase from marking a three-phase car as one-phase. FTW now marks a one-phase car only when its own order rises and one phase follows. A three-phase car no longer gets triple its planned order or swings between 6 A and 16 A. After a restart, a one-phase car is marked again at its next order rise.
