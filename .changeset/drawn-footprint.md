---
"ftw": patch
---

The roof section gains "Draw the footprint on the map": trace your
building's outline and the LiDAR derive clips to it, exactly as if the
footprint had come from a catalog. It is optional and stands in for "Find
buildings here" where the catalog publishes no building dataset over STAC —
the open LiDAR catalogs (IGN LiDAR HD, KAGIS) mostly ship point clouds only.
The traced ring travels as `footprint` ([lon, lat] pairs) through
POST /api/roofmodel/derive and `--footprint-json` into the module; a drawn
footprint and a picked building answer the same question, so the newest one
wins.
