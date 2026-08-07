# Roof geometry from Lantmäteriet

FTW's PV forecast needs the tilt and azimuth of each roof face. Typing them in
means measuring your own roof, and most people estimate. In Sweden the state
already flew a laser over it, so FTW can read the numbers instead.

This is **optional and Sweden-only**. Everywhere else, and whenever anything
below is missing, the numeric fields in **Settings → Weather → PV arrays** stay
the way they work today.

## What you need

A free [Geotorget](https://geotorget.lantmateriet.se) account with access
ordered to two products. Both are open data under CC BY 4.0; the account exists
so Lantmäteriet can see who is downloading, not to charge you.

| Product | Delivered as | What FTW uses it for |
|---|---|---|
| [Byggnad Nedladdning, vektor](https://geotorget.lantmateriet.se/geodataprodukter/byggnad-nedladdning-vektor-api) | STAC → **GeoPackage** | Building footprints, so you can point at your house |
| [Laserdata Nedladdning, Skog](https://geotorget.lantmateriet.se/geodataprodukter/laserdata-nedladdning-skog-api) | STAC → **LAZ as COPC** | The laser scan the roof planes are fitted to |

Both are STAC APIs behind the same account, so one set of credentials covers
both and FTW searches them the same way. They differ only in what the items
point at, and FTW picks the right asset by its declared media type rather than
by its name — a catalogue that renames `data` to `punktmoln` keeps working.

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

1. Open **Settings → Weather**.
2. Put the map marker on your building.
3. Under **Roof geometry from Lantmäteriet**, tick **Enable roof derivation**,
   enter your Geotorget username and token, and **Save**.
4. Press **Find buildings here**. Footprints appear on the map and as a list.
5. Click your building. It highlights green.
6. Press **Read roof from LiDAR**.

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
| "Geotorget rejected the credentials" | Wrong token, or the account has not been granted that product |
| "No buildings found here" | The marker is not on a building, or you are outside Sweden |
| "only N LiDAR returns fall on building" | The building is newer than the scan, or you picked the wrong footprint |
| "No roof faces worth mounting panels on" | Everything found was north-facing or under 8 m² |
| "the building tile could not be read" | The GeoPackage asset was not a GeoPackage — usually a changed download URL |

Failures never change your configuration.

*Data © Lantmäteriet, CC BY 4.0.*
