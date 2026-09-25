---
"ftw": patch
---

A car charging at a steady current is no longer stopped every three minutes.
Easee reports power only when it changes, and Core took the unchanged reading as
a stale charger and set it to 0 A, then started it again, all night. An old
charger power reading now only affects energy accounting; a stale site meter
still stops every charger. A stop that Core ordered itself no longer triggers
a replan, which had fed the start-stop loop.
