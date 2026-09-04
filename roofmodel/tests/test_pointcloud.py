"""The COPC window query must never prune on z.

Lantmäteriet's COPC writer keys octree nodes with a z origin that does not
match the file's own cube (seen live: a node keyed to a slab 1.7 km underground
holding points at +18..+42 m). Pruning by those keys silently drops the dense
deep levels, so the query's vertical range has to cover every slab the cube
could describe.
"""

from ftw_roofmodel.pointcloud import copc_query_z_range


def test_the_query_covers_the_whole_octree_cube():
    for center, half in [(332.51, 5000.005), (0.0, 128.0), (-250.0, 4096.0)]:
        z_lo, z_hi = copc_query_z_range(center, half)
        assert z_lo < center - half
        assert z_hi > center + half


def test_the_query_stays_bounded_for_scaled_int_filters():
    # laspy's post-filter casts scaled bounds to int32; the range must stay
    # proportional to the cube, never an arbitrary huge sentinel.
    z_lo, z_hi = copc_query_z_range(332.51, 5000.005)
    assert z_hi - z_lo < 10 * 5000.005


def test_a_degenerate_cube_still_yields_a_range():
    z_lo, z_hi = copc_query_z_range(100.0, 0.0)
    assert z_lo < 100.0 < z_hi
