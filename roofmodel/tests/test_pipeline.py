"""Geotorget/STAC and end-to-end derivation tests.

Lantmaeteriet is credential-gated, so the HTTP layer is exercised against a fake
session rather than the live service. What is verified here is our half of the
contract: the request we send, how we read the response, and how each documented
upstream quirk is handled.
"""

import datetime as dt
import json

import numpy as np
import pytest

from ftw_roofmodel import pipeline, sweref
from ftw_roofmodel.geotorget import (
    COLLECTION_LIDAR,
    Credentials,
    GeotorgetClient,
    GeotorgetError,
    MissingCredentials,
    newest_capture,
)
from ftw_roofmodel.pipeline import RoofModelError, planes_to_arrays
from ftw_roofmodel.segment import RoofPlane
from tests.test_segment import make_plane


class FakeResponse:
    def __init__(self, status_code=200, payload=None, content=b""):
        self.status_code = status_code
        self._payload = payload if payload is not None else {}
        self.content = content

    def json(self):
        return self._payload


class FakeSession:
    """Records requests and replays canned responses."""

    def __init__(self, search=None, asset=b"", status=200):
        self.search_payload = search if search is not None else {"features": []}
        self.asset = asset
        self.status = status
        self.posts = []
        self.gets = []

    def post(self, url, json=None, timeout=None):
        self.posts.append((url, json))
        return FakeResponse(self.status, self.search_payload)

    def get(self, url, timeout=None):
        self.gets.append(url)
        return FakeResponse(self.status, content=self.asset)


def feature(item_id="tile-1", datetime_value="2023-05-04T10:00:00Z", **props):
    p = {"datetime": datetime_value}
    p.update(props)
    return {
        "id": item_id,
        "collection": COLLECTION_LIDAR,
        "properties": p,
        "assets": {"data": {"href": f"https://example.test/{item_id}.laz"}},
    }


CREDS = Credentials("user", "token")


# --- credentials -----------------------------------------------------------

def test_missing_credentials_are_rejected_before_any_request():
    session = FakeSession()
    for creds in (Credentials("", "token"), Credentials("user", ""), Credentials("", "")):
        with pytest.raises(MissingCredentials):
            GeotorgetClient(creds, session=session)
    assert session.posts == [], "must not contact Geotorget without credentials"


def test_open_catalog_needs_no_credentials():
    session = FakeSession(search={"features": []})
    client = GeotorgetClient(
        Credentials("", ""), session=session, base_url="https://stac.example.org"
    )
    client.search(COLLECTION_LIDAR, (0, 0, 1, 1))
    assert session.posts, "anonymous search must reach an open catalog"


def test_open_catalog_still_refuses_half_a_credential():
    for creds in (Credentials("u", ""), Credentials("", "p")):
        with pytest.raises(MissingCredentials):
            GeotorgetClient(
                creds, session=FakeSession(), base_url="https://stac.example.org"
            )


def test_anonymous_rejection_says_to_configure_credentials():
    client = GeotorgetClient(
        Credentials("", ""),
        session=FakeSession(status=401),
        base_url="https://stac.example.org",
    )
    with pytest.raises(MissingCredentials) as exc:
        client.search(COLLECTION_LIDAR, (0, 0, 1, 1))
    assert "stac_username" in str(exc.value)


def test_rejected_credentials_say_what_to_check():
    client = GeotorgetClient(CREDS, session=FakeSession(status=403))
    with pytest.raises(MissingCredentials) as exc:
        client.search(COLLECTION_LIDAR, (0, 0, 1, 1))
    assert "ordered access" in str(exc.value)


def test_server_error_is_not_mistaken_for_an_auth_problem():
    client = GeotorgetClient(CREDS, session=FakeSession(status=500))
    with pytest.raises(GeotorgetError) as exc:
        client.search(COLLECTION_LIDAR, (0, 0, 1, 1))
    assert not isinstance(exc.value, MissingCredentials)


# --- STAC search -----------------------------------------------------------

def test_search_sends_the_collection_and_bbox():
    session = FakeSession(search={"features": [feature()]})
    client = GeotorgetClient(CREDS, session=session)
    items = client.search(COLLECTION_LIDAR, (600000.0, 6500000.0, 600100.0, 6500100.0))

    assert len(items) == 1
    (url, body), = session.posts
    # The client's default root is the live vector catalogue; search is the
    # spec's POST {base}/search on whatever root the client was given.
    assert url.endswith("/stac-vektor/v1/search")
    assert body["collections"] == [COLLECTION_LIDAR]
    assert body["bbox"] == [600000.0, 6500000.0, 600100.0, 6500100.0]


