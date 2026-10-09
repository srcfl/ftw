---
"ftw": patch
---

When an Easee charger's load balancer cuts the car below FTW's request, the EV sheet now says "Limited by the charger's load balancer" instead of "Not following" with no reason. Bundles easee_cloud 1.3.7, which keeps that reason while the car charges and matches its reason labels to Easee's published table.
