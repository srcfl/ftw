---
"ftw": minor
---

Roof-geometry credentials are now the Geotorget account username and password,
sent as HTTP Basic auth — Lantmäteriet provides no OAuth for its STAC download
APIs, so there is no issued token to paste. Configs that stored the secret
under the old `geotorget_token` key keep working and migrate to
`roofmodel.stac_password` on their next save.

The STAC client is catalog-agnostic while it is at it: `stac_base_url`,
`stac_buildings_collection`, `stac_lidar_collection` and `stac_bbox_epsg`
point FTW at any STAC-conformant catalog (search is the spec's
`POST {base}/search`), with Lantmäteriet as the default. A custom catalog also
lifts the Sweden-only coordinate gate, and needs no credentials at all when it
is open — anonymous access is only refused for the default Lantmäteriet
catalog, which always requires the operator's own account.