def test_search_speaks_standard_stac_to_a_custom_catalog():
    """Any STAC-conformant catalog works: base_url and collection ids are the
    whole coupling, and search is the spec's POST {base}/search."""
    session = FakeSession(search={"features": []})
    client = GeotorgetClient(CREDS, session=session, base_url="https://stac.example.org/")
    client.search("lidar-pointcloud", (5.0, 50.0, 6.0, 51.0))

    (url, body), = session.posts
    assert url == "https://stac.example.org/search"
    assert body["collections"] == ["lidar-pointcloud"]


def test_capture_date_is_read_from_stac_datetime():
    session = FakeSession(search={"features": [feature(datetime_value="2019-04-02T09:30:00Z")]})
    items = GeotorgetClient(CREDS, session=session).search(COLLECTION_LIDAR, (0, 0, 1, 1))
    assert items[0].captured_at.year == 2019
    assert items[0].captured_at.month == 4


def test_capture_date_falls_back_to_the_laser_strip_datum():
    """Lantmaeteriet is backfilling properties.datetime through 2026; the strip
    `datum` is the ground truth available in the meantime."""
    session = FakeSession(
        search={"features": [feature(datetime_value=None, datum="20180301")]}
    )
    items = GeotorgetClient(CREDS, session=session).search(COLLECTION_LIDAR, (0, 0, 1, 1))
    assert items[0].captured_at is not None
    assert items[0].captured_at.strftime("%Y-%m-%d") == "2018-03-01"


def test_absent_capture_date_is_not_an_error():
    session = FakeSession(search={"features": [feature(datetime_value=None)]})
    items = GeotorgetClient(CREDS, session=session).search(COLLECTION_LIDAR, (0, 0, 1, 1))
    assert items[0].captured_at is None
    assert newest_capture(items) is None


def test_newest_capture_picks_the_latest_known_date():
    session = FakeSession(
        search={
            "features": [
                feature("a", "2018-01-01T00:00:00Z"),
                feature("b", "2021-06-01T00:00:00Z"),
                feature("c", None),
            ]
        }
    )
    items = GeotorgetClient(CREDS, session=session).search(COLLECTION_LIDAR, (0, 0, 1, 1))
    assert newest_capture(items).year == 2021


# --- planes -> arrays ------------------------------------------------------

def test_north_facing_pitched_roofs_are_not_proposed():
    """At Swedish latitudes a north pitch yields too little to be worth adding
    to an operator's config."""
    planes = [
        RoofPlane(tilt_deg=35, azimuth_deg=0, area_m2=60, point_count=200, mean_height_m=6),
        RoofPlane(tilt_deg=35, azimuth_deg=180, area_m2=60, point_count=200, mean_height_m=6),
    ]
    arrays = planes_to_arrays(planes)
    assert len(arrays) == 1
    assert arrays[0].azimuth_deg == 180


def test_flat_roofs_are_kept_regardless_of_building_orientation():
    planes = [RoofPlane(tilt_deg=1.0, azimuth_deg=180, area_m2=90, point_count=300, mean_height_m=9)]
    assert len(planes_to_arrays(planes)) == 1


def test_tiny_faces_are_dropped():
    """Dormers and porches are real surfaces but not candidate arrays."""
    planes = [RoofPlane(tilt_deg=35, azimuth_deg=180, area_m2=3.0, point_count=50, mean_height_m=5)]
    assert planes_to_arrays(planes) == []


def test_arrays_get_readable_and_unique_names():
    planes = [
        RoofPlane(tilt_deg=35, azimuth_deg=180, area_m2=60, point_count=200, mean_height_m=6),
        RoofPlane(tilt_deg=35, azimuth_deg=182, area_m2=40, point_count=150, mean_height_m=6),
        RoofPlane(tilt_deg=35, azimuth_deg=270, area_m2=30, point_count=120, mean_height_m=6),
    ]
    arrays = planes_to_arrays(planes)
    names = [a.name for a in arrays]
    assert len(set(names)) == len(names), names
    assert names[0] == "Roof south"
    assert "Roof west" in names


def test_array_json_matches_the_config_field_names():
    """The document pre-fills weather.pv_arrays, so the keys must line up."""
    planes = [RoofPlane(tilt_deg=35, azimuth_deg=180, area_m2=60, point_count=200, mean_height_m=6)]
    payload = planes_to_arrays(planes)[0].to_json()
    assert set(payload) >= {"name", "rated_w", "tilt_deg", "azimuth_deg"}
    assert payload["rated_w"] > 0


# --- end to end ------------------------------------------------------------

def _patched_points(monkeypatch, cloud):
    monkeypatch.setattr(pipeline, "load_points", lambda data: cloud)


