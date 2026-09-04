"""Building footprints from Lantmaeteriet, and clipping LiDAR to one of them.

Picking a building matters more than it sounds. Searching LiDAR by a radius
around a coordinate returns the neighbours' roofs, the garage and whatever trees
stand in the garden, and the segmenter has no way to know which returns belong
to the operator's own house. Worse, RANSAC fits *infinite* planes over the whole
tile: an azimuth-180 roof plane is z = f(y) with no x term, so it does not stop
at the wall, and a second building sharing the pitch and ridge orientation lies
on *the same* plane rather than a similar one.

Measured on a synthetic pair, one RANSAC pass over a house and a garage 40 m
apart consumed all 576 of the house's south-face returns and all 256 of the
garage's as a single surface -- and produced identical output at every
separation from 3 m to 40 m, which is the signature of a global fit rather than
a neighbourhood effect. Only the DBSCAN pass afterwards told the two buildings
apart, leaving a clustering parameter as the sole thing standing between a
garage and its neighbour's roof.

Clipping to a chosen footprint removes that whole class of error: the returns
that reach the segmenter are the ones standing on the operator's building, and
the derived face is then identical to segmenting that building in isolation.

Coordinate frames
-----------------
GeoJSON mandates WGS84, and the live Lantmaeteriet STAC search takes a WGS84
bbox per the spec -- but the GeoPackages it hands out store their rings in
SWEREF 99 TM (EPSG:3006), and another catalogue may do either. Rather than
guess which one a given deployment returns, the frame is detected from the
magnitude of the numbers -- SWEREF eastings and northings are six and seven
figures, WGS84 degrees never are.
"""

from __future__ import annotations

import dataclasses
import math
from typing import Any, Iterable

from . import sweref
from .geopackage import GeoPackageError, read_features
from .geotorget import (
    COLLECTION_BUILDINGS,
    MEDIA_GEOJSON,
    MEDIA_GEOPACKAGE,
    MEDIA_ZIP,
    GeotorgetClient,
    GeotorgetError,
    StacItem,
)

# How far around the site to look for candidate buildings. Wide enough to reach
# a house set back from its coordinate, narrow enough not to return a village.
DEFAULT_SEARCH_RADIUS_M = 150.0

# A footprint larger than this is a tile boundary or a whole city block, not a
# building someone is about to mount panels on.
MAX_FOOTPRINT_AREA_M2 = 20000.0
# Below this it is a shed, a bin store or a digitising artefact.
MIN_FOOTPRINT_AREA_M2 = 8.0

# Roofs overhang their walls. Clipping exactly on the footprint would shave the
# eaves off every face, and the eaves are where the lowest roof returns are.
DEFAULT_EAVES_BUFFER_M = 1.0


class BuildingLookupError(GeotorgetError):
    """The building search succeeded but produced nothing usable."""


@dataclasses.dataclass
class Building:
    """One candidate building, ready to hand to a map."""

    building_id: str
    # Ring of (easting, northing) in SWEREF 99 TM -- the frame clipping happens
    # in, since the LiDAR arrives in it too.
    ring_sweref: list[tuple[float, float]]
    area_m2: float
    distance_m: float
    properties: dict[str, Any] = dataclasses.field(default_factory=dict)

    def centroid_sweref(self) -> tuple[float, float]:
        return _centroid(self.ring_sweref)

    def centroid_wgs84(self) -> tuple[float, float]:
        e, n = self.centroid_sweref()
        return sweref.sweref99tm_to_wgs84(n, e)

    def ring_wgs84(self) -> list[list[float]]:
        """GeoJSON ring: [lon, lat] pairs, closed."""
        out = []
        for e, n in self.ring_sweref:
            lat, lon = sweref.sweref99tm_to_wgs84(n, e)
            out.append([round(lon, 7), round(lat, 7)])
        if out and out[0] != out[-1]:
            out.append(out[0])
        return out

    def to_geojson(self) -> dict[str, Any]:
        lat, lon = self.centroid_wgs84()
        return {
            "type": "Feature",
            "id": self.building_id,
            "geometry": {"type": "Polygon", "coordinates": [self.ring_wgs84()]},
            "properties": {
                "building_id": self.building_id,
                "area_m2": round(self.area_m2, 1),
                "distance_m": round(self.distance_m, 1),
                "latitude": round(lat, 6),
                "longitude": round(lon, 6),
                **self.properties,
            },
        }


