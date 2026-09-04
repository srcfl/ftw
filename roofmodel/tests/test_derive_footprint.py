"""Deriving against a picked building footprint."""

from __future__ import annotations

import math

import numpy as np
import pytest

from ftw_roofmodel import pipeline, sweref
from ftw_roofmodel.buildings import BuildingLookupError, clip_to_footprint
from ftw_roofmodel.geotorget import COLLECTION_BUILDINGS, Credentials, StacItem
from ftw_roofmodel.pipeline import RoofModelError, derive
from ftw_roofmodel.segment import segment_roof

STOCKHOLM = (59.33, 18.07)


def roof_face(tilt, azimuth, w, d, origin, density=8, noise=0.04, seed=1):
    """Sample a tilted rectangle. Written as the inverse of what segment.py
    computes, so recovering the tilt is a real test rather than a tautology."""
    rng = np.random.default_rng(seed)
    n = int(w * d * density)
    x = rng.uniform(0, w, n)
    y = rng.uniform(0, d, n)
    az = math.radians(azimuth)
    s = math.tan(math.radians(tilt))
    z = -(x * math.sin(az) + y * math.cos(az)) * s + rng.normal(0, noise, n)
    return np.column_stack([x + origin[0], y + origin[1], z + origin[2]])


def ring(cx, cy, w, d):
    return [(cx, cy), (cx + w, cy), (cx + w, cy + d), (cx, cy + d)]


class FakeClient:
    """Stands in for Geotorget: canned buildings, canned LiDAR."""

    def __init__(self, buildings_payload, points):
        self._buildings = buildings_payload
        self._points = points
        self.searched = []
        self.searched_boxes = []

    def search(self, collection, bbox, limit=20):
        self.searched.append(collection)
        self.searched_boxes.append((collection, bbox))
        if collection == COLLECTION_BUILDINGS:
            return [StacItem(f["id"], collection, {}, None, raw=f) for f in self._buildings]
        return [StacItem("lidar-1", collection, {"data": "http://x/tile.laz"}, None, raw={})]

    def download(self, url):
        return b"laz-bytes"


@pytest.fixture
def scene(monkeypatch):
    """A house and a neighbour that share a ridge orientation.

    The neighbour is the point: an azimuth-180 plane is z = f(y) with no x term,
    so it extends across the whole tile and the two buildings compete for each
    other's returns unless the cloud is clipped first.
    """
    n, e = sweref.wgs84_to_sweref99tm(*STOCKHOLM)
    mine = np.vstack([
        roof_face(35, 180, 12, 6, (e, n, 0), seed=2),
        roof_face(35, 0, 12, 6, (e, n + 6, 4.2), seed=3),
    ])
    neighbour = np.vstack([
        roof_face(35, 180, 12, 6, (e + 40, n, 0), seed=4),
        roof_face(35, 0, 12, 6, (e + 40, n + 6, 4.2), seed=5),
    ])
    cloud = np.vstack([mine, neighbour])

    buildings = [
        {"id": "mine", "geometry": {"type": "Polygon",
         "coordinates": [[list(p) for p in ring(e, n, 12, 12)]]}, "properties": {}},
        {"id": "neighbour", "geometry": {"type": "Polygon",
         "coordinates": [[list(p) for p in ring(e + 40, n, 12, 12)]]}, "properties": {}},
    ]
    monkeypatch.setattr(pipeline, "load_points", lambda data: cloud)
    return FakeClient(buildings, cloud), cloud, (e, n)


def test_derive_clips_to_the_picked_building(scene):
    client, cloud, _ = scene
    model = derive(
        latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
        credentials=Credentials("u", "t"), client=client, building_id="mine",
    )

    b = model["building"]
    assert b["building_id"] == "mine"
    assert b["returns_in_radius"] == len(cloud)
    # Roughly half the tile is the neighbour's, and it must be gone.
    assert b["returns_used"] < b["returns_in_radius"] * 0.6
    assert model["arrays"], "a clipped house still has a south roof"


def test_clipping_recovers_the_true_area_the_neighbour_would_have_stolen(scene):
    """The measurable payoff: coplanar buildings stop eating each other."""
    _, cloud, (e, n) = scene
    truth = 12 * 6 / math.cos(math.radians(35))

    whole_tile = [p for p in segment_roof(cloud, point_density=8.0)
                  if abs(p.azimuth_deg - 180) < 5]
    clipped = [p for p in segment_roof(clip_to_footprint(cloud, ring(e, n, 12, 12)),
                                       point_density=8.0)
               if abs(p.azimuth_deg - 180) < 5]

    assert len(clipped) == 1, "one building has one south face"
    assert clipped[0].area_m2 == pytest.approx(truth, rel=0.10)
    # Unclipped, the two south faces are coplanar and merge into one oversized
    # segment, so the site's own roof cannot be measured at all.
    assert len(whole_tile) != 1 or whole_tile[0].area_m2 > truth * 1.5


