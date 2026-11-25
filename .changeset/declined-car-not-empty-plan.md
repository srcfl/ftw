---
"ftw": patch
---

A car that stopped asking for charge while FTW offered it now reads "Car not
taking power" instead of "Waiting for the plan". The plan leaves such a car
out for the rest of the session, so the old text blamed an empty plan for a
stop the car made. Nothing else changes: unplugging, a higher goal, Charge
now, measured charging, or a car on a finish-at-limit goal reporting that it
is charging still lets FTW offer current again, and the battery level is never
guessed from the stop.
