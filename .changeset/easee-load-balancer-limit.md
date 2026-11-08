---
"ftw": patch
---

When an Easee charger's own load balancing holds the car below FTW's order, the EV sheet now says "Easee limits the charger to 12 A (its load balancing). FTW asked for 15 A; the car takes 8.3 kW." instead of "Not following". It needs easee_cloud 1.4.1, which reports the current Easee lets the car take while it is below the order, and names its load balancing when Easee's allocation shows it. Without a named cause the sheet reads "The charger lets the car take 12 A, below the 15 A FTW asked for." A car taking far less or more than the limit allows is still judged as before, and an old charger reason is never shown as the cause.
