"""Building lookup, frame detection and footprint clipping."""

from __future__ import annotations

import math

import numpy as np
import pytest

from ftw_roofmodel import sweref
from ftw_roofmodel.buildings import (
    Building,
    BuildingLookupError,
    buildings_from_features,
    clip_to_footprint,
    inflate_ring,
    point_in_ring,
    search_buildings,
)
from ftw_roofmodel.geotorget import COLLECTION_BUILDINGS, Credentials, GeotorgetClient

STOCKHOLM = (59.33, 18.07)


def square_ring(cx, cy, side):
    h = side / 2.0
    return [(cx - h, cy - h), (cx + h, cy - h), (cx + h, cy + h), (cx - h, cy + h)]


class FakeResponse:
    def __init__(self, payload, status_code=200):
        self._payload = payload
        self.status_code = status_code

    def json(self):
        return self._payload


class FakeSession:
    """Records what was asked for and replays a canned STAC response."""

    def __init__(self, payload):
        self.payload = payload
        self.posts = []

    def post(self, url, json=None, timeout=None):
        self.posts.append((url, json))
        return FakeResponse(self.payload)


def stac_feature(ring, feature_id="bldg-1", **props):
    return {
        "id": feature_id,
        "collection": COLLECTION_BUILDINGS,
        "geometry": {"type": "Polygon", "coordinates": [[list(p) for p in ring] + [list(ring[0])]]},
        "properties": props,
        "assets": {},
    }


def test_area_and_centroid_of_a_known_square():
    ring = square_ring(674000.0, 6580000.0, 10.0)
    [b] = buildings_from_features(
        [{"geometry": {"type": "Polygon", "coordinates": [ring]}, "id": "sq"}],
        latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
    )
    assert b.area_m2 == pytest.approx(100.0)
    cx, cy = b.centroid_sweref()
    assert (cx, cy) == pytest.approx((674000.0, 6580000.0))


def test_wgs84_rings_are_projected_before_measuring():
    """A ring in degrees must be recognised and converted, not measured raw."""
    lat, lon = STOCKHOLM
    n, e = sweref.wgs84_to_sweref99tm(lat, lon)
    ring_sweref = square_ring(e, n, 12.0)
    ring_wgs84 = []
    for x, y in ring_sweref:
        blat, blon = sweref.sweref99tm_to_wgs84(y, x)  # ring is (E, N)
        ring_wgs84.append([blon, blat])  # GeoJSON is [lon, lat]

    [b] = buildings_from_features(
        [{"geometry": {"type": "Polygon", "coordinates": [ring_wgs84]}, "id": "deg"}],
        latitude=lat, longitude=lon,
    )
    # 12 m square, recovered through a full round trip through degrees.
    assert b.area_m2 == pytest.approx(144.0, abs=0.5)


def test_tiles_and_slivers_are_not_offered_as_buildings():
    lat, lon = STOCKHOLM
    n, e = sweref.wgs84_to_sweref99tm(lat, lon)
    feats = [
        {"geometry": {"type": "Polygon", "coordinates": [square_ring(e, n, 2500.0)]}, "id": "tile"},
        {"geometry": {"type": "Polygon", "coordinates": [square_ring(e, n, 1.0)]}, "id": "sliver"},
        {"geometry": {"type": "Polygon", "coordinates": [square_ring(e, n, 11.0)]}, "id": "house"},
    ]
    got = buildings_from_features(feats, latitude=lat, longitude=lon)
    assert [b.building_id for b in got] == ["house"]


def test_candidates_come_back_nearest_first():
    lat, lon = STOCKHOLM
    n, e = sweref.wgs84_to_sweref99tm(lat, lon)
    feats = [
        {"geometry": {"type": "Polygon", "coordinates": [square_ring(e + 60, n, 10.0)]}, "id": "far"},
        {"geometry": {"type": "Polygon", "coordinates": [square_ring(e + 5, n, 10.0)]}, "id": "near"},
        {"geometry": {"type": "Polygon", "coordinates": [square_ring(e + 25, n, 10.0)]}, "id": "mid"},
    ]
    got = buildings_from_features(feats, latitude=lat, longitude=lon)
    assert [b.building_id for b in got] == ["near", "mid", "far"]
    assert got[0].distance_m < got[1].distance_m < got[2].distance_m


def test_multipolygon_yields_one_candidate_per_part():
    lat, lon = STOCKHOLM
    n, e = sweref.wgs84_to_sweref99tm(lat, lon)
    feat = {
        "id": "pair",
        "geometry": {
            "type": "MultiPolygon",
            "coordinates": [[square_ring(e, n, 10.0)], [square_ring(e + 30, n, 12.0)]],
        },
    }
    got = buildings_from_features([feat], latitude=lat, longitude=lon)
    assert len(got) == 2
    assert len({b.building_id for b in got}) == 2, "parts must not share an id"


