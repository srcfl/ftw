"""Read polygon features out of a GeoPackage, using only the standard library.

Lantmaeteriet publishes *Byggnad Nedladdning, vektor* as GeoPackage, so a STAC
item for a building tile carries a `.gpkg` asset rather than inline GeoJSON.

A GeoPackage is a SQLite database with an agreed set of metadata tables, and
geometry stored as a small binary header followed by standard WKB. Both are
published specifications with fixed layouts, so decoding them here costs about
a hundred lines of `sqlite3` and `struct` -- against a GDAL/fiona/geopandas
dependency that would not install on a Pi without a compiler and pulls in a
second projection stack we already decided not to carry (see sweref.py, which
implements SWEREF 99 TM directly for the same reason).

Only what a roof model needs is read: polygon and multipolygon rings, in the
file's own coordinate reference system. Curves, triangulated surfaces and the
extended (`GPB` "extended geometry") binary types are rejected explicitly rather
than mis-parsed, because a wrong ring silently clips the wrong LiDAR.

References
----------
GeoPackage Encoding Standard (OGC 12-128r19), clause 2.1.3 "BLOB Format".
OpenGIS Simple Features (OGC 06-103r4), clause 8.2 "Well-known Binary".
"""

from __future__ import annotations

import os
import sqlite3
import struct
import tempfile
from typing import Any, Iterator

# "GP" -- the two magic bytes every GeoPackage geometry blob starts with.
GPKG_MAGIC = b"GP"

# Envelope sizes in doubles, indexed by the header's envelope indicator.
# 0 = absent, 1 = xy, 2 = xyz, 3 = xym, 4 = xyzm. 5-7 are reserved.
_ENVELOPE_DOUBLES = {0: 0, 1: 4, 2: 6, 3: 6, 4: 8}

# WKB geometry type codes we can use. The ISO variants add 1000 for Z, 2000 for
# M and 3000 for ZM, so the base code is recovered with % 1000.
_WKB_POLYGON = 3
_WKB_MULTIPOLYGON = 6

# The EWKB flag bits PostGIS adds to the type word. GeoPackage forbids them, but
# files written by other tools do turn up, and reading a flagged type as a
# geometry code would silently produce nonsense.
_EWKB_Z = 0x80000000
_EWKB_M = 0x40000000
_EWKB_SRID = 0x20000000


class GeoPackageError(ValueError):
    """The file is not a GeoPackage, or holds geometry we will not guess at."""


def _unpack(fmt: str, data: bytes, offset: int) -> tuple[Any, ...]:
    size = struct.calcsize(fmt)
    if offset + size > len(data):
        raise GeoPackageError("geometry blob ended mid-value")
    return struct.unpack_from(fmt, data, offset)


def _dimensions(type_word: int) -> tuple[int, int]:
    """(base geometry code, coordinates per point) for a WKB type word."""
    has_z = bool(type_word & _EWKB_Z)
    has_m = bool(type_word & _EWKB_M)
    code = type_word & ~(_EWKB_Z | _EWKB_M | _EWKB_SRID)
    # ISO style: 1000/2000/3000 offsets carry the same information.
    if code >= 3000:
        code, has_z, has_m = code - 3000, True, True
    elif code >= 2000:
        code, has_m = code - 2000, True
    elif code >= 1000:
        code, has_z = code - 1000, True
    return code, 2 + int(has_z) + int(has_m)


def _read_ring(data: bytes, offset: int, endian: str, coords: int) -> tuple[list[tuple[float, float]], int]:
    (count,) = _unpack(endian + "I", data, offset)
    offset += 4
    stride = 8 * coords
    ring: list[tuple[float, float]] = []
    for _ in range(count):
        x, y = _unpack(endian + "dd", data, offset)
        ring.append((x, y))
        offset += stride
    return ring, offset


def _read_polygon(data: bytes, offset: int, endian: str, coords: int) -> tuple[list[list[tuple[float, float]]], int]:
    (n_rings,) = _unpack(endian + "I", data, offset)
    offset += 4
    rings = []
    for _ in range(n_rings):
        ring, offset = _read_ring(data, offset, endian, coords)
        rings.append(ring)
    return rings, offset


