---
"ftw": patch
---

Roof derivation now works against the real Lantmäteriet service, verified
end-to-end with a live Geotorget account. The catalogue turned out to differ
from its paper description in four ways, each now handled: the STAC service
is two roots (`stac-vektor/v1` for buildings, `stac-hojd/v1` for the point
clouds) rather than one; the collections are `byggnader` and
`dsm-skoglig-copc`; searches take the STAC spec's WGS84 bbox; and buildings
arrive as one ZIP-wrapped GeoPackage per municipality — 90k+ features with no
per-row envelopes, so the reader now clips to the search window by parsed
geometry bounds instead of truncating at a row limit, and a tile item's own
geometry (the municipality outline) is no longer mistaken for a building.

Credentials typed into Settings also apply immediately: the roofmodel service
follows config hot-reload instead of keeping its boot-time snapshot, and a
module error on stderr survives third-party library warnings above it.

The roof section gains a data-catalog picker: Lantmäteriet (default), the
live-verified open catalogs (IGN LiDAR HD France, KAGIS Carinthia), or any
custom STAC endpoint with its own collections — open catalogs need no
credentials.
