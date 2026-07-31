"""CLI entry point: python -m ftw_roofmodel --lat .. --lon ..

Core invokes this as a subprocess and reads roof_model.json from stdout, the
same arm's-length pattern the optimizer uses. Errors go to stderr as JSON so
the caller can surface a reason rather than a stack trace.
"""

from __future__ import annotations

import argparse
import json
import sys

from .geotorget import Credentials, GeotorgetError
from .pipeline import RoofModelError, derive


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="ftw_roofmodel")
    p.add_argument("--lat", type=float, required=True)
    p.add_argument("--lon", type=float, required=True)
    p.add_argument("--username", default="", help="Geotorget username")
    p.add_argument("--token", default="", help="Geotorget token/password")
    p.add_argument("--radius-m", type=float, default=40.0)
    p.add_argument("--packing-factor", type=float, default=0.70)
    p.add_argument("--module-w-per-m2", type=float, default=200.0)
    args = p.parse_args(argv)

    try:
        model = derive(
            latitude=args.lat,
            longitude=args.lon,
            credentials=Credentials(args.username, args.token),
            radius_m=args.radius_m,
            packing_factor=args.packing_factor,
            module_w_per_m2=args.module_w_per_m2,
        )
    except (GeotorgetError, RoofModelError) as exc:
        json.dump({"error": str(exc), "kind": type(exc).__name__}, sys.stderr)
        sys.stderr.write("\n")
        return 1
    json.dump(model, sys.stdout)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
