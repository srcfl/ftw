---
"ftw": patch
---

Name the box's time zone by the rules its clock runs on. A Docker box that mounts the host's `/etc/localtime` used to tell the app and web client it was on UTC while it planned on the host's clock, so the car's ready time and the plan's days showed an hour or two off. Core now matches a copied zone file to the zone with the same rules, and says UTC only when the clock is UTC.
