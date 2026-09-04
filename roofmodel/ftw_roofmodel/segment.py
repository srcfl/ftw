"""Roof-plane segmentation from a LiDAR point cloud.

Implements the method described in the SPAN paper (Yavuzdogan, *Renewable
Energy* 2023, doi:10.1016/j.renene.2023.119022): iterative RANSAC plane fitting
to pull one roof surface at a time out of the cloud, then DBSCAN over each
plane's inliers to split faces that share a plane but not a location -- the two
halves of a gable on opposite wings of a building fit the same equation and are
not the same roof face.

The method is reimplemented from its description. No code is taken from SPAN's
QGIS plugin, which is GPL; everything here uses numpy and scikit-learn, both
BSD, so this module carries no copyleft obligation.

Coordinates are SWEREF 99 TM metres as (easting, northing, height). Azimuth
follows FTW's convention: 0 = north, 90 = east, 180 = south, 270 = west.
"""

from __future__ import annotations

import dataclasses
import math

import numpy as np

# A roof plane must hold at least this many returns to be believed. Below this
# a "plane" is usually a chimney, an aerial, or three points of noise that
# happen to be collinear.
MIN_PLANE_POINTS = 40

# Surfaces flatter than this are treated as flat roofs: their azimuth is
# meaningless (the normal is essentially vertical, so its horizontal component
# is numerical noise that can point anywhere).
FLAT_TILT_DEG = 5.0

# Roofs steeper than this are walls, dormer cheeks or mis-fits.
MAX_TILT_DEG = 80.0

# Fraction of a roof plane's area that can carry modules once you subtract
# ridges, eaves, chimneys, vents and walkways.
DEFAULT_PACKING_FACTOR = 0.70

# Module DC rating per square metre of module. ~20% efficiency at 1000 W/m2.
DEFAULT_MODULE_W_PER_M2 = 200.0


@dataclasses.dataclass
class RoofPlane:
    """One contiguous roof surface."""

    tilt_deg: float
    azimuth_deg: float
    area_m2: float
    point_count: int
    mean_height_m: float

    def rated_w(
        self,
        packing_factor: float = DEFAULT_PACKING_FACTOR,
        module_w_per_m2: float = DEFAULT_MODULE_W_PER_M2,
    ) -> float:
        """Installable DC capacity for this surface, in watts."""
        return self.area_m2 * packing_factor * module_w_per_m2

    def kwp(
        self,
        packing_factor: float = DEFAULT_PACKING_FACTOR,
        module_w_per_m2: float = DEFAULT_MODULE_W_PER_M2,
    ) -> float:
        """Installable DC capacity for this surface, in kWp (test helper)."""
        return self.rated_w(packing_factor, module_w_per_m2) / 1000.0


def _fit_plane(points: np.ndarray) -> np.ndarray:
    """Least-squares plane through points; returns a unit normal pointing up.

    Uses the smallest singular vector of the mean-centred points, which is the
    total-least-squares fit -- it minimises perpendicular distance rather than
    vertical distance, so a steep roof is not biased the way an ordinary
    z = ax + by + c regression would bias it.
    """
    centred = points - points.mean(axis=0)
    _, _, vh = np.linalg.svd(centred, full_matrices=False)
    normal = vh[-1]
    if normal[2] < 0:
        normal = -normal
    return normal / np.linalg.norm(normal)


def _tilt_azimuth(normal: np.ndarray) -> tuple[float, float]:
    """Convert an upward unit normal to (tilt_deg, azimuth_deg)."""
    tilt = math.degrees(math.acos(max(-1.0, min(1.0, float(normal[2])))))
    if tilt < FLAT_TILT_DEG:
        # Horizontal component is noise at this point; report due south, which
        # is what a flat-roof array is normally mounted to face anyway.
        return tilt, 180.0
    east, north = float(normal[0]), float(normal[1])
    azimuth = math.degrees(math.atan2(east, north)) % 360.0
    return tilt, azimuth


def _convex_hull_area(xy: np.ndarray) -> float:
    """Area of the convex hull of 2D points (monotone chain, shoelace).

    Hand-rolled to avoid a scipy dependency for one small routine. The hull
    overestimates a concave roof outline, so it is only used as a fallback
    when the point count is too low for the density estimate to be stable.
    """
    pts = np.unique(xy, axis=0)
    if len(pts) < 3:
        return 0.0
    order = np.lexsort((pts[:, 1], pts[:, 0]))
    pts = pts[order]

    def cross(o, a, b):
        return (a[0] - o[0]) * (b[1] - o[1]) - (a[1] - o[1]) * (b[0] - o[0])

    lower: list = []
    for p in pts:
        while len(lower) >= 2 and cross(lower[-2], lower[-1], p) <= 0:
            lower.pop()
        lower.append(p)
    upper: list = []
    for p in pts[::-1]:
        while len(upper) >= 2 and cross(upper[-2], upper[-1], p) <= 0:
            upper.pop()
        upper.append(p)
    hull = np.array(lower[:-1] + upper[:-1])
    if len(hull) < 3:
        return 0.0
    x, y = hull[:, 0], hull[:, 1]
    return 0.5 * abs(float(np.dot(x, np.roll(y, 1)) - np.dot(y, np.roll(x, 1))))


