from __future__ import annotations

import json
import math
import time
from pathlib import Path

import cvxpy as cp
import pytest

from ftw_optimizer.deadline import SolveDeadline
from ftw_optimizer.protocol import ProtocolError
from ftw_optimizer.preference import (
    apply_cvxpy_flatten,
    cost_bound_slack_ore,
    flatten_peaks_enabled,
    named_shortfalls,
)
from ftw_optimizer.worker import handshake, handle


CASES = Path(__file__).parent / "cases"


def test_handshake_advertises_peak_flattening() -> None:
    response = handshake({"type": "handshake", "protocol_version": 1})
    assert response is not None
    assert "preference_flatten_peaks" in response["features"]


def test_flatten_peaks_rejects_a_non_boolean() -> None:
    with pytest.raises(ProtocolError, match="boolean"):
        flatten_peaks_enabled({"flatten_peaks": 1})


def test_cost_bound_slack_is_numerical_noise() -> None:
    assert cost_bound_slack_ore(0.0) <= 1e-7
    assert cost_bound_slack_ore(100.0) < 1e-6
    assert cost_bound_slack_ore(1_000_000.0) < 1e-5


def test_named_shortfalls_ignore_noise() -> None:
    assert named_shortfalls({"home": 0.4, "car": 1200}) == {"car": 1200}


def flatten_request(*, enabled: bool, backend: str) -> dict:
    request = json.loads((CASES / "flatten-equal-price-charge.json").read_text())["request"]
    request = json.loads(json.dumps(request))
    request["request_id"] = f"flatten-{backend}-{enabled}"
    request["settings"]["flatten_peaks"] = enabled
    request["settings"]["shared_backend"] = backend
    return request


@pytest.mark.parametrize("backend", ["highs", "cvxpy"])
def test_flatten_spreads_cost_neutral_grid_charge(backend: str) -> None:
    response = handle(flatten_request(enabled=True, backend=backend))
    assert response["ok"], response
    assert response["solver"]["preference_stage"] == "flattened"
    assert response["solver"]["import_peak_w"] <= 2100
    cheap = response["plan"]["actions"][:2]
    assert sum(action["battery_w"] for action in cheap) >= 3900
    assert all(action["battery_w"] >= 1000 for action in cheap)


@pytest.mark.parametrize("backend", ["highs", "cvxpy"])
def test_flatten_does_not_spend_money(backend: str) -> None:
    flat = handle(flatten_request(enabled=True, backend=backend))
    spiked = handle(flatten_request(enabled=False, backend=backend))
    assert flat["ok"], flat
    assert spiked["ok"], spiked
    assert math.isclose(
        flat["solver"]["objective_ore"],
        spiked["solver"]["objective_ore"],
        abs_tol=1e-3,
    )
    assert flat["solver"]["import_peak_w"] <= spiked["solver"]["import_peak_w"] + 1.0


def test_golden_flatten_case_caps_the_import_peak() -> None:
    case = json.loads((CASES / "flatten-equal-price-charge.json").read_text())
    response = handle(case["request"])
    assert response["ok"] is case["expect"]["ok"], response
    assert response["solver"]["preference_stage"] == case["expect"]["preference_stage"]
    assert response["solver"]["import_peak_w"] <= case["expect"]["max_import_peak_w"]


@pytest.mark.parametrize("backend", ["highs", "cvxpy"])
def test_single_slot_skips_the_preference_solve(backend: str) -> None:
    request = flatten_request(enabled=True, backend=backend)
    request["slots"] = request["slots"][:1]
    request["request_id"] = f"flatten-single-slot-{backend}"
    response = handle(request)
    assert response["ok"], response
    assert response["solver"]["preference_stage"] == "single_slot"


