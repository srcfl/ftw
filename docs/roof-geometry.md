# Roof geometry from Lantmäteriet

FTW's PV forecast needs the tilt and azimuth of each roof face. Typing them in
means measuring your own roof, and most people estimate. In Sweden the state
already flew a laser over it, so FTW can read the numbers instead.

This is **optional, and Sweden-only by default** (any standard STAC catalog
can stand in — see [Other countries, other catalogs](#other-countries-other-catalogs)).
Everywhere else, and whenever anything below is missing, the numeric fields in
**Settings → Planner → PV arrays** stay the way they work today.

## What you need

A free [Geotorget](https://geotorget.lantmateriet.se) account with access
ordered to two products. Both are open data under CC BY 4.0; the account exists
so Lantmäteriet can see who is downloading, not to charge you.

| Product | Delivered as | What FTW uses it for |
|---|---|---|
| [Byggnad Nedladdning, vektor](https://geotorget.lantmateriet.se/geodataprodukter/byggnad-nedladdning-vektor-api) | STAC → **GeoPackage** | Building footprints, so you can point at your house |
| [Laserdata Nedladdning, Skog](https://geotorget.lantmateriet.se/geodataprodukter/laserdata-nedladdning-skog-api) | STAC → **LAZ as COPC** | The laser scan the roof planes are fitted to |

Both sit behind the same account, so one set of credentials covers both and
FTW searches them the same way. As verified against the live service
(2026-09-02), they are **two separate STAC roots**: buildings are collection
`byggnader` on `api.lantmateriet.se/stac-vektor/v1` — one item per
municipality whose asset is a ZIP holding the GeoPackage — and Laserdata Skog
is collection `dsm-skoglig-copc` on `api.lantmateriet.se/stac-hojd/v1`. Both
searches take the STAC spec's WGS84 bbox, the catalogue metadata is readable
anonymously, and the credentials are enforced where it matters — on the asset
downloads. FTW picks the right asset by its declared media type rather than
by its name — a catalogue that renames `data` to `punktmoln` keeps working.

Authentication is **HTTP Basic with your Geotorget account username and
password**. Lantmäteriet provides no OAuth for these STAC APIs, so there is no
issued token to paste — the account credential itself is what the catalog
accepts. FTW stores the password like every other secret: it is written to the
config file on the host, masked in every API response, and never logged.

Ordering access is not instant — Lantmäteriet approves it — so do it before you
plan to use this.

You also need the `roofmodel` module's dependencies on the FTW host:

```bash
pip install -e roofmodel[geo]
```

The `geo` extra pulls the LAZ reader. Without it everything except reading the
point cloud works, which is enough to run the tests but not enough to derive a
real roof. GeoPackage needs nothing extra: it is a SQLite file, and FTW reads it
with the standard library.

## Other countries, other catalogs

The client speaks plain [STAC](https://stacspec.org/) — search is the spec's
`POST {base}/search`, downloads follow asset hrefs, and authentication is
ordinary HTTP Basic. Lantmäteriet is only the default. If another country
publishes building footprints and LiDAR through a STAC API, point FTW at it:

```yaml
roofmodel:
  enabled: true
  stac_base_url: https://stac.example.org        # STAC API root
  stac_buildings_collection: buildings-vector    # footprint polygons
  stac_lidar_collection: lidar-pointcloud        # LAZ/COPC point clouds
  stac_bbox_epsg: 4326                           # the spec's WGS84; 3006 = SWEREF
  stac_username: you                             # omit both for an open catalog…
  stac_password: "…"                             # …that needs no login
```

Setting `stac_base_url` also lifts the Sweden-only coordinate gate, since FTW
cannot know what a third-party catalog covers.

### Catalogs verified to answer (2026-09-02)

Each row was checked live: the STAC landing page, an item search over a real
bbox, and an anonymous HEAD on a returned asset.

| Catalog | LiDAR | Auth | Licence | Caveat |
|---|---|---|---|---|
| [Lantmäteriet](https://api.lantmateriet.se/stac-vektor/v1) (default; LiDAR on [stac-hojd](https://api.lantmateriet.se/stac-hojd/v1)) | buildings as zipped GeoPackage + LAZ/COPC | HTTP Basic (Geotorget account), enforced on downloads | CC BY 4.0 | Sweden; WGS84 bbox per the spec; end-to-end verified with a real account 2026-09-02 |
| [IGN LiDAR HD via MTD](https://api.stac.teledetection.fr) (`lidarhd`) | COPC, served by IGN itself | none | etalab-2.0 | France; catalog is run by Université de Montpellier, not IGN, and IGN's download host rate-limits (~1 req/s) |
| [KAGIS Carinthia](https://gis.ktn.gv.at/api/stac/v1/) (`KAGIS_coll_ALS2_pc_*`) | COPC | none | CC BY 4.0 | four Alpine regions only; ~700 MB tiles, so the COPC window path matters |
| [swisstopo](https://data.geo.admin.ch/api/stac/v1) (`ch.swisstopo.swisssurface3d`) | LAS zipped as `.las.zip` | none | swisstopo open data terms | Switzerland; the zip wrapper is not yet unpacked by the module, so this one is search-verified but not derive-ready |

USGS 3DEP (`3dep-lidar-copc` on the [Planetary
Computer](https://planetarycomputer.microsoft.com/api/stac/v1)) is one small
extension away: search is anonymous, but asset downloads need a SAS token
fetched from an open endpoint and appended to the URL.

Building footprints are the scarce half. No other verified catalog serves them
as GeoPackage or GeoJSON — swisstopo publishes DXF/FileGDB, Microsoft and
Overture publish GeoParquet — so outside Sweden the practical path today is a
LiDAR-only catalog plus drawing the outline yourself on the map.

Two caveats. The search bbox CRS is per-catalog: the STAC spec mandates WGS84
(`stac_bbox_epsg: 4326`), but Lantmäteriet expects SWEREF 99 TM, which is why
the default stays `3006`. And the *data* itself must arrive in SWEREF 99 TM
metres for now — the plane fitting works in that frame — so a foreign catalog
needs its point clouds delivered in a matching projected CRS before the derived
tilt/azimuth mean anything. Lifting that second limit means carrying a real
projection library, which the module has so far deliberately avoided.

### Why picking a building also makes it fast

COPC — Cloud Optimized Point Cloud — is LAZ with the points ordered into an
octree and an index at a known offset, so a reader can ask for a region and
fetch only the parts that cover it. Once you have picked a building, FTW asks
for the bounding box of *that footprint* instead of the tile: a Laserdata Skog
tile covers 2.5 km and runs to hundreds of megabytes, and a house is a few tens
of metres across.

This is best-effort. A plain (non-COPC) `.laz` asset, a server that ignores
`Range` requests, or a laspy without COPC support all fall back to reading the
tile whole — slower, same answer. The result records which path ran as
`source.fetch`: `copc-window` or `whole-tile`.

## Using it

1. Open **Settings → Planner**.
2. Put the map marker on your building.
3. Under **Roof geometry from Lantmäteriet**, tick **Enable roof derivation**,
   enter your Geotorget username and password, and **Save**.
4. Press **Find buildings here**. Footprints appear on the map and as a list.
5. Click your building. It highlights green.
6. Press **Read roof from LiDAR**.

Where the catalog publishes no building footprints over STAC — the open
LiDAR catalogs mostly ship point clouds only — press **Draw the footprint on
the map** instead of step 4 and trace your building's outline; the laser scan
is clipped to what you drew. Drawing is optional everywhere else.

The PV arrays above fill in with one entry per usable roof face. **Nothing is
saved yet** — look at the numbers, correct anything that is wrong, then press
Save. FTW never rewrites your panel configuration on its own: the derivation is
a good guess from a scan that may be several years old, and you are the one who
knows whether there are panels on that face at all.

## What the numbers mean, and what they do not

- **Tilt and azimuth** come from a plane fitted to the laser returns on your
  roof. These are the values worth trusting.
- **kWp** is an *upper bound on what fits*: roof area × a packing factor (0.70
  by default, covering ridges, eaves, chimneys and walkways) × 200 W/m². It is
  not what you have installed. If you have six panels on a face that could hold
  twenty, correct it.
- **North-facing pitched faces are dropped.** At Swedish latitudes they yield
  too little to be worth proposing. Flat roofs are kept, since panels on them
  get mounted facing south regardless of which way the building points.
- **Capture date** is shown with the result. Lantmäteriet is still backfilling
  the STAC `datetime` field through 2026, so it sometimes reads "date unknown".
  A roof built after the scan will not be in the data at all — you will get a
  clear error rather than a wrong answer.

## Why you pick a building

Without a footprint the module segments everything within its radius: your
neighbour's roof, the garage, the trees. Worse, the plane fitting is *global* —
a fitted plane is infinite, so a roof at azimuth 180° is described by `z = f(y)`
with no `x` term at all and does not stop at your wall. A second building
sharing your pitch and ridge orientation is not on a *similar* plane; it is on
the same one.

Measured on a synthetic pair, a single fitting pass over a house and a garage
40 m apart swallowed **all** of both south faces — 576 returns from one and 256
from the other — as one surface. The result was identical at every separation
from 3 m to 40 m, which is what tells you it is a global effect and not a
proximity one. Only the clustering step afterwards told the two buildings apart.

Clipping to your footprint first gives exactly the same face you would get by
scanning your building alone. Picking a building is what makes the answer yours.

## Optional: shading

Tilt and azimuth predict an unobstructed roof. They say nothing about the spruce
to the south, which can cost more than any amount of azimuth.

If you install [vostok](https://github.com/3dgeo-heidelberg/vostok) and set
`roofmodel.vostok_binary`, each face is additionally run against the surrounding
geometry and gets a `shading_factor`.

vostok is **GPL-3.0 and is not part of FTW**. FTW never bundles it, never ships
it and never installs it; you install it yourself and point FTW at it. It is run
as a separate process communicating through files, which is what keeps the
licences apart. Without it, faces carry no shading factor at all — deliberately
distinct from a factor of 1.0, because "we did not look" and "we looked and it
is clear" are different claims.

## When it does not work

| What you see | What it means |
|---|---|
| "Roof derivation is off" | Tick the box, add credentials, Save, retry |
| "the STAC catalog rejected the credentials" | Wrong username or password, or the account has not been granted that product |
| "No buildings found here" | The marker is not on a building, or you are outside Sweden |
| "only N LiDAR returns fall on building" | The building is newer than the scan, or you picked the wrong footprint |
| "No roof faces worth mounting panels on" | Everything found was north-facing or under 8 m² |
| "the building tile could not be read" | The GeoPackage asset was not a GeoPackage — usually a changed download URL |

Failures never change your configuration.

*Data © Lantmäteriet, CC BY 4.0.*
