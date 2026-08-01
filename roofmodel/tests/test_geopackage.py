"""Decoding GeoPackage, the format Lantmaeteriet ships building vectors in.

The fixtures are built byte by byte from the published layouts rather than by
round-tripping the decoder, so a decoder that agrees with itself but not with
the standard still fails here.
"""

from __future__ import annotations

import os
import sqlite3
import struct
import tempfile

import pytest

from ftw_roofmodel.geopackage import (
    GeoPackageError,
    parse_geometry_blob,
    read_features,
)

SWEREF = 3006


def wkb_polygon(rings, *, little=True, z=False):
    """Standard WKB polygon, per OGC 06-103r4 clause 8.2."""
    e = "<" if little else ">"
    code = 1003 if z else 3
    out = struct.pack("B", 1 if little else 0) + struct.pack(e + "I", code)
    out += struct.pack(e + "I", len(rings))
    for ring in rings:
        out += struct.pack(e + "I", len(ring))
        for point in ring:
            out += struct.pack(e + "dd", point[0], point[1])
            if z:
                out += struct.pack(e + "d", point[2] if len(point) > 2 else 0.0)
    return out


def wkb_multipolygon(polygons, *, little=True):
    e = "<" if little else ">"
    out = struct.pack("B", 1 if little else 0) + struct.pack(e + "I", 6)
    out += struct.pack(e + "I", len(polygons))
    for rings in polygons:
        out += wkb_polygon(rings, little=little)
    return out


def gpkg_blob(wkb, *, envelope=None, srs_id=SWEREF, little=True, empty=False):
    """GeoPackage geometry BLOB, per OGC 12-128r19 clause 2.1.3.

    Header is magic(2) + version(1) + flags(1) + srs_id(4), then the envelope,
    then the WKB.
    """
    indicator = 0 if envelope is None else 1
    flags = (1 if little else 0) | (indicator << 1) | (0x10 if empty else 0)
    e = "<" if little else ">"
    header = b"GP" + bytes([0, flags]) + struct.pack(e + "i", srs_id)
    assert len(header) == 8, "the GeoPackage header is 8 bytes before the envelope"
    body = b""
    if envelope is not None:
        body = struct.pack(e + "dddd", *envelope)
        assert len(body) == 32, "an xy envelope is four doubles"
    return header + body + wkb


SQUARE = [[(0.0, 0.0), (10.0, 0.0), (10.0, 10.0), (0.0, 10.0), (0.0, 0.0)]]


def test_reads_a_plain_polygon():
    geom = parse_geometry_blob(gpkg_blob(wkb_polygon(SQUARE)))
    assert geom["type"] == "Polygon"
    assert geom["coordinates"][0][0] == [0.0, 0.0]
    assert geom["coordinates"][0][2] == [10.0, 10.0]
    assert len(geom["coordinates"][0]) == 5


def test_skips_the_envelope_when_one_is_present():
    """The envelope sits between the header and the WKB and must be stepped over."""
    with_env = parse_geometry_blob(
        gpkg_blob(wkb_polygon(SQUARE), envelope=(0.0, 10.0, 0.0, 10.0))
    )
    without = parse_geometry_blob(gpkg_blob(wkb_polygon(SQUARE)))
    assert with_env == without


def test_reads_big_endian_geometry():
    assert parse_geometry_blob(
        gpkg_blob(wkb_polygon(SQUARE, little=False), little=False)
    ) == parse_geometry_blob(gpkg_blob(wkb_polygon(SQUARE)))


def test_reads_3d_polygons_by_stepping_the_z():
    """Building footprints carry heights; the z must not shift the ring."""
    ring = [[(0.0, 0.0, 12.5), (10.0, 0.0, 12.5), (10.0, 10.0, 12.5), (0.0, 0.0, 12.5)]]
    geom = parse_geometry_blob(gpkg_blob(wkb_polygon(ring, z=True)))
    assert geom["coordinates"][0] == [[0.0, 0.0], [10.0, 0.0], [10.0, 10.0], [0.0, 0.0]]


def test_reads_a_multipolygon_as_several_rings():
    other = [[(20.0, 20.0), (30.0, 20.0), (30.0, 30.0), (20.0, 20.0)]]
    geom = parse_geometry_blob(gpkg_blob(wkb_multipolygon([SQUARE, other])))
    assert geom["type"] == "MultiPolygon"
    assert len(geom["coordinates"]) == 2