def _read_geometry(data: bytes, offset: int) -> tuple[list[list[list[tuple[float, float]]]], int]:
    """Read one WKB geometry, returning it as a list of polygons."""
    (byte_order,) = _unpack("B", data, offset)
    endian = "<" if byte_order == 1 else ">"
    (type_word,) = _unpack(endian + "I", data, offset + 1)
    offset += 5
    if type_word & _EWKB_SRID:
        offset += 4  # embedded SRID, which we take from the header instead
    code, coords = _dimensions(type_word)
    if code == _WKB_POLYGON:
        rings, offset = _read_polygon(data, offset, endian, coords)
        return [rings], offset
    if code == _WKB_MULTIPOLYGON:
        (n,) = _unpack(endian + "I", data, offset)
        offset += 4
        polys = []
        for _ in range(n):
            # Each part carries its own byte order and type word.
            part, offset = _read_geometry(data, offset)
            polys.extend(part)
        return polys, offset
    raise GeoPackageError(
        f"WKB geometry type {code} is not a polygon; a building footprint must be "
        "a Polygon or MultiPolygon"
    )


def parse_geometry_blob(blob: bytes) -> dict[str, Any] | None:
    """Decode a GeoPackage geometry BLOB into a GeoJSON-shaped geometry.

    Returns None for the empty geometry, which GeoPackage represents with a flag
    rather than an absent row.
    """
    if len(blob) < 8 or blob[:2] != GPKG_MAGIC:
        raise GeoPackageError("not a GeoPackage geometry blob (bad magic)")
    flags = blob[3]
    if flags & 0x20:
        raise GeoPackageError(
            "extended (ExtendedGeoPackageBinary) geometry is not supported"
        )
    envelope_indicator = (flags >> 1) & 0x07
    if envelope_indicator not in _ENVELOPE_DOUBLES:
        raise GeoPackageError(f"reserved envelope indicator {envelope_indicator}")
    if flags & 0x10:  # empty geometry
        return None
    offset = 8 + 8 * _ENVELOPE_DOUBLES[envelope_indicator]
    polygons, _ = _read_geometry(blob, offset)
    if not polygons:
        return None
    if len(polygons) == 1:
        return {"type": "Polygon", "coordinates": [[list(p) for p in r] for r in polygons[0]]}
    return {
        "type": "MultiPolygon",
        "coordinates": [[[list(p) for p in r] for r in poly] for poly in polygons],
    }


def _feature_tables(conn: sqlite3.Connection) -> list[tuple[str, str]]:
    """(table, geometry column) for every feature table in the file."""
    try:
        rows = conn.execute(
            "SELECT c.table_name, g.column_name FROM gpkg_contents c "
            "JOIN gpkg_geometry_columns g ON g.table_name = c.table_name "
            "WHERE c.data_type = 'features'"
        ).fetchall()
    except sqlite3.DatabaseError as exc:
        raise GeoPackageError(f"not a readable GeoPackage: {exc}") from exc
    return [(str(t), str(c)) for t, c in rows]


def _blob_envelope(blob: bytes) -> tuple[float, float, float, float] | None:
    """The header envelope of a geometry blob as (minx, miny, maxx, maxy).

    GeoPackage stores it as [minx, maxx, miny, maxy] doubles right after the
    8-byte header (OGC 12-128r19, clause 2.1.3), in the header's own byte
    order. None when the writer chose not to include one.
    """
    if len(blob) < 8 or blob[:2] != GPKG_MAGIC:
        return None
    flags = blob[3]
    if ((flags >> 1) & 0x07) == 0 or flags & 0x10:
        return None
    endian = "<" if flags & 0x01 else ">"
    try:
        minx, maxx, miny, maxy = _unpack(endian + "dddd", blob, 8)
    except GeoPackageError:
        return None
    return (minx, miny, maxx, maxy)


