"""The two Lantmaeteriet products end to end, in the formats they ship in.

Both are STAC APIs over one client and one credential. What differs is the
payload: *Byggnad Nedladdning, vektor* is GeoPackage, *Laserdata Nedladdning,
Skog* is LAZ organised as COPC.
"""

from __future__ import annotations

import json
import math

import numpy as np
import pytest

from ftw_roofmodel import pipeline, sweref
from ftw_roofmodel.buildings import BuildingLookupError, search_buildings
from ftw_roofmodel.geotorget import (
    COLLECTION_BUILDINGS,
    MEDIA_COPC,
    MEDIA_GEOJSON,
    MEDIA_GEOPACKAGE,
    MEDIA_LAZ,
    Asset,
    Credentials,
    StacItem,
)
from ftw_roofmodel.pipeline import derive
from ftw_roofmodel.pointcloud import PointCloudError

from .test_geopackage import build_gpkg, gpkg_blob, wkb_polygon

STOCKHOLM = (59.33, 18.07)
N, E = sweref.wgs84_to_sweref99tm(*STOCKHOLM)


def square(cx, cy, w, d):
    return [[(cx, cy), (cx + w, cy), (cx + w, cy + d), (cx, cy + d), (cx, cy)]]


def roof_face(tilt, azimuth, w, d, origin, density=8, noise=0.04, seed=1):
    rng = np.random.default_rng(seed)
    n = int(w * d * density)
    x = rng.uniform(0, w, n)
    y = rng.uniform(0, d, n)
    az = math.radians(azimuth)
    s = math.tan(math.radians(tilt))
    z = -(x * math.sin(az) + y * math.cos(az)) * s + rng.normal(0, noise, n)
    return np.column_stack([x + origin[0], y + origin[1], z + origin[2]])


HOUSE = np.vstack([
    roof_face(35, 180, 12, 6, (E, N, 0), seed=2),
    roof_face(35, 0, 12, 6, (E, N + 6, 4.2), seed=3),
])


class FakeClient:
    """A Geotorget stand-in that serves typed assets."""

    def __init__(self, building_asset=None, lidar_asset=None, payloads=None,
                 session=None):
        self._building_asset = building_asset
        self._lidar_asset = lidar_asset
        self._payloads = payloads or {}
        self.session = session
        self.downloaded: list[str] = []

    def search(self, collection, bbox, limit=20):
        if collection == COLLECTION_BUILDINGS:
            if self._building_asset is None:
                return []
            return [StacItem("tile-b", collection, {"data": self._building_asset}, None,
                             raw={"id": "tile-b"})]
        if self._lidar_asset is None:
            return []
        return [StacItem("tile-l", collection, {"data": self._lidar_asset}, None,
                         raw={"id": "tile-l"})]

    def download(self, url):
        self.downloaded.append(url)
        return self._payloads[url]


def a_geopackage_of_two_buildings():
    return build_gpkg([
        ("house-1", "Bostad", gpkg_blob(wkb_polygon(square(E - 6, N - 3, 12, 12)))),
        ("shed-1", "Komplementbyggnad",
         gpkg_blob(wkb_polygon(square(E + 40, N + 40, 8, 8)))),
    ])


def test_buildings_come_out_of_a_geopackage_asset():
    """The normal Byggnad-vektor path: the item points at a .gpkg tile."""
    url = "https://api.lantmateriet.se/x/byggnad.gpkg"
    client = FakeClient(
        building_asset=Asset(url, MEDIA_GEOPACKAGE),
        payloads={url: a_geopackage_of_two_buildings()},
    )
    found = search_buildings(client, latitude=STOCKHOLM[0], longitude=STOCKHOLM[1])

    assert [b.building_id for b in found] == ["house-1", "shed-1"], "nearest first"
    assert found[0].area_m2 == pytest.approx(144.0, rel=0.01)
    assert found[0].properties["andamal"] == "Bostad"
    assert client.downloaded == [url]


def test_buildings_come_out_of_a_zipped_geopackage_asset():
    """The live Lantmaeteriet shape: one item per municipality whose only
    asset is byggnad_kn<code>.zip with the GeoPackage inside."""
    import io
    import zipfile

    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr("byggnad_kn0180.gpkg", a_geopackage_of_two_buildings())
        zf.writestr("licens.txt", "CC BY 4.0")
    url = "https://dl1.lantmateriet.se/byggnadsverk/byggnad_kn0180.zip"
    client = FakeClient(
        building_asset=Asset(url, "application/zip"),
        payloads={url: buf.getvalue()},
    )
    found = search_buildings(client, latitude=STOCKHOLM[0], longitude=STOCKHOLM[1])

    assert [b.building_id for b in found] == ["house-1", "shed-1"]
    assert client.downloaded == [url]