def _looks_like_sweref(ring: Iterable[Iterable[float]]) -> bool:
    """True when a ring is projected metres rather than degrees.

    SWEREF 99 TM eastings run roughly 200 000-900 000 and northings 6 100 000-
    7 700 000. No WGS84 coordinate can reach either, so one look at the
    magnitude settles which frame a ring is in.
    """
    for point in ring:
        pt = list(point)
        if len(pt) < 2:
            continue
        if abs(pt[0]) > 180.0 or abs(pt[1]) > 90.0:
            return True
    return False


def building_from_drawn_footprint(points: list[Any]) -> Building:
    """A hand-drawn outline as a Building, for catalogs with no footprints.

    `points` are GeoJSON-style [lon, lat] pairs traced on the map. The
    operator's drawing stands in for the picker where no building dataset is
    published (IGN's LiDAR HD, for instance, ships point clouds but no
    footprints). The ring is projected to the pipeline's working frame; where
    the point cloud arrives in another projection the clip fails loudly with
    "no returns fall on the footprint" rather than clipping the wrong spot.
    """
    ring: list[tuple[float, float]] = []
    for pt in points or []:
        seq = list(pt)
        if len(seq) < 2:
            continue
        lon, lat = float(seq[0]), float(seq[1])
        n, e = sweref.wgs84_to_sweref99tm(lat, lon)
        ring.append((e, n))
    if len(ring) >= 2 and ring[0] == ring[-1]:
        ring.pop()
    if len(ring) < 3:
        raise BuildingLookupError(
            "a drawn footprint needs at least three corners"
        )
    area = _shoelace_area(ring)
    if area < MIN_FOOTPRINT_AREA_M2:
        raise BuildingLookupError(
            f"the drawn footprint encloses {area:.1f} m2, too small to hold a "
            "roof; draw the building's outline, not a point"
        )
    return Building(
        building_id="drawn-footprint",
        ring_sweref=ring,
        area_m2=area,
        distance_m=0.0,
        properties={"source": "drawn"},
    )


def _shoelace_area(ring: list[tuple[float, float]]) -> float:
    """Planar polygon area in square metres. Ring must be projected."""
    n = len(ring)
    if n < 3:
        return 0.0
    total = 0.0
    for i in range(n):
        x1, y1 = ring[i]
        x2, y2 = ring[(i + 1) % n]
        total += x1 * y2 - x2 * y1
    return abs(total) / 2.0


def _centroid(ring: list[tuple[float, float]]) -> tuple[float, float]:
    """Area centroid of a projected ring, falling back to the vertex mean."""
    n = len(ring)
    if n == 0:
        return (0.0, 0.0)
    if n < 3:
        return (sum(p[0] for p in ring) / n, sum(p[1] for p in ring) / n)
    cx = cy = a = 0.0
    for i in range(n):
        x1, y1 = ring[i]
        x2, y2 = ring[(i + 1) % n]
        cross = x1 * y2 - x2 * y1
        a += cross
        cx += (x1 + x2) * cross
        cy += (y1 + y2) * cross
    if abs(a) < 1e-9:  # degenerate (collinear) ring
        return (sum(p[0] for p in ring) / n, sum(p[1] for p in ring) / n)
    a *= 0.5
    return (cx / (6.0 * a), cy / (6.0 * a))


def _rings_from_geometry(geometry: dict[str, Any]) -> list[list[tuple[float, float]]]:
    """Outer rings of a GeoJSON Polygon or MultiPolygon, in their own frame."""
    if not isinstance(geometry, dict):
        return []
    kind = geometry.get("type")
    coords = geometry.get("coordinates") or []
    rings: list[list[tuple[float, float]]] = []
    if kind == "Polygon" and coords:
        rings.append([(float(p[0]), float(p[1])) for p in coords[0] if len(p) >= 2])
    elif kind == "MultiPolygon":
        for poly in coords:
            if poly:
                rings.append([(float(p[0]), float(p[1])) for p in poly[0] if len(p) >= 2])
    return [r for r in rings if len(r) >= 3]


def _normalize_sweref_ring(
    ring: list[tuple[float, float]],
) -> list[tuple[float, float]]:
    """Force a projected ring into (easting, northing) order.

    GIS files conventionally store x=easting first, but EPSG:3006 formally
    declares north-first and some exports follow the registry. The two ranges
    cannot collide -- eastings stay under a million metres, northings start
    above six million -- so each point states its own order.
    """
    out = []
    for point in ring:
        x, y = float(point[0]), float(point[1])
        if x > 1_000_000.0 and y < 1_000_000.0:
            x, y = y, x
        out.append((x, y))
    return out


