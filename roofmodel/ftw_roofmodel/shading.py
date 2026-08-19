"""Optional shadow-aware irradiance via vostok.

Roof segmentation gives each face a tilt and an azimuth, which is enough to
predict yield for an unobstructed roof. It says nothing about the spruce to the
south or the neighbour's gable, and those can cost a real installation far more
than a few degrees of azimuth ever will.

`vostok` ("Voxel Octree Solar Toolkit", 3DGeo Heidelberg) computes per-point
solar potential against voxelised occlusion geometry, so pointing it at the same
LiDAR tile the roof came from yields exactly the missing number.

LICENSING -- read before changing anything here
-----------------------------------------------
vostok is **GPL-3.0**. FTW is not. This module therefore treats it as an
external program invoked at arm's length: a separate process, communicating
through files and a command line, with no linking and no code copied in either
direction. Under the FSF's own mere-aggregation reading that leaves FTW's
licence unaffected.

Two rules keep it that way, and neither is negotiable:

  1. **vostok is never bundled or redistributed with FTW.** The operator installs
     it themselves. Adding it to an image, an installer or a dependency list
     would turn aggregation into distribution.
  2. **FTW never installs it automatically.** No download, no build, no package
     manager invocation. If the binary is absent, shading is skipped.

Absent vostok, every roof face keeps a shading factor of 1.0 and the roof model
says shading was not evaluated -- an honest "unknown", not an assumed "unshaded".
"""

from __future__ import annotations

import dataclasses
import os
import shutil
import subprocess
import tempfile

import numpy as np

DEFAULT_BINARY = "vostok"
DEFAULT_VOXEL_SIZE_M = 1.0
DEFAULT_TIMEOUT_S = 900

# Sampling cap per roof face. vostok cost scales with query points and a roof
# needs a representative sample, not every return.
MAX_QUERY_POINTS_PER_PLANE = 200


class VostokUnavailable(RuntimeError):
    """vostok is not installed, or not runnable."""


class VostokFailed(RuntimeError):
    """vostok ran but did not produce usable output."""


@dataclasses.dataclass
class ShadingResult:
    """Per-plane shading factors, keyed by the plane's index."""

    factors: dict[int, float]
    evaluated: bool
    reason: str = ""

    def factor_for(self, index: int) -> float:
        """Shading factor for a plane; 1.0 (no derate) when not evaluated."""
        return self.factors.get(index, 1.0)


def find_vostok(binary: str = DEFAULT_BINARY) -> str | None:
    """Absolute path to the vostok executable, or None if not installed."""
    if os.path.isabs(binary):
        return binary if os.path.isfile(binary) and os.access(binary, os.X_OK) else None
    return shutil.which(binary)


def available(binary: str = DEFAULT_BINARY) -> bool:
    return find_vostok(binary) is not None


def _plane_normal(tilt_deg: float, azimuth_deg: float) -> tuple[float, float, float]:
    """Upward unit normal for a surface with the given tilt and azimuth.

    Inverse of segment._tilt_azimuth. vostok requires a normal on every query
    point, and the fitted plane normal is a cleaner input than re-deriving one
    per point from noisy neighbours.
    """
    import math

    t = math.radians(tilt_deg)
    a = math.radians(azimuth_deg)
    return (math.sin(t) * math.sin(a), math.sin(t) * math.cos(a), math.cos(t))


def _write_xyz(path: str, points: np.ndarray, normal=None) -> None:
    """Write an ASCII point cloud, with normals when given."""
    with open(path, "w", encoding="ascii") as fh:
        if normal is None:
            for p in points:
                fh.write(f"{p[0]:.3f} {p[1]:.3f} {p[2]:.3f}\n")
        else:
            nx, ny, nz = normal
            for p in points:
                fh.write(f"{p[0]:.3f} {p[1]:.3f} {p[2]:.3f} {nx:.6f} {ny:.6f} {nz:.6f}\n")


def _write_sol(
    path: str,
    *,
    shadow_cloud: str,
    query_cloud: str,
    output: str,
    latitude: float,
    longitude: float,
    voxel_size_m: float,
    year: int,
    shadowing: bool,
) -> None:
    """Write a vostok .sol configuration.

    `shadowing` is the whole trick: the same query points are run twice, once
    against the real occlusion geometry and once with shadowing off. The ratio
    of the two is a shading factor that is independent of vostok's absolute
    units, its sky model and the year chosen -- all of which cancel.
    """
    lines = [
        f"shadow_cloud = {shadow_cloud}",
        f"query_cloud = {query_cloud}",
        f"output = {output}",
        f"voxel_size = {voxel_size_m}",
        f"latitude = {latitude:.6f}",
        f"longitude = {longitude:.6f}",
        f"year = {year}",
        "start_day = 1",
        "end_day = 365",
        "time_step_minutes = 60",
        f"shadowing = {'true' if shadowing else 'false'}",
    ]
    with open(path, "w", encoding="ascii") as fh:
        fh.write("\n".join(lines) + "\n")


def _read_potential(path: str) -> np.ndarray:
    """Read the per-point solar potential column from a vostok output file.

    vostok appends the potential (Wh/m2/day) after the xyz and nxnynz columns,
    so it is the last numeric field on each line. Reading it positionally from
    the end rather than by a fixed index keeps this working whether or not the
    build emits normals it was not given.
    """
    values: list[float] = []
    with open(path, "r", encoding="ascii", errors="ignore") as fh:
        for line in fh:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            parts = line.split()
            try:
                values.append(float(parts[-1]))
            except (ValueError, IndexError):
                continue
    if not values:
        raise VostokFailed(f"no usable values in vostok output {path}")
    return np.asarray(values, dtype=float)