def _intersects(a: tuple[float, float, float, float], b: tuple[float, float, float, float]) -> bool:
    return a[0] <= b[2] and a[2] >= b[0] and a[1] <= b[3] and a[3] >= b[1]


def _geometry_bounds(geometry: dict[str, Any]) -> tuple[float, float, float, float] | None:
    """(minx, miny, maxx, maxy) over every ring of a parsed geometry.

    The fallback when a writer omitted the header envelope — Lantmaeteriet's
    municipality files do (flags 0x00 on every blob), so without this the
    bbox filter would keep all 90k+ rows and the row limit would truncate the
    table before it ever reached the site.
    """
    polys = geometry.get("coordinates") or []
    if geometry.get("type") == "Polygon":
        polys = [polys]
    xs: list[float] = []
    ys: list[float] = []
    for poly in polys:
        for ring in poly:
            for point in ring:
                xs.append(point[0])
                ys.append(point[1])
    if not xs:
        return None
    return (min(xs), min(ys), max(xs), max(ys))


def read_features(
    data: bytes,
    *,
    limit: int = 5000,
    bboxes: list[tuple[float, float, float, float]] | None = None,
) -> list[dict[str, Any]]:
    """Every polygon feature in a GeoPackage, as GeoJSON-shaped dicts.

    Attributes travel alongside the geometry so the picker can label a building
    with whatever the source calls it.

    `bboxes` filters by the geometry blobs' header envelopes: a row is kept
    when its envelope intersects ANY of the boxes (each (minx, miny, maxx,
    maxy)). More than one box exists because the file's CRS isn't declared to
    this reader — the caller passes the same window in every frame the file
    could be in, and the frames' coordinate magnitudes are so far apart
    (degrees vs. six-figure metres) that only the matching one can intersect.
    Lantmaeteriet ships one GeoPackage per *municipality*, so without a filter
    a 150 m search would decode a whole city.
    """
    return list(iter_features(data, limit=limit, bboxes=bboxes))


def iter_features(
    data: bytes,
    *,
    limit: int = 5000,
    bboxes: list[tuple[float, float, float, float]] | None = None,
) -> Iterator[dict[str, Any]]:
    if not data.startswith(b"SQLite format 3\x00"):
        raise GeoPackageError(
            "asset is not a GeoPackage (missing the SQLite file header)"
        )
    # sqlite3 opens paths, not buffers, and a GeoPackage is random-access by
    # design, so the bytes land in a temp file for the life of the read.
    fd, path = tempfile.mkstemp(suffix=".gpkg")
    try:
        with os.fdopen(fd, "wb") as fh:
            fh.write(data)
        conn = sqlite3.connect(path)
        try:
            conn.row_factory = sqlite3.Row
            yielded = 0
            for table, geom_col in _feature_tables(conn):
                cursor = conn.execute(f'SELECT * FROM "{table}"')
                for row in cursor:
                    if yielded >= limit:
                        return
                    blob = row[geom_col]
                    if not isinstance(blob, (bytes, bytearray)):
                        continue
                    env = None
                    if bboxes:
                        env = _blob_envelope(bytes(blob))
                        if env is not None and not any(_intersects(env, b) for b in bboxes):
                            continue
                    try:
                        geometry = parse_geometry_blob(bytes(blob))
                    except GeoPackageError:
                        # One unreadable row must not lose the other buildings
                        # in the tile.
                        continue
                    if geometry is None:
                        continue
                    if bboxes and env is None:
                        bounds = _geometry_bounds(geometry)
                        if bounds is not None and not any(_intersects(bounds, b) for b in bboxes):
                            continue
                    props = {
                        k: row[k]
                        for k in row.keys()
                        if k != geom_col and isinstance(row[k], (str, int, float))
                    }
                    yield {
                        "id": str(props.get("objektidentitet") or f"{table}-{yielded}"),
                        "geometry": geometry,
                        "properties": props,
                    }
                    yielded += 1
        finally:
            conn.close()
    finally:
        try:
            os.unlink(path)
        except OSError:  # pragma: no cover - Windows may hold the handle briefly
            pass