def test_derive_produces_a_versioned_document(monkeypatch):
    gable = np.vstack([
        make_plane(tilt_deg=35, azimuth_deg=180, width=12, depth=6, seed=11),
        make_plane(tilt_deg=35, azimuth_deg=0, width=12, depth=6, origin=(0, 6, -4.2), seed=12),
    ])
    _patched_points(monkeypatch, gable)
    session = FakeSession(search={"features": [feature()]}, asset=b"laz-bytes")
    client = GeotorgetClient(CREDS, session=session)

    model = pipeline.derive(
        latitude=59.33, longitude=18.07, credentials=CREDS, client=client,
        now=dt.datetime(2026, 7, 31, tzinfo=dt.timezone.utc),
    )

    assert model["schema_version"] == pipeline.SCHEMA_VERSION
    assert model["source"]["provider"] == "lantmateriet"
    assert model["captured_at_ms"] > 0
    assert model["derived_at_ms"] == 1785456000000
    # The gable's north face is dropped, so exactly the south face survives.
    assert len(model["arrays"]) == 1
    south = model["arrays"][0]
    assert south["azimuth_deg"] == pytest.approx(180.0, abs=3.0)
    assert south["tilt_deg"] == pytest.approx(35.0, abs=3.0)
    assert south["rated_w"] > 0
    # Must be JSON-serialisable for the subprocess contract.
    json.dumps(model)


def test_derive_searches_the_spec_bbox(monkeypatch):
    _patched_points(monkeypatch, make_plane(tilt_deg=35, azimuth_deg=180))
    session = FakeSession(search={"features": [feature()]}, asset=b"x")
    pipeline.derive(
        latitude=59.33, longitude=18.07, credentials=CREDS,
        client=GeotorgetClient(CREDS, session=session), radius_m=40.0,
    )
    (_, body), = session.posts
    # WGS84 lon/lat per the STAC spec, the live service's frame — and still
    # square on the ground: built by stepping metres in SWEREF and
    # unprojecting, which is what stac_search_bbox does.
    expected = sweref.stac_search_bbox(59.33, 18.07, 40.0, 4326)
    assert list(body["bbox"]) == pytest.approx(list(expected))


def test_derive_uses_both_lantmateriet_roots_by_default(monkeypatch):
    """The live service splits its STAC per product family: buildings on the
    vector root, point clouds on the elevation root. The default derive must
    build a client for each."""
    made = []

    class RecordingClient:
        def __init__(self, credentials, base_url=None, **kw):
            made.append(base_url)

        def search(self, collection, bbox, limit=20):
            return []

    monkeypatch.setattr(pipeline, "GeotorgetClient", RecordingClient)
    with pytest.raises(RoofModelError):
        pipeline.derive(latitude=59.33, longitude=18.07, credentials=CREDS)
    assert made == [
        pipeline.geotorget.DEFAULT_BASE_URL,
        pipeline.geotorget.DEFAULT_LIDAR_BASE_URL,
    ]


def test_derive_uses_one_client_for_a_custom_catalog(monkeypatch):
    """A custom single-root catalog serves both collections itself."""
    made = []

    class RecordingClient:
        def __init__(self, credentials, base_url=None, **kw):
            made.append(base_url)

        def search(self, collection, bbox, limit=20):
            return []

    monkeypatch.setattr(pipeline, "GeotorgetClient", RecordingClient)
    with pytest.raises(RoofModelError):
        pipeline.derive(
            latitude=59.33, longitude=18.07, credentials=CREDS,
            base_url="https://stac.example.org",
        )
    assert made == ["https://stac.example.org"]


def test_derive_outside_sweden_says_so(monkeypatch):
    """No tiles come back for a site Lantmaeteriet does not cover."""
    session = FakeSession(search={"features": []})
    with pytest.raises(RoofModelError) as exc:
        pipeline.derive(
            latitude=-33.87, longitude=151.21, credentials=CREDS,
            client=GeotorgetClient(CREDS, session=session),
        )
    assert "Sweden only" in str(exc.value)


def test_derive_reports_unknown_capture_date_without_failing(monkeypatch):
    _patched_points(monkeypatch, make_plane(tilt_deg=35, azimuth_deg=180))
    session = FakeSession(search={"features": [feature(datetime_value=None)]}, asset=b"x")
    model = pipeline.derive(
        latitude=59.33, longitude=18.07, credentials=CREDS,
        client=GeotorgetClient(CREDS, session=session),
    )
    assert model["captured_at_ms"] is None
    assert model["source"]["dataset_datetime"] is None
    assert model["arrays"], "a missing provenance date must not block the model"