def _run(binary: str, sol_path: str, timeout_s: int) -> None:
    try:
        proc = subprocess.run(
            [binary, sol_path],
            capture_output=True,
            timeout=timeout_s,
            check=False,
        )
    except FileNotFoundError as exc:
        raise VostokUnavailable(f"vostok not found: {binary}") from exc
    except subprocess.TimeoutExpired as exc:
        raise VostokFailed(f"vostok timed out after {timeout_s}s") from exc
    if proc.returncode != 0:
        tail = (proc.stderr or b"").decode("utf-8", "replace").strip()[-400:]
        raise VostokFailed(f"vostok exited {proc.returncode}: {tail}")


def compute_shading(
    points: np.ndarray,
    planes: list,
    *,
    latitude: float,
    longitude: float,
    binary: str = DEFAULT_BINARY,
    voxel_size_m: float = DEFAULT_VOXEL_SIZE_M,
    timeout_s: int = DEFAULT_TIMEOUT_S,
    year: int = 2024,
    max_query_points: int = MAX_QUERY_POINTS_PER_PLANE,
    seed: int = 0,
) -> ShadingResult:
    """Shading factor per roof plane, in [0, 1].

    1.0 means the surface receives everything an unobstructed one would; 0.6
    means neighbouring geometry costs it 40% of its annual irradiation.

    `points` is the full local point cloud, which doubles as the occlusion
    geometry -- the trees and neighbouring roofs that cast the shadows are
    already in the tile the roof was segmented from.

    Never raises for an absent vostok: that is the normal case, and it returns
    an unevaluated result instead so the caller can report "not evaluated"
    rather than silently assuming no shading.
    """
    if not planes:
        return ShadingResult({}, evaluated=False, reason="no roof planes to evaluate")

    resolved = find_vostok(binary)
    if resolved is None:
        return ShadingResult(
            {},
            evaluated=False,
            reason=(
                "vostok is not installed; shading was not evaluated. It is a "
                "separate GPL-3.0 tool that FTW never bundles or installs -- see "
                "https://github.com/3dgeo-heidelberg/vostok"
            ),
        )

    rng = np.random.default_rng(seed)
    pts = np.asarray(points, dtype=float)

    with tempfile.TemporaryDirectory(prefix="ftw-vostok-") as tmp:
        shadow_path = os.path.join(tmp, "shadow.xyz")
        _write_xyz(shadow_path, pts)

        # One query file per plane keeps the mapping from output rows back to
        # planes trivial, and lets a single failed plane be dropped rather than
        # invalidating the whole run.
        query_specs: list[tuple[int, str, int]] = []
        for idx, plane in enumerate(planes):
            sample = _sample_plane_points(pts, plane, rng, max_query_points)
            if sample is None:
                continue
            qpath = os.path.join(tmp, f"query-{idx}.xyz")
            _write_xyz(qpath, sample, normal=_plane_normal(plane.tilt_deg, plane.azimuth_deg))
            query_specs.append((idx, qpath, len(sample)))

        if not query_specs:
            return ShadingResult({}, evaluated=False, reason="no query points could be sampled")

        factors: dict[int, float] = {}
        for idx, qpath, _ in query_specs:
            try:
                shaded = _run_once(
                    resolved, tmp, idx, shadow_path, qpath, latitude, longitude,
                    voxel_size_m, year, timeout_s, shadowing=True,
                )
                open_sky = _run_once(
                    resolved, tmp, idx, shadow_path, qpath, latitude, longitude,
                    voxel_size_m, year, timeout_s, shadowing=False,
                )
            except (VostokFailed, VostokUnavailable):
                continue
            reference = float(np.mean(open_sky))
            if reference <= 0:
                continue
            factor = float(np.mean(shaded)) / reference
            # Shadowing can only remove irradiance. A ratio above 1 means the
            # two runs are not comparable, so clamp rather than report a gain.
            factors[idx] = max(0.0, min(1.0, factor))

        if not factors:
            return ShadingResult({}, evaluated=False, reason="vostok produced no usable results")
        return ShadingResult(factors, evaluated=True)


def _run_once(
    binary: str,
    tmp: str,
    idx: int,
    shadow_path: str,
    query_path: str,
    latitude: float,
    longitude: float,
    voxel_size_m: float,
    year: int,
    timeout_s: int,
    *,
    shadowing: bool,
) -> np.ndarray:
    tag = "shaded" if shadowing else "open"
    out_path = os.path.join(tmp, f"out-{idx}-{tag}.xyz")
    sol_path = os.path.join(tmp, f"run-{idx}-{tag}.sol")
    _write_sol(
        sol_path,
        shadow_cloud=shadow_path,
        query_cloud=query_path,
        output=out_path,
        latitude=latitude,
        longitude=longitude,
        voxel_size_m=voxel_size_m,
        year=year,
        shadowing=shadowing,
    )
    _run(binary, sol_path, timeout_s)
    if not os.path.exists(out_path):
        raise VostokFailed(f"vostok wrote no output for plane {idx} ({tag})")
    return _read_potential(out_path)


def _sample_plane_points(
    pts: np.ndarray, plane, rng: np.random.Generator, limit: int
) -> np.ndarray | None:
    """Pick representative query points lying on one roof plane.

    Planes carry statistics rather than their member points, so the face is
    recovered by selecting returns near its mean height. Coarse, but the query
    points only need to sit on the surface being evaluated.
    """
    if len(pts) == 0:
        return None
    band = 1.0
    mask = np.abs(pts[:, 2] - plane.mean_height_m) < band
    candidates = pts[mask]
    if len(candidates) < 10:
        return None
    if len(candidates) > limit:
        idx = rng.choice(len(candidates), size=limit, replace=False)
        candidates = candidates[idx]
    return candidates