def _to_sweref(ring: list[tuple[float, float]]) -> list[tuple[float, float]]:
    """Any ring -> (easting, northing) SWEREF, the order ring_sweref documents.

    Both sources must land in the same order: the LiDAR clip compares these
    rings against point coordinates, and wgs84_to_sweref99tm returns
    (northing, easting) -- stored unswapped it would put every inline-geometry
    building's clip window sideways.
    """
    if _looks_like_sweref(ring):
        return _normalize_sweref_ring(ring)
    # GeoJSON order is [lon, lat].
    return [
        (e, n)
        for n, e in (sweref.wgs84_to_sweref99tm(lat, lon) for lon, lat in ring)
    ]


def _gpkg_from_zip(payload: bytes) -> bytes:
    """The GeoPackage inside a ZIP asset.

    Lantmaeteriet's live `byggnader` collection carries one item per
    municipality whose only asset is `byggnad_kn<code>.zip` with the
    GeoPackage inside. The largest `.gpkg` member wins if there are several;
    anything else in the archive (metadata PDFs, licence texts) is ignored.
    """
    import io
    import zipfile

    try:
        with zipfile.ZipFile(io.BytesIO(payload)) as zf:
            members = [i for i in zf.infolist() if i.filename.lower().endswith(".gpkg")]
            if not members:
                raise BuildingLookupError(
                    "the building tile ZIP holds no GeoPackage"
                )
            member = max(members, key=lambda i: i.file_size)
            return zf.read(member)
    except zipfile.BadZipFile as exc:
        raise BuildingLookupError(
            f"the building tile was announced as ZIP but did not open: {exc}"
        ) from exc


def _features_from_item(
    item: StacItem,
    client: GeotorgetClient | None = None,
    window_boxes: list[tuple[float, float, float, float]] | None = None,
) -> list[dict[str, Any]]:
    """Every building-like feature an item carries.

    A STAC item may *be* the building -- geometry inline, no download -- or it
    may be a tile whose asset holds thousands of them. Lantmaeteriet's live
    catalogue publishes `byggnader` as one zipped **GeoPackage** per
    municipality, so the asset path is the normal one and the inline path the
    exception — and `window_boxes` (the search window, in each frame the file
    could be in) keeps a whole city from being decoded for a 150 m search.
    """
    # A usable data asset wins over the item's own geometry: a tile item's
    # geometry is the TILE's outline — for Lantmaeteriet, the whole
    # municipality — and reading it as a building both invents a footprint
    # nobody has and skips the real ones in the asset. Inline geometry is the
    # fallback for catalogues whose items *are* the buildings.
    asset = item.pick(MEDIA_GEOPACKAGE, MEDIA_ZIP, MEDIA_GEOJSON)
    if asset is None or not asset.href or client is None:
        geom = (item.raw or {}).get("geometry")
        if geom:
            return [{"geometry": geom, "properties": (item.raw or {}).get("properties") or {},
                     "id": item.item_id}]
        return []
    media = asset.effective_media_type
    payload = client.download(asset.href)
    if media == MEDIA_GEOJSON:
        return _features_from_geojson(payload)
    if payload[:4] == b"PK\x03\x04":
        payload = _gpkg_from_zip(payload)
    try:
        return read_features(payload, bboxes=window_boxes)
    except GeoPackageError as exc:
        raise BuildingLookupError(
            f"the building tile for this site could not be read: {exc}"
        ) from exc


def _features_from_geojson(payload: bytes) -> list[dict[str, Any]]:
    """A GeoJSON FeatureCollection asset, for catalogues that publish one."""
    import json

    try:
        doc = json.loads(payload.decode("utf-8"))
    except (UnicodeDecodeError, ValueError) as exc:
        raise BuildingLookupError(
            f"the building tile was announced as GeoJSON but did not parse: {exc}"
        ) from exc
    if isinstance(doc, dict) and doc.get("type") == "FeatureCollection":
        return list(doc.get("features") or [])
    if isinstance(doc, dict) and doc.get("type") == "Feature":
        return [doc]
    return []


