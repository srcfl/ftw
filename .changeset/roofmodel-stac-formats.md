---
"ftw": minor
---

Read Lantmäteriet's data in the formats it is actually published in.

Both roof-geometry products are STAC APIs behind one Geotorget account, and they
differ only in what their items point at: *Byggnad Nedladdning, vektor* delivers
**GeoPackage**, *Laserdata Nedladdning, Skog* delivers **LAZ organised as COPC**
(Cloud Optimized Point Cloud). Assets are now chosen by their declared media
type instead of by guessing at asset names, so a catalogue that calls its asset
`punktmoln` rather than `data` still works, and a thumbnail is never mistaken
for a point cloud.

Building footprints are read straight out of the GeoPackage with the standard
library — a GeoPackage is a SQLite database holding geometry as WKB, both
published formats with fixed layouts, so this needs no GDAL and installs on a Pi
unchanged. Previously only inline STAC geometry was handled, which meant the
normal asset-backed case returned nothing at all.

Because COPC indexes its points into an octree, picking a building now also
makes the download small: FTW range-requests only the octree nodes covering that
footprint instead of pulling a 2.5 km tile that runs to hundreds of megabytes.
Plain `.laz` assets, servers that ignore `Range`, and builds of laspy without
COPC support all fall back to reading the tile whole — slower, same answer. The
derived model records which path ran as `source.fetch`.
