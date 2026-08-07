"""vostok shading-integration tests.

vostok is GPL-3.0 and FTW never bundles or installs it, so it is not present in
CI or on a developer machine by default. These tests therefore drive a stub that
implements vostok's documented contract -- read a .sol, write the query points
back with an appended Wh/m2/day column -- which exercises everything on our side
of the boundary: config generation, the two-run ratio, output parsing, clamping,
and every way the tool can be missing or broken.
"""

import os
import stat
import sys
import textwrap

import numpy as np
import pytest

from ftw_roofmodel import shading
from ftw_roofmodel.segment import RoofPlane


def plane(tilt=35.0, azimuth=180.0, height=6.0, area=60.0):
    return RoofPlane(
        tilt_deg=tilt, azimuth_deg=azimuth, area_m2=area,
        point_count=400, mean_height_m=height,
    )


def cloud(n=500, height=6.0, seed=0):
    rng = np.random.default_rng(seed)
    return np.column_stack([
        rng.uniform(0, 12, n),
        rng.uniform(0, 8, n),
        rng.normal(height, 0.2, n),
    ])


def make_stub(tmp_path, *, shaded_value=600.0, open_value=1000.0, exit_code=0, write_output=True):
    """A stand-in vostok: parses the .sol, emits a constant potential.

    Returning different values for the shadowed and open-sky runs is what makes
    the ratio testable without a real solar model.
    """
    script = tmp_path / "stub_vostok.py"
    script.write_text(textwrap.dedent(f"""
        import sys
        cfg = {{}}
        for line in open(sys.argv[1]):
            if '=' in line:
                k, v = line.split('=', 1)
                cfg[k.strip()] = v.strip()
        if {exit_code} != 0:
            sys.stderr.write('stub failure\\n')
            sys.exit({exit_code})
        if not {write_output}:
            sys.exit(0)
        value = {shaded_value} if cfg.get('shadowing') == 'true' else {open_value}
        with open(cfg['output'], 'w') as out:
            for line in open(cfg['query_cloud']):
                parts = line.split()
                if not parts:
                    continue
                out.write(' '.join(parts) + f' {{value}}\\n')
    """).strip(), encoding="ascii")

    # A launcher so the binary looks like an ordinary executable to the adapter.
    if os.name == "nt":
        launcher = tmp_path / "vostok.bat"
        launcher.write_text(f'@echo off\r\n"{sys.executable}" "{script}" %1\r\n', encoding="ascii")
    else:
        launcher = tmp_path / "vostok"
        launcher.write_text(f'#!/bin/sh\nexec "{sys.executable}" "{script}" "$1"\n', encoding="ascii")
        launcher.chmod(launcher.stat().st_mode | stat.S_IEXEC)
    return str(launcher)


# --- absence is the normal case --------------------------------------------

def test_absent_vostok_is_not_an_error():
    """FTW never installs it, so absence must degrade rather than fail."""
    result = shading.compute_shading(
        cloud(), [plane()], latitude=59.33, longitude=18.07,
        binary="definitely-not-installed-vostok",
    )
    assert result.evaluated is False
    assert result.factors == {}


def test_absent_vostok_explains_itself_without_assuming_no_shading():
    result = shading.compute_shading(
        cloud(), [plane()], latitude=59.33, longitude=18.07,
        binary="definitely-not-installed-vostok",
    )
    assert "not installed" in result.reason
    assert "GPL" in result.reason, "the reason should say why FTW does not ship it"
    # An unevaluated plane must read as no derate, not as fully shaded.
    assert result.factor_for(0) == 1.0


def test_available_reports_missing_binary():
    assert shading.available("definitely-not-installed-vostok") is False
    assert shading.find_vostok("definitely-not-installed-vostok") is None


def test_no_planes_short_circuits():
    result = shading.compute_shading(cloud(), [], latitude=59.33, longitude=18.07)
    assert result.evaluated is False
    assert "no roof planes" in result.reason


# --- the ratio -------------------------------------------------------------

def test_shading_factor_is_the_ratio_of_the_two_runs(tmp_path):
    """600 Wh shadowed against 1000 Wh open sky is a factor of 0.6."""
    binary = make_stub(tmp_path, shaded_value=600.0, open_value=1000.0)
    result = shading.compute_shading(
        cloud(), [plane()], latitude=59.33, longitude=18.07, binary=binary,
    )
    assert result.evaluated is True
    assert result.factor_for(0) == pytest.approx(0.6, abs=0.01)


def test_unobstructed_roof_scores_one(tmp_path):
    binary = make_stub(tmp_path, shaded_value=1000.0, open_value=1000.0)
    result = shading.compute_shading(
        cloud(), [plane()], latitude=59.33, longitude=18.07, binary=binary,
    )
    assert result.factor_for(0) == pytest.approx(1.0, abs=0.01)


def test_fully_shaded_roof_scores_zero(tmp_path):
    binary = make_stub(tmp_path, shaded_value=0.0, open_value=1000.0)
    result = shading.compute_shading(
        cloud(), [plane()], latitude=59.33, longitude=18.07, binary=binary,
    )
    assert result.factor_for(0) == 0.0


