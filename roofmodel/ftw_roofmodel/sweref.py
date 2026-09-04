"""SWEREF 99 TM <-> WGS84 conversion.

Lantmaeteriet publishes everything in SWEREF 99 TM (EPSG:3006) while FTW stores
site location as WGS84 latitude/longitude, so every bounding box we send and
every point cloud we read has to cross this boundary.

This is Lantmaeteriet's own published Gauss conformal projection algorithm
(Krueger series), implemented directly rather than pulled in via pyproj. The
reason is proportion: pyproj ships a full PROJ build for what is, here, exactly
one projection with fixed parameters. The series below is accurate to well under
a millimetre across Sweden, which is several orders of magnitude finer than the
1-2 points/m2 LiDAR it is used to place.

SWEREF 99 TM is a transverse Mercator on GRS 80 with central meridian 15 deg E,
scale factor 0.9996, false easting 500 000 m and false northing 0.
"""

from __future__ import annotations

import math

# GRS 80 ellipsoid.
_A = 6378137.0
_F = 1.0 / 298.257222101

# SWEREF 99 TM projection parameters.
_CENTRAL_MERIDIAN = 15.0
_SCALE = 0.9996
_FALSE_EASTING = 500000.0
_FALSE_NORTHING = 0.0

# Derived constants, computed once.
_E2 = _F * (2.0 - _F)
_N = _F / (2.0 - _F)
_A_HAT = _A / (1.0 + _N) * (1.0 + _N**2 / 4.0 + _N**4 / 64.0)


def _forward_coefficients() -> tuple[float, float, float, float]:
    n = _N
    return (
        n / 2.0 - 2.0 * n**2 / 3.0 + 5.0 * n**3 / 16.0 + 41.0 * n**4 / 180.0,
        13.0 * n**2 / 48.0 - 3.0 * n**3 / 5.0 + 557.0 * n**4 / 1440.0,
        61.0 * n**3 / 240.0 - 103.0 * n**4 / 140.0,
        49561.0 * n**4 / 161280.0,
    )


def _inverse_coefficients() -> tuple[float, float, float, float]:
    n = _N
    return (
        n / 2.0 - 2.0 * n**2 / 3.0 + 37.0 * n**3 / 96.0 - n**4 / 360.0,
        n**2 / 48.0 + n**3 / 15.0 - 437.0 * n**4 / 1440.0,
        17.0 * n**3 / 480.0 - 37.0 * n**4 / 840.0,
        4397.0 * n**4 / 161280.0,
    )


def wgs84_to_sweref99tm(lat: float, lon: float) -> tuple[float, float]:
    """Convert WGS84 degrees to SWEREF 99 TM (northing, easting) in metres."""
    phi = math.radians(lat)
    lam = math.radians(lon)
    lam0 = math.radians(_CENTRAL_MERIDIAN)

    e2 = _E2
    a_coef = e2
    b_coef = (5.0 * e2**2 - e2**3) / 6.0
    c_coef = (104.0 * e2**3 - 45.0 * e2**4) / 120.0
    d_coef = 1237.0 * e2**4 / 1260.0

    sin_phi = math.sin(phi)
    phi_star = phi - sin_phi * math.cos(phi) * (
        a_coef
        + b_coef * sin_phi**2
        + c_coef * sin_phi**4
        + d_coef * sin_phi**6
    )

    dlam = lam - lam0
    xi_p = math.atan2(math.tan(phi_star), math.cos(dlam))
    eta_p = math.atanh(math.cos(phi_star) * math.sin(dlam))

    b1, b2, b3, b4 = _forward_coefficients()
    scaled = _SCALE * _A_HAT
    northing = _FALSE_NORTHING + scaled * (
        xi_p
        + b1 * math.sin(2 * xi_p) * math.cosh(2 * eta_p)
        + b2 * math.sin(4 * xi_p) * math.cosh(4 * eta_p)
        + b3 * math.sin(6 * xi_p) * math.cosh(6 * eta_p)
        + b4 * math.sin(8 * xi_p) * math.cosh(8 * eta_p)
    )
    easting = _FALSE_EASTING + scaled * (
        eta_p
        + b1 * math.cos(2 * xi_p) * math.sinh(2 * eta_p)
        + b2 * math.cos(4 * xi_p) * math.sinh(4 * eta_p)
        + b3 * math.cos(6 * xi_p) * math.sinh(6 * eta_p)
        + b4 * math.cos(8 * xi_p) * math.sinh(8 * eta_p)
    )
    return northing, easting


