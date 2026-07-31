"""Derive roof geometry for a site: STAC search -> LiDAR -> planes -> arrays.

The output contract is a versioned `roof_model.json`, which is the whole reason
this lives in a separate module rather than inside core: core only ever reads
that document, so the heavy geospatial dependencies, their failure modes and
their update cadence stay on this side of the boundary.

Nothing here is authoritative. The derived arrays *pre-fill* the operator's
editable `weather.pv_arrays`; they are hints, and the numeric editor stays the
final word. A failure produces a clean error and leaves the existing config
untouched.
"""

from __future__ import annotations

import dataclasses
import datetime as dt
from typing import Any

from . import geotorget, pointcloud, sweref
from .buildings import (
    DEFAULT_EAVES_BUFFER_M,
    Building,
    clip_to_footprint,
    search_buildings,
)
from .geotorget import (
    COLLECTION_LIDAR,
    Credentials,
    GeotorgetClient,
    GeotorgetError,
    StacItem,
    newest_capture,
)
from .segment import (
    DEFAULT_MODULE_W_PER_M2,
    DEFAULT_PACKING_FACTOR,
    RoofPlane,
    segment_roof,
)

SCHEMA_VERSION = 1

# How far around the site to pull LiDAR. 40 m comfortably contains a detached
# house and its outbuildings without dragging in the neighbours' roofs.
DEFAULT_RADIUS_M = 40.0

# Roof faces smaller than this are dormers, porches and sheds: real surfaces,
# but not worth proposing as a PV array.
MIN_ARRAY_AREA_M2 = 8.0

# Lantmaeteriet's Laserdata Skog is specified at 1-2 points/m2.
NOMINAL_POINT_DENSITY = 1.5

# Below this, a clipped footprint cannot support a plane fit -- segment_roof
# needs 40 points for a single face, and a roof has at least two.
MIN_POINTS_AFTER_CLIP = 80


class RoofModelError(RuntimeError):
    """Derivation failed."""


@dataclasses.dataclass
class DerivedArray:
    name: str
    kwp: float
    tilt_deg: float
    azimuth_deg: float
    area_m2: float
    segment_id: str
    # Fraction of open-sky irradiation this face actually receives once
    # neighbouring geometry is accounted for. None when shading was not
    # evaluated, which is different from 1.0 ("evaluated, unobstructed").
    shading_factor: float | None = None

    def to_json(self) -> dict[str, Any]:
        out = {
            "name": self.name,
            "kwp": round(self.kwp, 2),
            "tilt_deg": round(self.tilt_deg, 1),
            "azimuth_deg": round(self.azimuth_deg, 1),
            "area_m2": round(self.area_m2, 1),
            "segment_id": self.segment_id,
        }
        if self.shading_factor is not None:
            out["shading_factor"] = round(self.shading_factor, 3)
        return out


def _compass_name(azimuth_deg: float, tilt_deg: float) -> str:
    """Human-readable face name, e.g. "Roof south"."""
    if tilt_deg < 5.0:
        return "Roof flat"
    points = [
        (0, "north"), (45, "north-east"), (90, "east"), (135, "south-east"),
        (180, "south"), (225, "south-west"), (270, "west"), (315, "north-west"),
        (360, "north"),
    ]
    best = min(points, key=lambda p: abs(p[0] - azimuth_deg))
    return f"Roof {best[1]}"


def planes_to_arrays(
    planes: list[RoofPlane],
    *,
    packing_factor: float = DEFAULT_PACKING_FACTOR,
    module_w_per_m2: float = DEFAULT_MODULE_W_PER_M2,
    min_area_m2: float = MIN_ARRAY_AREA_M2,
) -> list[DerivedArray]:
    """Convert roof planes into candidate PV arrays.

    North-facing pitched roofs are dropped: at Swedish latitudes a north face
    at any real pitch yields so little that proposing it as an array would be
    noise in the operator's config. Flat roofs are kept -- they are mounted to
    face south regardless of which way the building points.
    """
    arrays: list[DerivedArray] = []
    used: dict[str, int] = {}
    for idx, plane in enumerate(planes):
        if plane.area_m2 < min_area_m2:
            continue
        if plane.tilt_deg >= 5.0 and (plane.azimuth_deg <= 45.0 or plane.azimuth_deg >= 315.0):
            continue
        name = _compass_name(plane.azimuth_deg, plane.tilt_deg)
        used[name] = used.get(name, 0) + 1
        if used[name] > 1:
            name = f"{name} {used[name]}"
        arrays.append(
            DerivedArray(
                name=name,
                kwp=plane.kwp(packing_factor, module_w_per_m2),
                tilt_deg=plane.tilt_deg,
                azimuth_deg=plane.azimuth_deg,
                area_m2=plane.area_m2,
                segment_id=f"seg-{idx}",
            )
        )
    return arrays


def load_points(data: bytes) -> Any:
    """Decode a whole LAZ/LAS payload into (N, 3) SWEREF 99 TM metres.

    Re-exported from pointcloud so that laspy stays lazily imported: everything
    above -- projection, segmentation, array derivation -- is importable and
    testable without the geospatial stack installed.
    """
    return pointcloud.load_points(data)


