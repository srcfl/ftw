---
"ftw": minor
---

New optional roof-geometry module derives PV array tilt, azimuth and kWp from
Lantmäteriet open geodata, so a Swedish site can stop typing panel angles in by
hand.

`roofmodel/` is a separate Python package alongside `optimizer/`, invoked at
arm's length: core spawns it, hands it coordinates and the operator's own
Geotorget credentials, and reads back one versioned `roof_model.json`. LiDAR
segmentation drags in a compiled point-cloud stack and runs for minutes, so
keeping it in a time-boxed subprocess means it cannot stall the control tick or
leak into the daemon — and it can be absent entirely, which is the normal case,
since the data only exists for Sweden.

The pipeline follows the SPAN method (Yavuzdoğan, *Renewable Energy* 2023):
iterative RANSAC plane fitting pulls one roof surface at a time out of the point
cloud, then DBSCAN splits faces that share a plane equation but not a location —
two wings of a building fit the same plane and are not the same roof. Method
only; no code is taken from SPAN's GPL QGIS plugin, and the module depends on
numpy, scikit-learn and requests, all BSD.

`GET /api/roofmodel` reports availability and coverage; `POST
/api/roofmodel/derive` runs a derive and returns the proposed arrays. Applying
them to `weather.pv_arrays` is deliberately left as a separate explicit act:
derivation is a best guess from a point cloud that may be years old, and
silently rewriting an operator's panel config is a change they should make
knowingly. Lantmäteriet also joins the `/api/data-sources` registry as a
Sweden-only, credential-gated source.

Off by default. Absent credentials, absent module or a non-Swedish site all
produce a clean explanation rather than a failure.