def _surface_area(points: np.ndarray, tilt_deg: float, point_density: float | None) -> float:
    """Sloped surface area of a roof face, in m2.

    LiDAR density is quoted per square metre of *ground*, so a known density
    gives the horizontal footprint directly from the point count; dividing by
    cos(tilt) lifts that onto the slope. Without a density we fall back to the
    convex hull of the horizontal projection, which is looser -- it fills in
    L-shapes and courtyards.
    """
    if point_density and point_density > 0:
        horizontal = len(points) / point_density
    else:
        horizontal = _convex_hull_area(points[:, :2])
    cos_t = math.cos(math.radians(min(tilt_deg, MAX_TILT_DEG)))
    if cos_t <= 1e-6:
        return horizontal
    return horizontal / cos_t


def _ransac_plane(
    points: np.ndarray,
    threshold_m: float,
    iterations: int,
    rng: np.random.Generator,
) -> np.ndarray | None:
    """Return a boolean inlier mask for the best plane found, or None.

    Plain RANSAC over point triples. scikit-learn's RANSACRegressor is not used
    because it regresses z on (x, y) and so cannot represent a vertical or
    near-vertical surface, and weights errors vertically rather than
    perpendicular to the plane.
    """
    n = len(points)
    if n < 3:
        return None
    best_mask = None
    best_count = 0
    for _ in range(iterations):
        idx = rng.choice(n, size=3, replace=False)
        a, b, c = points[idx]
        normal = np.cross(b - a, c - a)
        norm = np.linalg.norm(normal)
        if norm < 1e-9:
            continue  # degenerate (collinear) sample
        normal = normal / norm
        distances = np.abs((points - a) @ normal)
        mask = distances < threshold_m
        count = int(mask.sum())
        if count > best_count:
            best_count, best_mask = count, mask
    if best_mask is None or best_count < 3:
        return None
    return best_mask


def segment_roof(
    points: np.ndarray,
    *,
    threshold_m: float = 0.25,
    max_planes: int = 8,
    min_plane_points: int = MIN_PLANE_POINTS,
    cluster_eps_m: float = 1.5,
    point_density: float | None = None,
    ransac_iterations: int = 200,
    seed: int = 0,
) -> list[RoofPlane]:
    """Segment a roof point cloud into planes.

    `points` is an (N, 3) array of SWEREF 99 TM (easting, northing, height).
    Returns planes ordered by descending area. Determinism is deliberate: the
    same cloud must always yield the same arrays, or an operator re-running a
    derive would see the geometry shuffle for no reason.
    """
    from sklearn.cluster import DBSCAN  # imported here to keep import cost off the CLI path

    pts = np.asarray(points, dtype=float)
    if pts.ndim != 2 or pts.shape[1] != 3:
        raise ValueError(f"points must be (N, 3), got {pts.shape}")

    rng = np.random.default_rng(seed)
    remaining = pts
    planes: list[RoofPlane] = []

    for _ in range(max_planes):
        if len(remaining) < min_plane_points:
            break
        mask = _ransac_plane(remaining, threshold_m, ransac_iterations, rng)
        if mask is None or int(mask.sum()) < min_plane_points:
            break
        inliers = remaining[mask]
        remaining = remaining[~mask]

        # One plane equation can describe several disjoint faces. Split them.
        labels = DBSCAN(eps=cluster_eps_m, min_samples=10).fit(inliers[:, :2]).labels_
        for label in sorted(set(labels)):
            if label == -1:
                continue  # DBSCAN noise
            cluster = inliers[labels == label]
            if len(cluster) < min_plane_points:
                continue
            normal = _fit_plane(cluster)
            tilt, azimuth = _tilt_azimuth(normal)
            if tilt > MAX_TILT_DEG:
                continue  # a wall, not a roof
            planes.append(
                RoofPlane(
                    tilt_deg=round(tilt, 1),
                    # Round before normalising: 359.97 rounds to 360.0, which is
                    # the same direction as 0 but reads as an out-of-range value.
                    azimuth_deg=round(azimuth, 1) % 360.0,
                    area_m2=round(_surface_area(cluster, tilt, point_density), 1),
                    point_count=len(cluster),
                    mean_height_m=round(float(cluster[:, 2].mean()), 2),
                )
            )

    planes.sort(key=lambda p: p.area_m2, reverse=True)
    return planes