def test_a_tile_asset_wins_over_the_tile_outline():
    """The live municipality items carry their own geometry — the TILE's
    outline, not a building. With a data asset present, the asset must be
    read and the outline ignored; treating the outline as a building both
    invented a giant footprint and skipped the 90k real ones (found live:
    'found=0' with no error, because the outline failed the area filter)."""
    url = "https://dl1.lantmateriet.se/byggnadsverk/byggnad_kn0180.zip"
    import io
    import zipfile

    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr("byggnad_kn0180.gpkg", a_geopackage_of_two_buildings())
    kommun_outline = {
        "type": "Polygon",
        "coordinates": [[[E - 20000, N - 20000], [E + 20000, N - 20000],
                         [E + 20000, N + 20000], [E - 20000, N + 20000],
                         [E - 20000, N - 20000]]],
    }

    class TileClient(FakeClient):
        def search(self, collection, bbox, limit=20):
            return [StacItem("0180", collection, {"data": Asset(url, "application/zip")},
                             None, raw={"id": "0180", "geometry": kommun_outline})]

    client = TileClient(payloads={url: buf.getvalue()})
    found = search_buildings(client, latitude=STOCKHOLM[0], longitude=STOCKHOLM[1])

    assert [b.building_id for b in found] == ["house-1", "shed-1"]
    assert client.downloaded == [url]


def test_a_municipality_tile_is_filtered_to_the_search_window():
    """One GeoPackage covers a whole municipality — Stockholm's has 90k+
    buildings — so rows outside the search window must be dropped, or the
    reader's row limit truncates the table before it reaches the site.

    Both drop paths matter: a stored header envelope skips the row before
    its WKB is decoded, and the live Lantmaeteriet files write NO envelopes
    (flags 0x00), where the parsed geometry's own bounds must do it."""
    far_e, far_n = E + 5000, N + 5000
    gpkg = build_gpkg([
        # Near the site, no envelope: kept via its parsed bounds.
        ("near-1", "Bostad", gpkg_blob(wkb_polygon(square(E - 6, N - 3, 12, 12)))),
        # Far away with an envelope: dropped before the WKB is decoded.
        ("far-1", "Bostad", gpkg_blob(wkb_polygon(square(far_e, far_n, 12, 12)),
                                      envelope=(far_e, far_e + 12, far_n, far_n + 12))),
        # Far away without an envelope: dropped via its parsed bounds.
        ("far-2", "Bostad", gpkg_blob(wkb_polygon(square(far_e, far_n - 60, 12, 12)))),
    ])
    url = "https://api.lantmateriet.se/x/byggnad_kn0180.gpkg"
    client = FakeClient(building_asset=Asset(url, MEDIA_GEOPACKAGE), payloads={url: gpkg})
    found = search_buildings(client, latitude=STOCKHOLM[0], longitude=STOCKHOLM[1])

    assert [b.building_id for b in found] == ["near-1"]


def test_buildings_come_out_of_a_geojson_asset_too():
    """Some catalogues publish GeoJSON; both are handled by media type."""
    url = "https://api.lantmateriet.se/x/byggnad.geojson"
    doc = {
        "type": "FeatureCollection",
        "features": [{
            "type": "Feature",
            "id": "gj-1",
            "geometry": {"type": "Polygon",
                         "coordinates": [[list(p) for p in square(E - 6, N - 3, 12, 12)[0]]]},
            "properties": {"andamal": "Bostad"},
        }],
    }
    client = FakeClient(
        building_asset=Asset(url, MEDIA_GEOJSON),
        payloads={url: json.dumps(doc).encode()},
    )
    found = search_buildings(client, latitude=STOCKHOLM[0], longitude=STOCKHOLM[1])
    assert [b.building_id for b in found] == ["gj-1"]


def test_an_unreadable_building_tile_says_what_is_wrong():
    url = "https://api.lantmateriet.se/x/byggnad.gpkg"
    client = FakeClient(
        building_asset=Asset(url, MEDIA_GEOPACKAGE),
        payloads={url: b"this is not a database"},
    )
    with pytest.raises(BuildingLookupError, match="could not be read"):
        search_buildings(client, latitude=STOCKHOLM[0], longitude=STOCKHOLM[1])