def test_search_queries_the_building_collection_and_maps_results():
    lat, lon = STOCKHOLM
    n, e = sweref.wgs84_to_sweref99tm(lat, lon)
    session = FakeSession({"features": [stac_feature(square_ring(e, n, 10.0), "b1")]})
    client = GeotorgetClient(Credentials("u", "t"), session=session)

    got = search_buildings(client, latitude=lat, longitude=lon)

    assert [b.building_id for b in got] == ["b1"]
    (_, body), = session.posts
    assert body["collections"] == [COLLECTION_BUILDINGS]
    # The bbox is WGS84 lon/lat per the STAC spec — verified against the live
    # Lantmaeteriet service, which follows it.
    assert 17.9 < body["bbox"][0] < 18.2, body["bbox"]
    assert 59.2 < body["bbox"][1] < 59.5, body["bbox"]


def test_search_says_what_to_do_when_nothing_comes_back():
    client = GeotorgetClient(Credentials("u", "t"), session=FakeSession({"features": []}))
    with pytest.raises(BuildingLookupError) as exc:
        search_buildings(client, latitude=STOCKHOLM[0], longitude=STOCKHOLM[1])
    assert "Byggnad" in str(exc.value)


def test_point_in_ring_handles_edges_and_outside():
    ring = square_ring(0.0, 0.0, 10.0)
    assert point_in_ring(0.0, 0.0, ring)
    assert point_in_ring(4.9, 4.9, ring)
    assert not point_in_ring(5.1, 0.0, ring)
    assert not point_in_ring(0.0, 99.0, ring)


def test_inflate_ring_grows_the_outline():
    ring = square_ring(0.0, 0.0, 10.0)
    bigger = inflate_ring(ring, 1.0)
    # Corners sit at radius 7.07; pushing 1 m out puts them at 8.07.
    assert math.hypot(*bigger[0]) == pytest.approx(math.hypot(*ring[0]) + 1.0)
    assert all(point_in_ring(x, y, bigger) for x, y in ring)


def test_clip_keeps_the_building_and_drops_the_neighbours():
    rng = np.random.default_rng(3)
    mine = np.column_stack([
        rng.uniform(-4, 4, 400), rng.uniform(-4, 4, 400), rng.uniform(0, 4, 400)])
    theirs = np.column_stack([
        rng.uniform(46, 54, 400), rng.uniform(-4, 4, 400), rng.uniform(0, 4, 400)])
    cloud = np.vstack([mine, theirs])

    kept = clip_to_footprint(cloud, square_ring(0.0, 0.0, 10.0), buffer_m=0.0)

    assert len(kept) == len(mine)
    assert kept[:, 0].max() < 10.0


def test_clip_keeps_the_eaves():
    """Roof returns overhang the wall line; clipping exactly would shave them."""
    ring = square_ring(0.0, 0.0, 10.0)
    eaves = np.array([[5.4, 0.0, 3.0], [-5.4, 0.0, 3.0], [0.0, 5.4, 3.0]])

    assert len(clip_to_footprint(eaves, ring, buffer_m=0.0)) == 0
    assert len(clip_to_footprint(eaves, ring, buffer_m=1.0)) == 3


def test_clip_of_an_empty_cloud_is_empty_not_an_error():
    assert len(clip_to_footprint(np.empty((0, 3)), square_ring(0, 0, 10))) == 0


def test_geojson_feature_is_wgs84_and_closed():
    lat, lon = STOCKHOLM
    n, e = sweref.wgs84_to_sweref99tm(lat, lon)
    b = Building("b1", square_ring(e, n, 10.0), 100.0, 0.0)
    feat = b.to_geojson()

    ring = feat["geometry"]["coordinates"][0]
    assert ring[0] == ring[-1], "GeoJSON rings must close"
    for x, y in ring:
        assert -180 <= x <= 180 and -90 <= y <= 90
    assert feat["properties"]["latitude"] == pytest.approx(lat, abs=1e-3)
    assert feat["properties"]["longitude"] == pytest.approx(lon, abs=1e-3)


def test_both_ring_orders_come_back_at_the_real_site():
    """Axis order must not depend on where the ring came from.

    A GeoPackage stores x=easting first; wgs84_to_sweref99tm returns northing
    first; EPSG's registry declares EPSG:3006 north-first and some exports
    follow it. The first mismatch shipped: GeoPackage-sourced buildings — the
    normal Lantmäteriet case — reported their centroids in the Indian Ocean
    (lat ≈ 4°) with 8 000 km distances, while every test asserted only areas
    and SWEREF centroids, both of which are blind to a consistent swap.
    """
    lat, lon = STOCKHOLM
    n, e = sweref.wgs84_to_sweref99tm(lat, lon)
    ring_en = square_ring(e, n, 10.0)                # as a GeoPackage stores it
    ring_ne = [(y, x) for x, y in ring_en]           # as the EPSG registry says

    for ring in (ring_en, ring_ne):
        [b] = buildings_from_features(
            [{"geometry": {"type": "Polygon", "coordinates": [ring]}, "id": "b"}],
            latitude=lat, longitude=lon,
        )
        feat = b.to_geojson()
        assert feat["properties"]["latitude"] == pytest.approx(lat, abs=1e-3)
        assert feat["properties"]["longitude"] == pytest.approx(lon, abs=1e-3)
        assert b.distance_m < 50.0, (
            f"a building drawn around the site is {b.distance_m:.0f} m away"
        )
