"""Choosing STAC assets by what they are, and reading them in ranges.

Both Lantmaeteriet products are STAC APIs; they differ in what their items point
at. Byggnad-vektor delivers GeoPackage, Laserdata Skog delivers LAZ organised as
COPC. Selecting on media type rather than on an asset key is what keeps that
difference from becoming a pile of special cases.
"""

from __future__ import annotations

import io

import pytest

from ftw_roofmodel.geotorget import (
    MEDIA_COPC,
    MEDIA_GEOJSON,
    MEDIA_GEOPACKAGE,
    MEDIA_LAZ,
    Asset,
    StacItem,
    _item_from_feature,
    media_type_for,
)
from ftw_roofmodel.pointcloud import HttpRangeFile, PointCloudError, bounds_of


def item(assets):
    return StacItem("i", "c", assets, None, raw={})


def test_a_bare_href_still_gets_a_type_from_its_extension():
    """Tests and simple catalogues pass strings; selection must still work."""
    it = item({"data": "https://x/tile.copc.laz"})
    assert isinstance(it.assets["data"], Asset)
    assert it.pick(MEDIA_COPC).href == "https://x/tile.copc.laz"


def test_copc_is_recognised_before_plain_laz():
    """A COPC file is also a .laz; reading it as one costs the whole tile."""
    assert media_type_for("https://x/y/tile.copc.laz") == MEDIA_COPC
    assert media_type_for("https://x/y/tile.laz") == MEDIA_LAZ


def test_query_strings_do_not_hide_the_extension():
    """Signed download URLs carry tokens after a '?'."""
    assert media_type_for("https://x/tile.gpkg?token=abc&x=1") == MEDIA_GEOPACKAGE


def test_a_declared_type_beats_the_extension():
    """The catalogue knows better than the filename."""
    a = Asset(href="https://x/download", media_type=MEDIA_GEOPACKAGE)
    assert a.effective_media_type == MEDIA_GEOPACKAGE


def test_preference_order_is_honoured():
    it = item({
        "laz": Asset("https://x/t.laz", MEDIA_LAZ),
        "copc": Asset("https://x/t.copc.laz", MEDIA_COPC),
    })
    assert it.pick(MEDIA_COPC, MEDIA_LAZ).effective_media_type == MEDIA_COPC
    assert it.pick(MEDIA_LAZ, MEDIA_COPC).effective_media_type == MEDIA_LAZ


def test_falls_back_to_the_data_role_when_the_type_is_unknown():
    it = item({
        "thumbnail": Asset("https://x/preview.png", "image/png", roles=("thumbnail",)),
        "mystery": Asset("https://x/blob", None, roles=("data",)),
    })
    assert it.pick(MEDIA_COPC).href == "https://x/blob"


def test_a_lone_asset_is_unambiguous_whatever_it_is_called():
    assert item({"whatever": Asset("https://x/blob")}).pick(MEDIA_COPC).href == "https://x/blob"


def test_several_unlabelled_assets_are_refused_rather_than_guessed():
    it = item({"a": Asset("https://x/a"), "b": Asset("https://x/b")})
    assert it.pick(MEDIA_COPC) is None


def test_stac_assets_keep_their_type_and_roles():
    feature = {
        "id": "tile-1",
        "collection": "laserdata-nedladdning-skog",
        "assets": {
            "data": {
                "href": "https://x/t.copc.laz",
                "type": MEDIA_COPC,
                "roles": ["data"],
                "title": "Punktmoln",
            }
        },
        "properties": {},
    }
    parsed = _item_from_feature(feature)
    asset = parsed.pick(MEDIA_COPC)
    assert asset.media_type == MEDIA_COPC
    assert asset.roles == ("data",)
    assert asset.title == "Punktmoln"


def test_geojson_and_geopackage_are_both_selectable():
    it = item({"gj": Asset("https://x/b.geojson"), "gp": Asset("https://x/b.gpkg")})
    assert it.pick(MEDIA_GEOPACKAGE, MEDIA_GEOJSON).href == "https://x/b.gpkg"
    assert it.pick(MEDIA_GEOJSON, MEDIA_GEOPACKAGE).href == "https://x/b.geojson"


