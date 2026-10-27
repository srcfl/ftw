---
"ftw": patch
---

A battery that sits exactly at its minimum level no longer gets a discharge order. Before, 5.0 % (or exactly the `soc_min` floor with no plan, during fuse protection and under the hourly import limit) still counted as above the floor.
