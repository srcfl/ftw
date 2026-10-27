---
"ftw": patch
---

A control interval or watchdog timeout of 0 in Settings now means "use the default" (2 s and 60 s). Before, a save with an empty or zero control interval made the next start crash in a loop. Boxes that already stored a 0 start again with the default.
