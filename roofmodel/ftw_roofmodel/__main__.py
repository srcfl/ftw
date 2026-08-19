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
from .geotorget import Credentials, GeotorgetClient, GeotorgetError
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
    p.add_argument("--search-radius-m", type=float, default=DEFAULT_SEARCH_RADIUS_M)
    p.add_argument("--username", default="", help="Geotorget username")
    p.add_argument("--token", default="", help="Geotorget token/password")
    p.add_argument("--radius-m", type=float, default=40.0)
    p.add_argument("--packing-factor", type=float, default=0.70)
    p.add_argument("--module-w-per-m2", type=float, default=200.0)
    p.add_argument(
        "--vostok",
        default="",
        help=(
            "path to a vostok binary for shadow-aware irradiance. vostok is a "
            "separate GPL-3.0 tool that FTW never bundles or installs; omit this "
            "and shading is simply not evaluated."
        ),
    )
    args = p.parse_args(argv)
    credentials = Credentials(args.username, args.token)

    try:
        if args.mode == "buildings":
            client = GeotorgetClient(credentials)
            found = search_buildings(
                client,
                latitude=args.lat,
                longitude=args.lon,
                radius_m=args.search_radius_m,
            )
            payload = {
                "schema_version": SCHEMA_VERSION,
                "site": {"latitude": args.lat, "longitude": args.lon},
                "buildings": [b.to_geojson() for b in found],
            }
        else:
            payload = derive(
                latitude=args.lat,
                longitude=args.lon,
                credentials=credentials,
                radius_m=args.radius_m,
                packing_factor=args.packing_factor,
                module_w_per_m2=args.module_w_per_m2,
                vostok_binary=args.vostok or None,
                building_id=args.building_id or None,
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