def _read_lidar(
    client: GeotorgetClient,
    items: list[StacItem],
    chosen: Building | None,
) -> tuple[Any, str]:
    """Points for the first readable LiDAR asset, and how they were fetched.

    Laserdata Skog is LAZ organised as COPC, so when the operator has already
    picked a building there is no reason to move the rest of a 2.5 km tile
    across the network: the footprint's bounding box is exactly the query COPC
    is built to answer. Everything about that is best-effort -- a plain LAZ
    asset, a host that ignores `Range`, or a laspy without COPC support all
    fall back to reading the tile whole.
    """
    for item in items:
        asset = item.pick(
            geotorget.MEDIA_COPC, geotorget.MEDIA_LAZ, geotorget.MEDIA_LAS
        )
        if asset is None or not asset.href:
            continue
        if chosen is not None and asset.effective_media_type == geotorget.MEDIA_COPC:
            session = getattr(client, "session", None)
            if session is not None:
                bounds = pointcloud.bounds_of(chosen.ring_sweref, DEFAULT_EAVES_BUFFER_M)
                try:
                    return pointcloud.read_copc_window(session, asset.href, bounds), "copc-window"
                except pointcloud.PointCloudError:
                    # A slow success beats a failure; the operator gets their
                    # roof either way, and `fetch` records which path ran.
                    pass
        return load_points(client.download(asset.href)), "whole-tile"
    raise RoofModelError("LiDAR tiles carried no readable point data")


def derive(
    *,
    latitude: float,
    longitude: float,
    credentials: Credentials,
    client: GeotorgetClient | None = None,
    radius_m: float = DEFAULT_RADIUS_M,
    packing_factor: float = DEFAULT_PACKING_FACTOR,
    module_w_per_m2: float = DEFAULT_MODULE_W_PER_M2,
    vostok_binary: str | None = None,
    building_id: str | None = None,
    now: dt.datetime | None = None,
) -> dict[str, Any]:
    """Derive a roof model for one site and return it as a JSON-ready dict.

    Pass `building_id` -- one the operator picked from `search_buildings` -- to
    clip the LiDAR to that footprint before segmenting. Without it the whole
    radius is segmented, which will happily return the neighbour's roof and lets
    coplanar buildings steal each other's points; see buildings.py.
    """
    if client is None:
        client = GeotorgetClient(credentials)

    chosen: Building | None = None
    if building_id:
        candidates = search_buildings(
            client, latitude=latitude, longitude=longitude, radius_m=radius_m
        )
        chosen = next((b for b in candidates if b.building_id == building_id), None)
        if chosen is None:
            raise RoofModelError(
                f"building {building_id!r} was not found near this site; it may "
                "have been picked against a different coordinate"
            )

    south, west, north, east = sweref.metre_box_around(latitude, longitude, radius_m)
    bbox = sweref.bbox_wgs84_to_sweref99tm(south, west, north, east)

    try:
        lidar_items: list[StacItem] = client.search(COLLECTION_LIDAR, bbox)
    except GeotorgetError:
        raise
    if not lidar_items:
        raise RoofModelError(
            f"no LiDAR tiles cover ({latitude:.5f}, {longitude:.5f}); "
            "Lantmaeteriet data is Sweden only"
        )

    points, fetch = _read_lidar(client, lidar_items, chosen)
    if points is None or len(points) == 0:
        raise RoofModelError("LiDAR tiles carried no readable point data")

    total_returns = len(points)
    if chosen is not None:
        points = clip_to_footprint(points, chosen.ring_sweref)
        if len(points) < MIN_POINTS_AFTER_CLIP:
            raise RoofModelError(
                f"only {len(points)} LiDAR returns fall on building "
                f"{chosen.building_id!r}. The footprint and the point cloud may "
                "be from different years, or the building is newer than the scan."
            )

    planes = segment_roof(points, point_density=NOMINAL_POINT_DENSITY)
    arrays = planes_to_arrays(
        planes, packing_factor=packing_factor, module_w_per_m2=module_w_per_m2
    )

    # Optional shadow-aware pass. vostok is GPL-3.0 and separately installed by
    # the operator; absent, every face keeps shading_factor None, which reads as
    # "not evaluated" rather than "unobstructed".
    shading_info: dict[str, Any] = {"evaluated": False, "reason": "not requested"}
    if vostok_binary:
        from .shading import compute_shading

        result = compute_shading(
            points, planes, latitude=latitude, longitude=longitude,
            binary=vostok_binary,
        )
        shading_info = {"evaluated": result.evaluated, "reason": result.reason}
        if result.evaluated:
            # planes_to_arrays drops faces, so map back through segment_id
            # rather than assuming the two lists line up.
            for arr in arrays:
                idx = int(arr.segment_id.split("-")[-1])
                if idx in result.factors:
                    arr.shading_factor = result.factors[idx]

    captured = newest_capture(lidar_items)
    stamp = now or dt.datetime.now(dt.timezone.utc)
    return {
        "schema_version": SCHEMA_VERSION,
        "site": {"latitude": latitude, "longitude": longitude, "radius_m": radius_m},
        "source": {
            "provider": "lantmateriet",
            "collection": COLLECTION_LIDAR,
            "item_count": len(lidar_items),
            "dataset_datetime": captured.isoformat() if captured else None,
            # "copc-window" means only the footprint's neighbourhood was moved
            # across the network, which also makes returns_in_radius below a
            # count over that window rather than over the whole search radius.
            "fetch": fetch,
        },
        "building": {
            "building_id": chosen.building_id,
            "area_m2": round(chosen.area_m2, 1),
            "footprint": chosen.to_geojson()["geometry"],
            "returns_used": len(points),
            "returns_in_radius": total_returns,
        } if chosen is not None else None,
        "arrays": [a.to_json() for a in arrays],
        "planes_found": len(planes),
        "shading": shading_info,
        "captured_at_ms": int(captured.timestamp() * 1000) if captured else None,
        "derived_at_ms": int(stamp.timestamp() * 1000),
    }