def sweref99tm_to_wgs84(northing: float, easting: float) -> tuple[float, float]:
    """Convert SWEREF 99 TM (northing, easting) in metres to WGS84 degrees."""
    scaled = _SCALE * _A_HAT
    xi = (northing - _FALSE_NORTHING) / scaled
    eta = (easting - _FALSE_EASTING) / scaled

    d1, d2, d3, d4 = _inverse_coefficients()
    xi_p = (
        xi
        - d1 * math.sin(2 * xi) * math.cosh(2 * eta)
        - d2 * math.sin(4 * xi) * math.cosh(4 * eta)
        - d3 * math.sin(6 * xi) * math.cosh(6 * eta)
        - d4 * math.sin(8 * xi) * math.cosh(8 * eta)
    )
    eta_p = (
        eta
        - d1 * math.cos(2 * xi) * math.sinh(2 * eta)
        - d2 * math.cos(4 * xi) * math.sinh(4 * eta)
        - d3 * math.cos(6 * xi) * math.sinh(6 * eta)
        - d4 * math.cos(8 * xi) * math.sinh(8 * eta)
    )

    phi_star = math.asin(math.sin(xi_p) / math.cosh(eta_p))
    dlam = math.atan2(math.sinh(eta_p), math.cos(xi_p))

    e2 = _E2
    a_star = e2 + e2**2 + e2**3 + e2**4
    b_star = -(7.0 * e2**2 + 17.0 * e2**3 + 30.0 * e2**4) / 6.0
    c_star = (224.0 * e2**3 + 889.0 * e2**4) / 120.0
    d_star = -(4279.0 * e2**4) / 1260.0

    sin_ps = math.sin(phi_star)
    phi = phi_star + sin_ps * math.cos(phi_star) * (
        a_star
        + b_star * sin_ps**2
        + c_star * sin_ps**4
        + d_star * sin_ps**6
    )
    lam = math.radians(_CENTRAL_MERIDIAN) + dlam
    return math.degrees(phi), math.degrees(lam)


def bbox_wgs84_to_sweref99tm(
    min_lat: float, min_lon: float, max_lat: float, max_lon: float
) -> tuple[float, float, float, float]:
    """Project a WGS84 bounding box to a SWEREF 99 TM (minE, minN, maxE, maxN).

    All four corners are projected and the extremes taken, rather than just the
    two diagonal corners: the projection is not axis-aligned, so a box's
    projected edges bow outward and the diagonal-only result would clip.
    """
    corners = [
        wgs84_to_sweref99tm(min_lat, min_lon),
        wgs84_to_sweref99tm(min_lat, max_lon),
        wgs84_to_sweref99tm(max_lat, min_lon),
        wgs84_to_sweref99tm(max_lat, max_lon),
    ]
    northings = [c[0] for c in corners]
    eastings = [c[1] for c in corners]
    return min(eastings), min(northings), max(eastings), max(northings)


def stac_search_bbox(
    lat: float, lon: float, radius_m: float, bbox_epsg: int = 3006
) -> tuple[float, float, float, float]:
    """Bounding box around a site, in the CRS a STAC catalog expects.

    EPSG:3006 (the default) is what Lantmaeteriet's catalog takes; EPSG:4326
    in lon/lat order is what the STAC spec itself mandates, for catalogs that
    follow it. Anything else would need a projection stack this module
    deliberately does not carry.
    """
    south, west, north, east = metre_box_around(lat, lon, radius_m)
    if bbox_epsg == 4326:
        return (west, south, east, north)
    if bbox_epsg == 3006:
        return bbox_wgs84_to_sweref99tm(south, west, north, east)
    raise ValueError(f"unsupported bbox EPSG {bbox_epsg}; use 3006 or 4326")


def metre_box_around(lat: float, lon: float, radius_m: float) -> tuple[float, float, float, float]:
    """Return a WGS84 (min_lat, min_lon, max_lat, max_lon) box of +/- radius_m.

    Built by projecting the centre, stepping in metres in SWEREF 99 TM, and
    unprojecting: doing it that way keeps the box square on the ground instead
    of stretching with latitude the way a naive degree offset would.
    """
    n, e = wgs84_to_sweref99tm(lat, lon)
    south, west = sweref99tm_to_wgs84(n - radius_m, e - radius_m)
    north, east = sweref99tm_to_wgs84(n + radius_m, e + radius_m)
    return south, west, north, east
