---
"ftw": patch
---

The line that says the battery is not following the plan no longer mixes two
quarters. At a new quarter or a new plan, FTW used to hold the battery's last
minute against the new target and keep the old verdict for a minute, so it
could say "Charging 1.2 kW. The plan said: charge 4.2 kW." while the battery
charged 4.3 kW. FTW now starts the comparison over when the target moves.
A battery that comes back to the plan clears the line after 15 seconds
instead of a minute.