def test_factor_is_clamped_to_one(tmp_path):
    """Shadowing can only remove irradiance. A ratio above 1 means the two runs
    are not comparable, so it must clamp rather than report a gain."""
    binary = make_stub(tmp_path, shaded_value=1500.0, open_value=1000.0)
    result = shading.compute_shading(
        cloud(), [plane()], latitude=59.33, longitude=18.07, binary=binary,
    )
    assert result.factor_for(0) == 1.0


def test_each_plane_gets_its_own_factor(tmp_path):
    binary = make_stub(tmp_path)
    planes = [plane(height=6.0), plane(azimuth=0.0, height=6.0)]
    result = shading.compute_shading(
        cloud(), planes, latitude=59.33, longitude=18.07, binary=binary,
    )
    assert result.evaluated
    assert set(result.factors) <= {0, 1}
    for f in result.factors.values():
        assert 0.0 <= f <= 1.0


# --- failure handling ------------------------------------------------------

def test_nonzero_exit_is_reported_not_silently_assumed_unshaded(tmp_path):
    binary = make_stub(tmp_path, exit_code=3)
    result = shading.compute_shading(
        cloud(), [plane()], latitude=59.33, longitude=18.07, binary=binary,
    )
    assert result.evaluated is False
    assert "no usable results" in result.reason


def test_missing_output_file_is_handled(tmp_path):
    binary = make_stub(tmp_path, write_output=False)
    result = shading.compute_shading(
        cloud(), [plane()], latitude=59.33, longitude=18.07, binary=binary,
    )
    assert result.evaluated is False


def test_a_plane_with_too_few_points_is_skipped_not_fatal(tmp_path):
    binary = make_stub(tmp_path)
    planes = [plane(height=6.0), plane(height=900.0)]  # second matches nothing
    result = shading.compute_shading(
        cloud(), planes, latitude=59.33, longitude=18.07, binary=binary,
    )
    assert result.evaluated is True
    assert 1 not in result.factors
    assert result.factor_for(1) == 1.0, "a skipped plane must not be derated"


# --- the .sol contract -----------------------------------------------------

def test_query_points_carry_normals(tmp_path):
    """vostok requires a normal on every query point; without one it refuses."""
    captured = {}

    def fake_run(binary, sol_path, timeout_s):
        cfg = dict(
            line.split("=", 1) for line in open(sol_path) if "=" in line
        )
        cfg = {k.strip(): v.strip() for k, v in cfg.items()}
        captured.update(cfg)
        with open(cfg["query_cloud"]) as fh:
            captured["query_first_line"] = fh.readline().split()
        with open(cfg["output"], "w") as out:
            out.write("0 0 0 0 0 1 500\n")

    import ftw_roofmodel.shading as mod
    original = mod._run
    mod._run = fake_run
    try:
        mod.compute_shading(
            cloud(), [plane(tilt=35, azimuth=180)],
            latitude=59.33, longitude=18.07, binary=sys.executable,
        )
    finally:
        mod._run = original

    assert len(captured["query_first_line"]) == 6, "expected x y z nx ny nz"
    nx, ny, nz = (float(v) for v in captured["query_first_line"][3:])
    # A 35-degree south-facing plane: normal tips south (negative northing) and
    # still points up.
    assert nz == pytest.approx(np.cos(np.radians(35)), abs=1e-3)
    assert ny < 0
    assert abs(nx) < 1e-6


def test_sol_carries_the_site_location(tmp_path):
    captured = {}

    def fake_run(binary, sol_path, timeout_s):
        cfg = {}
        for line in open(sol_path):
            if "=" in line:
                k, v = line.split("=", 1)
                cfg[k.strip()] = v.strip()
        captured.setdefault("runs", []).append(cfg)
        with open(cfg["output"], "w") as out:
            out.write("0 0 0 0 0 1 500\n")

    import ftw_roofmodel.shading as mod
    original = mod._run
    mod._run = fake_run
    try:
        mod.compute_shading(
            cloud(), [plane()], latitude=59.33, longitude=18.07,
            binary=sys.executable, voxel_size_m=0.5,
        )
    finally:
        mod._run = original

    runs = captured["runs"]
    assert len(runs) == 2, "one shadowed run and one open-sky run per plane"
    assert {r["shadowing"] for r in runs} == {"true", "false"}
    for r in runs:
        assert float(r["latitude"]) == pytest.approx(59.33)
        assert float(r["longitude"]) == pytest.approx(18.07)
        assert float(r["voxel_size"]) == 0.5


def test_normal_matches_the_segmenter_convention():
    """Round-trip: the normal handed to vostok must describe the same surface
    the segmenter measured, or the shading would be computed for a different
    roof than the one being derated."""
    from ftw_roofmodel.segment import _tilt_azimuth

    for tilt, azimuth in [(35, 180), (20, 90), (45, 270), (60, 135)]:
        n = shading._plane_normal(tilt, azimuth)
        back_tilt, back_az = _tilt_azimuth(np.array(n))
        assert back_tilt == pytest.approx(tilt, abs=0.01)
        assert back_az == pytest.approx(azimuth, abs=0.01)