# --- range reads ------------------------------------------------------------


class FakeResponse:
    def __init__(self, status, content=b"", headers=None):
        self.status_code = status
        self.content = content
        self.headers = headers or {}


class RangeServer:
    """Serves a byte string over Range, and counts what was actually moved."""

    def __init__(self, body: bytes, *, supports_range: bool = True):
        self.body = body
        self.supports_range = supports_range
        self.requests: list[str] = []

    def head(self, url, timeout=None):
        return FakeResponse(200, headers={"Content-Length": str(len(self.body))})

    def get(self, url, headers=None, timeout=None):
        rng = (headers or {}).get("Range")
        if not self.supports_range or not rng:
            self.requests.append("full")
            return FakeResponse(200, self.body)
        self.requests.append(rng)
        spec = rng.split("=", 1)[1]
        start, end = spec.split("-")
        lo = int(start)
        hi = min(int(end), len(self.body) - 1)
        return FakeResponse(206, self.body[lo : hi + 1])


BODY = bytes(range(256)) * 40  # 10 240 bytes, every offset distinguishable


def test_reads_a_window_without_moving_the_whole_file():
    server = RangeServer(BODY)
    fh = HttpRangeFile(server, "https://x/t.copc.laz", chunk_bytes=512)
    fh.seek(1000)
    assert fh.read(16) == BODY[1000:1016]
    assert fh.bytes_fetched == 512, "one chunk, not the whole file"
    assert len(server.requests) == 1


def test_seek_and_tell_track_the_position():
    fh = HttpRangeFile(RangeServer(BODY), "https://x/t", chunk_bytes=64)
    assert fh.seek(100) == 100 and fh.tell() == 100
    fh.read(10)
    assert fh.tell() == 110
    assert fh.seek(-10, io.SEEK_END) == len(BODY) - 10
    assert fh.seek(5, io.SEEK_CUR) == len(BODY) - 5


def test_a_second_read_inside_the_chunk_costs_no_request():
    server = RangeServer(BODY)
    fh = HttpRangeFile(server, "https://x/t", chunk_bytes=1024)
    fh.seek(0)
    fh.read(8)
    before = len(server.requests)
    fh.seek(64)
    assert fh.read(8) == BODY[64:72]
    assert len(server.requests) == before, "the chunk was already held"


def test_reading_across_the_chunk_boundary_fetches_again():
    server = RangeServer(BODY)
    fh = HttpRangeFile(server, "https://x/t", chunk_bytes=128)
    fh.seek(0)
    assert fh.read(8) == BODY[0:8]
    fh.seek(4096)
    assert fh.read(8) == BODY[4096:4104]
    assert len(server.requests) == 2


def test_a_host_that_ignores_range_is_detected_not_trusted():
    """A 200 means the whole body arrived; treating it as partial corrupts."""
    fh = HttpRangeFile(RangeServer(BODY, supports_range=False), "https://x/t")
    fh.seek(10)
    with pytest.raises(PointCloudError, match="range requests"):
        fh.read(4)


def test_a_host_that_will_not_report_a_size_is_refused():
    class NoLength:
        def head(self, url, timeout=None):
            return FakeResponse(200, headers={})

    with pytest.raises(PointCloudError, match="size"):
        HttpRangeFile(NoLength(), "https://x/t").size


def test_readinto_fills_the_buffer():
    fh = HttpRangeFile(RangeServer(BODY), "https://x/t", chunk_bytes=256)
    fh.seek(32)
    buf = bytearray(16)
    assert fh.readinto(buf) == 16
    assert bytes(buf) == BODY[32:48]


def test_bounds_pad_the_footprint_so_the_eaves_survive():
    ring = [(100.0, 200.0), (110.0, 200.0), (110.0, 220.0), (100.0, 220.0)]
    assert bounds_of(ring, 1.0) == (99.0, 199.0, 111.0, 221.0)
