---
"ftw": patch
---

The LiDAR derive now reads the full-density point cloud. Lantmäteriet's COPC
files key their octree nodes with a broken z origin (a node whose key implies
a slab 1.7 km underground holds the roof points), which made the windowed
query prune every dense level and return ~47 preview points — too few to fit
any plane. The query now spans the octree cube vertically, so pruning happens
on x/y only. Verified live: a picked Stockholm building went from "only 34
LiDAR returns" to 8 proposed arrays from 9 roof planes out of the 2021 scan.
