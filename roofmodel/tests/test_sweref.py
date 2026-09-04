"""SWEREF 99 TM projection tests.

The projection has exact analytic properties at the central meridian and the
equator, which pin the parameters without needing a published coordinate table.
Round-trip accuracy then pins the series expansion itself.
"""

import math

import pytest

from ftw_roofmodel.sweref import (
    bbox_wgs84_to_sweref99tm,
    metre_box_around,
    sweref99tm_to_wgs84,
    wgs84_to_sweref99tm,
)


@pytest.mark.parametrize("lat", [0.0, 55.0, 59.33, 63.0, 69.0])
def test_central_meridian_maps_to_false_easting(lat):
    """On the central meridian easting is exactly the false easting, by
    definition. Any error in scale, ellipsoid or meridian shows up here."""
    _, easting = wgs84_to_sweref99tm(lat, 15.0)
    assert easting == pytest.approx(500000.0, abs=1e-6)


def test_equator_on_central_meridian_is_the_projection_origin():
    northing, easting = wgs84_to_sweref99tm(0.0, 15.0)
    assert northing == pytest.approx(0.0, abs=1e-6)
    assert easting == pytest.approx(500000.0, abs=1e-6)


@pytest.mark.parametrize(
    "lat,lon",
    [
        (55.34, 13.15),   # Smygehuk, southernmost Sweden
        (59.33, 18.07),   # Stockholm
        (57.71, 11.97),   # Gothenburg
        (63.83, 20.26),   # Umea
        (67.86, 20.23),   # Kiruna
        (69.06, 20.55),   # Treriksroeset, northernmost
    ],
)
def test_round_trip_is_sub_millimetre(lat, lon):
    """Project and unproject across the full extent of Sweden."""
    n, e = wgs84_to_sweref99tm(lat, lon)
    back_lat, back_lon = sweref99tm_to_wgs84(n, e)
    # 1e-8 degrees is about 1 mm of latitude.
    assert back_lat == pytest.approx(lat, abs=1e-8)
    assert back_lon == pytest.approx(lon, abs=1e-8)


def test_coordinates_land_in_the_expected_range_for_sweden():
    """Sanity-check magnitudes: Swedish SWEREF 99 TM eastings sit inside
    260-920 km and northings inside 6100-7700 km. A transposed or
    wrongly-scaled result would fall far outside."""
    n, e = wgs84_to_sweref99tm(59.33, 18.07)
    assert 260_000 < e < 920_000, e
    assert 6_100_000 < n < 7_700_000, n


def test_east_of_the_meridian_increases_easting():
    _, west = wgs84_to_sweref99tm(59.33, 14.0)
    _, east = wgs84_to_sweref99tm(59.33, 16.0)
    assert west < 500000.0 < east


def test_north_increases_northing():
    south, _ = wgs84_to_sweref99tm(55.0, 15.0)
    north, _ = wgs84_to_sweref99tm(65.0, 15.0)
    assert north > south


def test_one_degree_of_latitude_is_about_111_km():
    a, _ = wgs84_to_sweref99tm(59.0, 15.0)
    b, _ = wgs84_to_sweref99tm(60.0, 15.0)
    # Scaled by k0 = 0.9996 on the central meridian.
    assert 110_000 < (b - a) < 112_000


def test_bbox_uses_all_four_corners():
    """The projection is not axis-aligned, so the projected box must be at
    least as large as the one implied by the two diagonal corners."""
    min_lat, min_lon, max_lat, max_lon = 59.0, 17.0, 60.0, 19.0
    min_e, min_n, max_e, max_n = bbox_wgs84_to_sweref99tm(
        min_lat, min_lon, max_lat, max_lon
    )
    sw_n, sw_e = wgs84_to_sweref99tm(min_lat, min_lon)
    ne_n, ne_e = wgs84_to_sweref99tm(max_lat, max_lon)
    assert min_e <= sw_e and min_n <= sw_n
    assert max_e >= ne_e and max_n >= ne_n
    assert min_e < max_e and min_n < max_n


def test_metre_box_is_square_on_the_ground():
    """A 100 m box must measure 200 m on both axes regardless of latitude --
    the whole reason it is built in projected metres rather than in degrees."""
    for lat in (55.5, 59.33, 68.0):
        south, west, north, east = metre_box_around(lat, 15.0, 100.0)
        sn, se = wgs84_to_sweref99tm(south, west)
        nn, ne = wgs84_to_sweref99tm(north, east)
        assert (ne - se) == pytest.approx(200.0, abs=0.5)
        assert (nn - sn) == pytest.approx(200.0, abs=0.5)


def test_metre_box_brackets_its_centre():
    south, west, north, east = metre_box_around(59.33, 18.07, 50.0)
    assert south < 59.33 < north
    assert west < 18.07 < east


def test_stac_search_bbox_sweref_matches_the_long_form():
    from ftw_roofmodel.sweref import bbox_wgs84_to_sweref99tm, stac_search_bbox

    south, west, north, east = metre_box_around(59.33, 18.07, 40.0)
    assert stac_search_bbox(59.33, 18.07, 40.0) == bbox_wgs84_to_sweref99tm(
        south, west, north, east
    )


def test_stac_search_bbox_wgs84_is_lon_lat_ordered():
    """The STAC spec's bbox is [west, south, east, north] in degrees."""
    from ftw_roofmodel.sweref import stac_search_bbox

    west, south, east, north = stac_search_bbox(59.33, 18.07, 40.0, bbox_epsg=4326)
    assert west < 18.07 < east
    assert south < 59.33 < north


def test_stac_search_bbox_refuses_a_crs_it_cannot_produce():
    from ftw_roofmodel.sweref import stac_search_bbox

    with pytest.raises(ValueError):
        stac_search_bbox(59.33, 18.07, 40.0, bbox_epsg=3857)


def test_degree_box_would_have_been_wrong_at_high_latitude():
    """Guards the reason metre_box_around exists: a fixed degree offset gives
    wildly different ground distances at Malmoe and Kiruna, so anyone tempted to
    simplify it back to degrees has to defeat this test first."""
    span = []
    for lat in (55.5, 68.0):
        # What a naive 0.001-degree longitude offset would span, in metres.
        _, e0 = wgs84_to_sweref99tm(lat, 15.0)
        _, e1 = wgs84_to_sweref99tm(lat, 15.001)
        span.append(e1 - e0)
    assert span[0] / span[1] > 1.5, span
