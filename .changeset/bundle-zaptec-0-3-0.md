---
"ftw": patch
---

Bundle zaptec_cloud 0.3.0 and easee_cloud 1.5.0. Zaptec: a one-phase car on
an idle three-phase charger now starts at the charger's minimum instead of
getting 0 A, no positive current goes below the charger's own minimum,
removing the charger gives back the owner's maximum and resumes it, and slow
Zaptec Cloud calls get 20 s and one retry. Easee: re-sends the current limit
as a command, rejects out-of-order power readings, reports when Easee's load
balancing holds the car below FTW's order or holds a start, and exposes a
confirmed pilot current. Neither is confirmed on hardware yet.
