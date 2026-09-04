"""CLI entry point: python -m ftw_roofmodel --lat .. --lon ..

Core invokes this as a subprocess and reads roof_model.json from stdout, the
same arm's-length pattern the optimizer uses. Errors go to stderr as JSON so
the caller can surface a reason rather than a stack trace.
"""

from __future__ import annotations

import argparse
import json
import sys

from .buildings import DEFAULT_SEARCH_RADIUS_M, search_buildings
from .geotorget import (
    COLLECTION_BUILDINGS,
    COLLECTION_LIDAR,
    DEFAULT_BASE_URL,
    Credentials,
    GeotorgetClient,
    GeotorgetError,
)
from .pipeline import SCHEMA_VERSION, RoofModelError, derive


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="ftw_roofmodel")
    p.add_argument(
        "--mode",
        choices=("derive", "buildings"),
        default="derive",
        help="'buildings' lists footprints to pick from; 'derive' fits the roof",
    )
    p.add_argument("--lat", type=float, required=True)
    p.add_argument("--lon", type=float, required=True)
    p.add_argument(
        "--building-id",
        default="",
        help="footprint to clip the LiDAR to, from a --mode buildings run",
    )
    p.add_argument(
        "--footprint-json",
        default="",
        help="hand-drawn footprint to clip the LiDAR to, as a JSON array of "
        "[lon, lat] pairs — for catalogs that publish no building dataset. "
        "Wins over --building-id.",
    )
    p.add_argument("--search-radius-m", type=float, default=DEFAULT_SEARCH_RADIUS_M)
    p.add_argument("--username", default="", help="STAC catalog username (Geotorget account)")
    p.add_argument(
        "--password",
        "--token",  # legacy spelling, from when the credential was assumed to be a token
        dest="password",
        default="",
        help="STAC catalog password, sent as HTTP Basic auth (Geotorget account password)",
    )
    p.add_argument("--radius-m", type=float, default=40.0)
    p.add_argument("--packing-factor", type=float, default=0.70)
    p.add_argument("--module-w-per-m2", type=float, default=200.0)
    p.add_argument(
        "--stac-base-url",
        default=DEFAULT_BASE_URL,
        help="STAC API root; search is POST {base}/search (default: Lantmäteriet)",
    )
    p.add_argument(
        "--buildings-collection",
        default=COLLECTION_BUILDINGS,
        help="collection id for building footprints",
    )
    p.add_argument(
        "--lidar-collection",
        default=COLLECTION_LIDAR,
        help="collection id for LiDAR point clouds",
    )
    p.add_argument(
        "--bbox-epsg",
        type=int,
        choices=(3006, 4326),
        default=4326,
        help="CRS of the search bbox: 4326 per the STAC spec (Lantmäteriet "
        "included, verified live), 3006 for a catalog that wants SWEREF",
    )
    args = p.parse_args(argv)
    credentials = Credentials(args.username, args.password)

    try:
        if args.mode == "buildings":
            client = GeotorgetClient(credentials, base_url=args.stac_base_url)
            found = search_buildings(
                client,
                latitude=args.lat,
                longitude=args.lon,
                radius_m=args.search_radius_m,
                collection=args.buildings_collection,
                bbox_epsg=args.bbox_epsg,
            )
            payload = {
                "schema_version": SCHEMA_VERSION,
                "site": {"latitude": args.lat, "longitude": args.lon},
                "buildings": [b.to_geojson() for b in found],
            }
        else:
            footprint = None
            if args.footprint_json:
                try:
                    footprint = json.loads(args.footprint_json)
                except ValueError as exc:
                    json.dump({"error": f"--footprint-json is not valid JSON: {exc}",
                               "kind": "ValueError"}, sys.stderr)
                    sys.stderr.write("\n")
                    return 1
            payload = derive(
                latitude=args.lat,
                longitude=args.lon,
                credentials=credentials,
                radius_m=args.radius_m,
                packing_factor=args.packing_factor,
                module_w_per_m2=args.module_w_per_m2,
                building_id=args.building_id or None,
                footprint=footprint,
                base_url=args.stac_base_url,
                buildings_collection=args.buildings_collection,
                lidar_collection=args.lidar_collection,
                bbox_epsg=args.bbox_epsg,
            )
    except (GeotorgetError, RoofModelError) as exc:
        json.dump({"error": str(exc), "kind": type(exc).__name__}, sys.stderr)
        sys.stderr.write("\n")
        return 1
    json.dump(payload, sys.stdout)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
