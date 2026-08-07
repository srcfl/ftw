---
"ftw": minor
---

Pick your building on the map and read the panel angles off Lantmäteriet's laser
scan, instead of measuring your own roof.

**Settings → Weather** gains a Geotorget credential form and a building picker.
Press *Find buildings here* and the footprints near the marker are drawn on the
map and listed beside it; click yours, press *Read roof from LiDAR*, and the PV
arrays fill in with one entry per usable roof face. The form is filled but
nothing is saved: the operator sees the numbers, corrects what is wrong and
presses Save. FTW never rewrites a panel configuration on its own, because the
derivation is a guess from a scan that may be years old and only the operator
knows whether that face has panels on it at all.

Picking a building is not cosmetic. Without a footprint the module segments
whatever stands inside its search radius, and the plane fitting is global — a
fitted plane is infinite, so a roof at azimuth 180° is `z = f(y)` with no `x`
term and extends across the whole tile. A second building sharing that ridge
orientation lands inside its inlier band however far away it is, and the two lose
returns to each other. Measured on a synthetic pair: a detached garage recovered
93% of its true area and split into two fragments while coplanar with the house,
against 100% and one clean face once clipped to its own footprint. Clipping also
buffers the outline by a metre so the eaves, where the lowest roof returns are,
are not shaved off.

New `GET /api/roofmodel/buildings` lists footprints as GeoJSON, honouring an
explicit `lat`/`lon` so the picker can search where the marker is rather than
where the last save put it. `POST /api/roofmodel/derive` accepts a `building_id`.
`GET /api/roofmodel` reports `has_credentials` so the UI can stop asking.

The Geotorget token now masks and restores like every other secret: it never
appears in an API response, and saving an unrelated setting no longer wipes it.

Documented in [docs/roof-geometry.md](../docs/roof-geometry.md), including how to
order the two Geotorget products, what the derived kWp does and does not mean,
and what each failure message is telling you.
