---
"ftw": minor
---

The support file now answers for the last seven days, not the last hour.
Its help report gains "What happened over the last week": per day and
night, the battery's lowest and highest charge, energy in and out of the
battery, to the car and over the grid, how often the plan changed and why,
how far the battery strayed from the plan, fuse limits, devices out of
contact, plans that fell back to Core and restarts, with the notable ones
stated first. The archive carries the week behind it in `week/`: every
series in five-minute buckets, the control journal, plan snapshots,
notifications, start and stop events, and a week of warnings and errors
counted per hour, which the box now stores so they outlast the log buffer
and a restart. The redacted config and logs no longer show the owner's
e-mail address or account username.