def test_derive_without_a_building_id_does_not_search_for_buildings(scene):
    client, _, _ = scene
    model = derive(
        latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
        credentials=Credentials("u", "t"), client=client,
    )
    assert COLLECTION_BUILDINGS not in client.searched
    assert model["building"] is None


def test_derive_rejects_a_building_id_that_is_not_there(scene):
    client, _, _ = scene
    with pytest.raises(RoofModelError) as exc:
        derive(
            latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
            credentials=Credentials("u", "t"), client=client, building_id="not-a-building",
        )
    assert "not found" in str(exc.value)


def test_derive_explains_a_footprint_with_no_returns_on_it(monkeypatch, scene):
    """A building newer than the scan is a real case and needs a real message."""
    client, _, (e, n) = scene
    empty = np.empty((0, 3))
    monkeypatch.setattr(pipeline, "load_points", lambda data: np.vstack([
        roof_face(35, 180, 12, 6, (e + 400, n, 0), seed=9)]))

    with pytest.raises(RoofModelError) as exc:
        derive(
            latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
            credentials=Credentials("u", "t"), client=client, building_id="mine",
        )
    msg = str(exc.value)
    assert "fall on building" in msg and "newer than the scan" in msg


def test_derive_clips_to_a_drawn_footprint(scene):
    """Where the catalog has no building dataset, the operator traces the
    outline by hand — and that must clip exactly like a picked building,
    without any building search happening at all."""
    client, cloud, (e, n) = scene
    corners = []
    for ee, nn in ring(e, n, 12, 12):
        lat, lon = sweref.sweref99tm_to_wgs84(nn, ee)
        corners.append([lon, lat])
    corners.append(list(corners[0]))  # GeoJSON rings close themselves

    model = derive(
        latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
        credentials=Credentials("u", "t"), client=client, footprint=corners,
    )

    assert COLLECTION_BUILDINGS not in client.searched
    b = model["building"]
    assert b["building_id"] == "drawn-footprint"
    assert b["returns_used"] < b["returns_in_radius"] * 0.6
    assert model["arrays"], "a traced house still has a south roof"


def test_a_drawn_footprint_needs_three_corners(scene):
    client, _, _ = scene
    with pytest.raises(Exception) as exc:
        derive(
            latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
            credentials=Credentials("u", "t"), client=client,
            footprint=[[18.06, 59.33], [18.07, 59.33]],
        )
    assert "three corners" in str(exc.value)


def test_the_re_search_reaches_as_far_as_the_picker_did(monkeypatch, scene):
    """A barn 100 m out is inside the picker's 150 m search but outside the
    40 m LiDAR radius. The derive's re-find must use the picker's reach, or
    every such pick dies with "not found near this site"."""
    client, _, _ = scene
    real_search = pipeline.search_buildings
    radii = []

    def windowed(client_, *, latitude, longitude, radius_m, **kw):
        radii.append(radius_m)
        # Model the real windowing: the barn only comes back when the search
        # reaches at least as far as the picker's default.
        if radius_m < 150.0:
            raise BuildingLookupError("nothing inside this window")
        return real_search(client_, latitude=latitude, longitude=longitude,
                           radius_m=radius_m, **kw)

    monkeypatch.setattr(pipeline, "search_buildings", windowed)
    model = derive(
        latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
        credentials=Credentials("u", "t"), client=client, building_id="mine",
    )
    assert model["building"]["building_id"] == "mine"
    assert radii and min(radii) >= 150.0


def test_the_lidar_lookup_centres_on_the_picked_building(scene):
    """The tile search must cover the roof being derived, not the pin: a
    building at the search edge can sit on a different tile."""
    client, _, (e, n) = scene
    derive(
        latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
        credentials=Credentials("u", "t"), client=client, building_id="neighbour",
    )
    lidar_bboxes = [b for c, b in getattr(client, "searched_boxes", [])
                    if c != COLLECTION_BUILDINGS]
    assert lidar_bboxes, "the LiDAR collection was searched"
    west, south, east, north = lidar_bboxes[-1]
    centre_n, centre_e = sweref.wgs84_to_sweref99tm(
        (south + north) / 2.0, (west + east) / 2.0
    )
    # The neighbour's centroid is 40 m east of the site; the bbox centre must
    # follow it rather than stay on the pin.
    assert centre_e == pytest.approx(e + 40 + 6, abs=15.0)
    assert centre_n == pytest.approx(n + 6, abs=15.0)