def test_an_interior_ring_survives():
    """A courtyard is a second ring, and dropping it would inflate the roof."""
    hole = [(2.0, 2.0), (4.0, 2.0), (4.0, 4.0), (2.0, 2.0)]
    geom = parse_geometry_blob(gpkg_blob(wkb_polygon(SQUARE + [hole])))
    assert len(geom["coordinates"]) == 2


def test_empty_geometry_is_none_not_an_error():
    assert parse_geometry_blob(gpkg_blob(b"", empty=True)) is None


def test_rejects_a_blob_that_is_not_a_geopackage_geometry():
    with pytest.raises(GeoPackageError, match="magic"):
        parse_geometry_blob(b"XX" + bytes(20))


def test_refuses_extended_geometry_rather_than_guessing():
    blob = bytearray(gpkg_blob(wkb_polygon(SQUARE)))
    blob[3] |= 0x20
    with pytest.raises(GeoPackageError, match="extended"):
        parse_geometry_blob(bytes(blob))


def test_refuses_a_non_polygon_rather_than_mis_clipping():
    point = struct.pack("B", 1) + struct.pack("<I", 1) + struct.pack("<dd", 1.0, 2.0)
    with pytest.raises(GeoPackageError, match="not a polygon"):
        parse_geometry_blob(gpkg_blob(point))


def build_gpkg(rows, *, table="byggnad", geom_col="geom"):
    """A minimal but standards-shaped GeoPackage file."""
    fd, path = tempfile.mkstemp(suffix=".gpkg")
    os.close(fd)
    conn = sqlite3.connect(path)
    conn.execute(
        "CREATE TABLE gpkg_contents (table_name TEXT, data_type TEXT, srs_id INTEGER)"
    )
    conn.execute(
        "CREATE TABLE gpkg_geometry_columns (table_name TEXT, column_name TEXT, "
        "geometry_type_name TEXT, srs_id INTEGER)"
    )
    conn.execute(
        "INSERT INTO gpkg_contents VALUES (?,?,?)", (table, "features", SWEREF)
    )
    conn.execute(
        "INSERT INTO gpkg_geometry_columns VALUES (?,?,?,?)",
        (table, geom_col, "POLYGON", SWEREF),
    )
    conn.execute(
        f'CREATE TABLE "{table}" (fid INTEGER PRIMARY KEY, objektidentitet TEXT, '
        f'andamal TEXT, "{geom_col}" BLOB)'
    )
    for i, (oid, purpose, blob) in enumerate(rows, start=1):
        conn.execute(
            f'INSERT INTO "{table}" VALUES (?,?,?,?)', (i, oid, purpose, blob)
        )
    conn.commit()
    conn.close()
    with open(path, "rb") as fh:
        data = fh.read()
    os.unlink(path)
    return data


def test_reads_features_out_of_a_real_file():
    data = build_gpkg([
        ("abc-123", "Bostad", gpkg_blob(wkb_polygon(SQUARE))),
        ("def-456", "Komplementbyggnad",
         gpkg_blob(wkb_polygon([[(20.0, 20.0), (30.0, 20.0), (30.0, 30.0), (20.0, 20.0)]]))),
    ])
    feats = read_features(data)
    assert [f["id"] for f in feats] == ["abc-123", "def-456"]
    assert feats[0]["properties"]["andamal"] == "Bostad"
    assert feats[0]["geometry"]["type"] == "Polygon"


def test_one_corrupt_row_does_not_lose_the_others():
    data = build_gpkg([
        ("good-1", "Bostad", gpkg_blob(wkb_polygon(SQUARE))),
        ("bad", "Bostad", b"not a geometry at all"),
        ("good-2", "Bostad",
         gpkg_blob(wkb_polygon([[(20.0, 20.0), (30.0, 20.0), (30.0, 30.0), (20.0, 20.0)]]))),
    ])
    assert [f["id"] for f in read_features(data)] == ["good-1", "good-2"]


def test_a_non_geopackage_payload_says_so():
    with pytest.raises(GeoPackageError, match="SQLite"):
        read_features(b"{\"type\": \"FeatureCollection\"}")


def test_limit_caps_a_city_sized_tile():
    rows = [(f"id-{i}", "Bostad", gpkg_blob(wkb_polygon(SQUARE))) for i in range(20)]
    assert len(read_features(build_gpkg(rows), limit=5)) == 5