@pytest.mark.parametrize("backend", ["highs", "cvxpy"])
def test_flatten_does_not_cut_profitable_fuse_export(backend: str) -> None:
    request = {
        "schema_version": 1,
        "request_id": f"flatten-keep-export-{backend}",
        "settings": {
            "mode": "passive_arbitrage",
            "solver": "HIGHS",
            "formulation": "relaxed",
            "time_limit_s": 2,
            "mip_rel_gap": 0.001,
            "shared_backend": backend,
            "flatten_peaks": True,
            "export_bonus_ore_kwh": 0,
            "export_fee_ore_kwh": 0,
        },
        "slots": [
            {
                "start_ms": 1 + index * 3_600_000,
                "len_min": 60,
                "price_ore": 100,
                "spot_ore": 50,
                "confidence": 1,
                "pv_w": -4000,
                "load_w": 500,
                "max_import_w": 8000,
                "max_export_w": 100,
            }
            for index in range(2)
        ],
        "storages": [
            {
                "id": "home",
                "capacity_wh": 10000,
                "initial_energy_wh": 9500,
                "min_energy_wh": 1000,
                "max_energy_wh": 9500,
                "max_charge_w": 0,
                "max_discharge_w": 0,
                "charge_efficiency": 1,
                "discharge_efficiency": 1,
                "terminal_price_ore_kwh": 0,
                "cycle_cost_ore_kwh": 0,
            }
        ],
        "flex_loads": [],
        "thermal_loads": [],
        "commercial_constraints": {},
    }
    response = handle(request)
    assert response["ok"], response
    assert response["solver"]["preference_stage"] == "flattened"
    assert math.isclose(response["solver"]["export_peak_w"], 100, abs_tol=1e-3)
    for action in response["plan"]["actions"]:
        assert math.isclose(action["grid_w"], -100, abs_tol=1e-3)
        assert math.isclose(action["battery_w"], 0, abs_tol=1e-3)


def test_expired_deadline_skips_flatten_and_keeps_peaks() -> None:
    grid_import = cp.Variable(2, nonneg=True)
    grid_export = cp.Variable(2, nonneg=True)
    cost_objective = cp.sum(grid_import)
    constraints = [grid_import == [4000, 0], grid_export == [0, 0]]
    problem = cp.Problem(cp.Minimize(cost_objective), constraints)
    problem.solve(solver=cp.HIGHS)

    def boom(_problem: cp.Problem) -> None:
        raise AssertionError("preference solve must not run when the deadline is gone")

    result = apply_cvxpy_flatten(
        cost_problem=problem,
        cost_objective=cost_objective,
        constraints=constraints,
        grid_import=grid_import,
        grid_export=grid_export,
        settings={"flatten_peaks": True, "time_limit_s": 2.0},
        deadline=SolveDeadline(time.perf_counter() - 1.0),
        discrete=False,
        solve_problem=boom,
    )
    assert result.stage == "no_time"
    assert result.import_peak_w == pytest.approx(4000)
    assert result.export_peak_w == pytest.approx(0)


@pytest.mark.parametrize("backend", ["highs", "cvxpy"])
def test_service_report_names_an_unmet_storage_target(backend: str) -> None:
    request = flatten_request(enabled=True, backend=backend)
    request["request_id"] = f"storage-shortfall-{backend}"
    request["storages"][0]["target_energy_wh"] = 20_000
    request["storages"][0]["target_slot"] = 2
    response = handle(request)
    assert response["ok"], response
    report = response["solver"].get("service_report") or {}
    assert report["storage_shortfall_wh"]["home"] >= 10_000


def test_service_report_names_an_unmet_ev_deadline() -> None:
    request = {
        "schema_version": 1,
        "request_id": "ev-shortfall",
        "settings": {
            "mode": "arbitrage",
            "solver": "HIGHS",
            "formulation": "auto",
            "time_limit_s": 2,
            "mip_rel_gap": 0.001,
            "shared_backend": "cvxpy",
        },
        "slots": [
            {
                "start_ms": 1,
                "len_min": 60,
                "price_ore": 20,
                "spot_ore": 0,
                "confidence": 1,
                "pv_w": 0,
                "load_w": 0,
                "max_import_w": 8000,
                "max_export_w": 8000,
            }
        ],
        "storages": [],
        "flex_loads": [
            {
                "id": "car",
                "capacity_wh": 60000,
                "initial_energy_wh": 10000,
                "max_energy_wh": 60000,
                "target_energy_wh": 50000,
                "target_slot": 0,
                "charge_efficiency": 1,
                "max_charge_w": 2000,
                "allowed_steps_w": [0, 2000],
            }
        ],
        "thermal_loads": [],
        "commercial_constraints": {},
    }
    response = handle(request)
    assert response["ok"], response
    report = response["solver"].get("service_report") or {}
    assert report["flex_shortfall_wh"]["car"] >= 37000
    assert response["plan"]["actions"][0]["flex_energy_wh"]["car"] == pytest.approx(
        12000, abs=1
    )