def test_an_item_with_no_usable_asset_is_skipped_not_fatal():
    client = FakeClient(building_asset=Asset("https://x/preview.png", "image/png"))
    with pytest.raises(BuildingLookupError, match="no building footprints"):
        search_buildings(client, latitude=STOCKHOLM[0], longitude=STOCKHOLM[1])


# --- LiDAR: COPC windowing -------------------------------------------------


@pytest.fixture
def lidar_scene(monkeypatch):
    """A client whose LiDAR asset is COPC, with both read paths instrumented."""
    gpkg_url = "https://api.lantmateriet.se/x/byggnad.gpkg"
    copc_url = "https://api.lantmateriet.se/x/tile.copc.laz"
    client = FakeClient(
        building_asset=Asset(gpkg_url, MEDIA_GEOPACKAGE),
        lidar_asset=Asset(copc_url, MEDIA_COPC),
        payloads={gpkg_url: a_geopackage_of_two_buildings(), copc_url: b"laz-bytes"},
        session=object(),
    )
    calls: dict[str, object] = {}

    def fake_window(session, url, bounds, timeout=60.0):
        calls["window"] = {"url": url, "bounds": bounds}
        return HOUSE

    monkeypatch.setattr(pipeline.pointcloud, "read_copc_window", fake_window)
    monkeypatch.setattr(pipeline, "load_points", lambda data: HOUSE)
    return client, calls, copc_url


def test_a_picked_building_is_read_as_a_copc_window(lidar_scene):
    """The payoff: a footprint costs a window, not a 2.5 km tile."""
    client, calls, copc_url = lidar_scene
    model = derive(
        latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
        credentials=Credentials("u", "t"), client=client, building_id="house-1",
    )

    assert model["source"]["fetch"] == "copc-window"
    assert calls["window"]["url"] == copc_url
    min_x, min_y, max_x, max_y = calls["window"]["bounds"]
    # The footprint is 12 m square around the site, plus the eaves buffer.
    assert min_x == pytest.approx(E - 7, abs=0.5)
    assert max_x == pytest.approx(E + 7, abs=0.5)
    assert copc_url not in client.downloaded, "the tile was never downloaded whole"


def test_without_a_picked_building_the_tile_is_read_whole(lidar_scene):
    """No footprint means no window to ask for."""
    client, calls, copc_url = lidar_scene
    model = derive(
        latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
        credentials=Credentials("u", "t"), client=client,
    )
    assert model["source"]["fetch"] == "whole-tile"
    assert "window" not in calls
    assert copc_url in client.downloaded


def test_a_host_without_range_support_falls_back_to_the_whole_tile(monkeypatch, lidar_scene):
    """Best-effort: the operator still gets their roof, just slower."""
    client, _, copc_url = lidar_scene

    def refuses(session, url, bounds, timeout=60.0):
        raise PointCloudError("the LiDAR host does not support range requests")

    monkeypatch.setattr(pipeline.pointcloud, "read_copc_window", refuses)
    model = derive(
        latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
        credentials=Credentials("u", "t"), client=client, building_id="house-1",
    )
    assert model["source"]["fetch"] == "whole-tile"
    assert copc_url in client.downloaded
    assert model["arrays"], "and the roof still comes out"


def test_a_plain_laz_asset_is_read_whole_even_with_a_building(monkeypatch):
    """Only COPC can be windowed; plain LAZ has no index to seek with."""
    gpkg_url = "https://api.lantmateriet.se/x/byggnad.gpkg"
    laz_url = "https://api.lantmateriet.se/x/tile.laz"
    client = FakeClient(
        building_asset=Asset(gpkg_url, MEDIA_GEOPACKAGE),
        lidar_asset=Asset(laz_url, MEDIA_LAZ),
        payloads={gpkg_url: a_geopackage_of_two_buildings(), laz_url: b"laz-bytes"},
        session=object(),
    )
    monkeypatch.setattr(pipeline, "load_points", lambda data: HOUSE)
    called = []
    monkeypatch.setattr(pipeline.pointcloud, "read_copc_window",
                        lambda *a, **k: called.append(1))

    model = derive(
        latitude=STOCKHOLM[0], longitude=STOCKHOLM[1],
        credentials=Credentials("u", "t"), client=client, building_id="house-1",
    )
    assert model["source"]["fetch"] == "whole-tile"
    assert not called