def buildings_from_features(
    features: Iterable[dict[str, Any]],
    *,
    latitude: float,
    longitude: float,
    fallback_id: str = "building",
) -> list[Building]:
    """Turn GeoJSON-ish features into ranked Building candidates."""
    site_n, site_e = sweref.wgs84_to_sweref99tm(latitude, longitude)
    out: list[Building] = []
    for i, feat in enumerate(features):
        for j, ring in enumerate(_rings_from_geometry(feat.get("geometry") or {})):
            ring_sweref = _to_sweref(ring)
            area = _shoelace_area(ring_sweref)
            if area < MIN_FOOTPRINT_AREA_M2 or area > MAX_FOOTPRINT_AREA_M2:
                continue
            cx, cy = _centroid(ring_sweref)
            props = dict(feat.get("properties") or {})
            bid = str(feat.get("id") or props.get("objektidentitet") or f"{fallback_id}-{i}")
            if j:
                bid = f"{bid}-{j}"
            out.append(Building(
                building_id=bid,
                ring_sweref=ring_sweref,
                area_m2=area,
                distance_m=math.hypot(cx - site_e, cy - site_n),
                properties={k: v for k, v in props.items() if isinstance(v, (str, int, float))},
            ))
    out.sort(key=lambda b: b.distance_m)
    return out


def search_buildings(
    client: GeotorgetClient,
    *,
    latitude: float,
    longitude: float,
    radius_m: float = DEFAULT_SEARCH_RADIUS_M,
    limit: int = 50,
    collection: str = COLLECTION_BUILDINGS,
    bbox_epsg: int = 4326,
) -> list[Building]:
    """Building footprints near a site, nearest first."""
    bbox = sweref.stac_search_bbox(latitude, longitude, radius_m, bbox_epsg)
    # The same window in every frame and axis order a tile could be stored
    # in. Coordinate magnitude keeps the frames apart (degrees never look
    # like metres), and carrying both axis orders costs nothing but a few
    # extra decoded rows — a box only ever widens what is kept.
    def swapped(b):
        return (b[1], b[0], b[3], b[2])

    wgs = sweref.stac_search_bbox(latitude, longitude, radius_m, 4326)
    swe = sweref.stac_search_bbox(latitude, longitude, radius_m, 3006)
    window_boxes = [wgs, swapped(wgs), swe, swapped(swe)]
    items = client.search(collection, bbox, limit=limit)
    features: list[dict[str, Any]] = []
    for item in items:
        features.extend(_features_from_item(item, client, window_boxes))
    if not features:
        raise BuildingLookupError(
            "no building footprints were returned for this site. The Geotorget "
            "account needs access to 'Byggnad Nedladdning, vektor', and the data "
            "covers Sweden only."
        )
    return buildings_from_features(
        features, latitude=latitude, longitude=longitude
    )


def point_in_ring(x: float, y: float, ring: list[tuple[float, float]]) -> bool:
    """Ray-casting point-in-polygon, in the ring's own projected frame."""
    inside = False
    n = len(ring)
    for i in range(n):
        x1, y1 = ring[i]
        x2, y2 = ring[(i + 1) % n]
        if (y1 > y) != (y2 > y):
            if y2 != y1 and x < x1 + (y - y1) * (x2 - x1) / (y2 - y1):
                inside = not inside
    return inside


def inflate_ring(ring: list[tuple[float, float]], metres: float) -> list[tuple[float, float]]:
    """Push a ring outward from its centroid by roughly `metres`.

    Approximate on purpose: a true polygon offset needs a geometry library, and
    this only has to catch the eaves. For the compact, roughly convex outlines
    houses actually have, scaling about the centroid is within a few centimetres
    of a proper offset; for a long thin wing it over-buffers the short sides,
    which costs a little neighbouring ground rather than losing roof.
    """
    if metres <= 0 or len(ring) < 3:
        return ring
    cx, cy = _centroid(ring)
    out = []
    for x, y in ring:
        dx, dy = x - cx, y - cy
        d = math.hypot(dx, dy)
        if d < 1e-9:
            out.append((x, y))
            continue
        scale = (d + metres) / d
        out.append((cx + dx * scale, cy + dy * scale))
    return out


def clip_to_footprint(points, ring: list[tuple[float, float]],
                      *, buffer_m: float = DEFAULT_EAVES_BUFFER_M):
    """Keep only the returns standing on one building.

    `points` is (N, 3) in SWEREF 99 TM metres, the frame the LiDAR arrives in.
    """
    import numpy as np

    pts = np.asarray(points, dtype=float)
    if len(pts) == 0 or len(ring) < 3:
        return pts
    outline = inflate_ring(ring, buffer_m)

    # Cheap rejection first: most of a tile is nowhere near the building.
    xs = [p[0] for p in outline]
    ys = [p[1] for p in outline]
    box = (
        (pts[:, 0] >= min(xs)) & (pts[:, 0] <= max(xs))
        & (pts[:, 1] >= min(ys)) & (pts[:, 1] <= max(ys))
    )
    candidates = np.nonzero(box)[0]
    keep = [i for i in candidates if point_in_ring(pts[i, 0], pts[i, 1], outline)]
    return pts[keep]
