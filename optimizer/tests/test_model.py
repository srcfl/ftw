from __future__ import annotations

import copy
import json
import math
import threading
import time

import cvxpy as cp
import numpy as np
import pytest

from ftw_optimizer.deadline import SolveDeadlineExceeded
from ftw_optimizer.direct_highs import DirectHighsError, _remaining_time_s
from ftw_optimizer.multistage import clear_multistage_cache
from ftw_optimizer.model import (
    OPTIMAL_STATUSES,
    _arbitrage_spread_ore_kwh,
    _canonicalize_storage_payload,
    _pv_charge_bonus_ore_kwh,
    _pv_curtail_output,
    _requires_direction_binary,
    _storage_relaxation_is_unsafe,
)
from ftw_optimizer.protocol import ProtocolError
from ftw_optimizer.scenario_tree import (
    Scenario,
    build_scenario_tree,
    decision_blocks,
    reduce_scenarios,
)
from ftw_optimizer.worker import handle, handshake


def test_pv_charge_bonus_matches_go_dp_mode_gate() -> None:
    settings = {"pv_charge_bonus_ore_kwh": 30}
    assert _pv_charge_bonus_ore_kwh(settings, "passive_arbitrage") == 30
    assert _pv_charge_bonus_ore_kwh(settings, "arbitrage") == 0
    assert _pv_charge_bonus_ore_kwh(settings, "self_consumption") == 0
    assert _pv_charge_bonus_ore_kwh(settings, "cheap_charge") == 0


def test_pv_curtail_output_distinguishes_zero_cap_from_release() -> None:
    limit, active = _pv_curtail_output(-5000, 0)
    assert (limit, active) == (0.0, False)
    limit, active = _pv_curtail_output(-5000, 5000)
    assert active is True
    assert limit == 0.0
    limit, active = _pv_curtail_output(-5000, 2000)
    assert active is True
    assert limit == 3000.0


def test_cvxpy_user_limit_is_not_an_accepted_solution() -> None:
    assert cp.OPTIMAL in OPTIMAL_STATUSES
    assert cp.OPTIMAL_INACCURATE in OPTIMAL_STATUSES
    assert cp.USER_LIMIT not in OPTIMAL_STATUSES


def test_worker_handshake_exposes_module_contract() -> None:
    response = handshake({"type": "handshake", "protocol_version": 1})
    assert response is not None
    assert response["name"] == "ftw-optimizer"
    assert response["protocol_version"] == 1
    assert {
        "champion",
        "recourse",
        "multistage",
        "commercial_constraints_v1",
    }.issubset(response["features"])


def test_commercial_constraints_hold_reserve_backup_and_demand_peak() -> None:
    request = base_request()
    request["slots"] = [
        {
            "start_ms": 1,
            "len_min": 60,
            "price_ore": 100,
            "spot_ore": 20,
            "confidence": 1,
            "pv_w": 0,
            "load_w": 8000,
            "max_import_w": 10000,
            "max_export_w": 10000,
        },
        {
            "start_ms": 3600001,
            "len_min": 60,
            "price_ore": 100,
            "spot_ore": 20,
            "confidence": 1,
            "pv_w": 0,
            "load_w": 8000,
            "max_import_w": 10000,
            "max_export_w": 10000,
        },
    ]
    request["storages"][0].update(
        {
            "initial_energy_wh": 8000,
            "min_energy_wh": 1000,
            "max_energy_wh": 9000,
            "terminal_price_ore_kwh": 0,
            "cycle_cost_ore_kwh": 0,
            "throughput_cost_ore_kwh": 1,
        }
    )
    request["commercial_constraints"] = {
        "version": "srcful-commercial-v1",
        "reserve_up_w": [2000, 2000],
        "reserve_down_w": [0, 0],
        "required_up_wh": [0, 0],
        "required_down_wh": [0, 0],
        "local_uncertainty_up_wh": [0, 0],
        "local_uncertainty_down_wh": [0, 0],
        "backup_min_usable_energy_wh": [4000, 4000],
        "load_low_w": [8000, 8000],
        "load_high_w": [8000, 8000],
        "pv_low_w": [0, 0],
        "pv_high_w": [0, 0],
        "allow_pv_curtailment": False,
        "demand_charge": {
            "rate_ore_per_kw": 1000,
            "billing_peak_so_far_w": 0,
            "active_window": [True, True],
        },
    }
    response = handle(request)
    assert response["ok"], response
    actions = response["plan"]["actions"]
    assert all(action["battery_w"] >= -3000 - 2 for action in actions)
    assert all(
        action["storage_energy_wh"]["home"] >= 5000 - 2
        for action in actions
    )
    assert max(action["grid_w"] for action in actions) < 8000
    assert (
        response["solver"]["objective_breakdown_ore"][
            "demand_charge_increment"
        ]
        > 0
    )


def test_commercial_constraints_reject_negative_reserve() -> None:
    request = base_request()
    request["commercial_constraints"] = {
        "version": "srcful-commercial-v1",
        "reserve_up_w": [-1, 0],
    }
    response = handle(request)
    assert not response["ok"]
    assert response["error"]["code"] == "invalid_request"
    assert "non-negative" in response["error"]["message"]


def base_request() -> dict:
    return {
        "schema_version": 1,
        "request_id": "test-1",
        "settings": {
            "mode": "arbitrage",
            "solver": "HIGHS",
            "formulation": "auto",
            "time_limit_s": 2,
            "mip_rel_gap": 0.001,
            "export_bonus_ore_kwh": 0,
            "export_fee_ore_kwh": 0,
        },
        "slots": [
            {"start_ms": 1, "len_min": 60, "price_ore": 20, "spot_ore": 10, "confidence": 1, "pv_w": 0, "load_w": 500, "max_import_w": 8000, "max_export_w": 8000},
            {"start_ms": 3600001, "len_min": 60, "price_ore": 300, "spot_ore": 240, "confidence": 1, "pv_w": 0, "load_w": 2500, "max_import_w": 8000, "max_export_w": 8000},
        ],
        "storages": [
            {
                "id": "home",
                "capacity_wh": 10000,
                "initial_energy_wh": 2000,
                "min_energy_wh": 1000,
                "max_energy_wh": 9500,
                "max_charge_w": 5000,
                "max_discharge_w": 5000,
                "charge_efficiency": 0.95,
                "discharge_efficiency": 0.95,
                "terminal_price_ore_kwh": 20,
                "cycle_cost_ore_kwh": 5,
            }
        ],
        "flex_loads": [],
        "thermal_loads": [],
    }


def mode_spread_request(mode: str, backend: str, spread: float) -> dict:
    request = base_request()
    request["request_id"] = f"spread-{mode}-{backend}-{spread}"
    request["settings"].update(
        {
            "mode": mode,
            "shared_backend": backend,
            "min_arbitrage_spread_ore_kwh": spread,
        }
    )
    request["slots"] = [
        {
            "start_ms": 1,
            "len_min": 60,
            "price_ore": 30,
            "spot_ore": 0,
            "confidence": 1,
            "pv_w": 0,
            "load_w": 2000,
            "max_import_w": 8000,
            "max_export_w": 8000,
        }
    ]
    request["storages"][0].update(
        {
            "initial_energy_wh": 8000,
            "terminal_price_ore_kwh": 0,
            "cycle_cost_ore_kwh": 0,
            "throughput_cost_ore_kwh": 0,
        }
    )
    return request


@pytest.mark.parametrize("mode", ["self_consumption", "cheap_charge"])
@pytest.mark.parametrize("backend", ["highs", "cvxpy"])
def test_arbitrage_spread_does_not_tax_service_discharge(
    mode: str, backend: str
) -> None:
    without_spread = handle(mode_spread_request(mode, backend, 0))
    with_spread = handle(mode_spread_request(mode, backend, 100))
    assert without_spread["ok"], without_spread
    assert with_spread["ok"], with_spread
    baseline_w = without_spread["plan"]["actions"][0]["battery_w"]
    guarded_w = with_spread["plan"]["actions"][0]["battery_w"]
    assert baseline_w < -1900
    assert guarded_w == pytest.approx(baseline_w, abs=2)


@pytest.mark.parametrize("scenario_policy", ["recourse", "multistage"])
def test_arbitrage_spread_is_ignored_by_stochastic_service_policies(
    scenario_policy: str,
) -> None:
    without_spread = mode_spread_request("self_consumption", "auto", 0)
    with_spread = mode_spread_request("self_consumption", "auto", 100)
    for request in (without_spread, with_spread):
        request["settings"]["scenario_policy"] = scenario_policy
        request["settings"]["decomposition_method"] = "extensive"

    baseline = handle(without_spread)
    guarded = handle(with_spread)
    assert baseline["ok"], baseline
    assert guarded["ok"], guarded
    baseline_w = baseline["plan"]["actions"][0]["battery_w"]
    guarded_w = guarded["plan"]["actions"][0]["battery_w"]
    assert baseline_w < -1900
    assert guarded_w == pytest.approx(baseline_w, abs=2)


@pytest.mark.parametrize("mode", ["arbitrage", "passive_arbitrage"])
@pytest.mark.parametrize("backend", ["highs", "cvxpy"])
def test_arbitrage_spread_still_blocks_marginal_arbitrage_discharge(
    mode: str, backend: str
) -> None:
    without_spread = handle(mode_spread_request(mode, backend, 0))
    with_spread = handle(mode_spread_request(mode, backend, 100))
    assert without_spread["ok"], without_spread
    assert with_spread["ok"], with_spread
    assert without_spread["plan"]["actions"][0]["battery_w"] < -1900
    assert abs(with_spread["plan"]["actions"][0]["battery_w"]) < 2


def test_arbitrage_spread_is_validated_even_when_mode_ignores_it() -> None:
    with pytest.raises(ProtocolError, match="must be a number"):
        _arbitrage_spread_ore_kwh(
            {"min_arbitrage_spread_ore_kwh": "bad"},
            "self_consumption",
        )


def test_direct_highs_accepts_a_positive_sub_50ms_budget() -> None:
    remaining = _remaining_time_s(time.perf_counter() + 0.01)
    assert 0.0 < remaining <= 0.01


def test_direct_highs_rejects_an_exhausted_budget() -> None:
    with pytest.raises(SolveDeadlineExceeded, match="deadline exceeded"):
        _remaining_time_s(time.perf_counter() - 0.001)


def assert_storage_replays(request: dict, response: dict, tolerance_wh: float = 2.1) -> None:
    energies = {
        str(spec["id"]): float(spec["initial_energy_wh"])
        for spec in request["storages"]
    }
    for slot, action in zip(request["slots"], response["plan"]["actions"]):
        dt_h = slot["len_min"] / 60.0
        for spec in request["storages"]:
            storage_id = str(spec["id"])
            power = action["storage_power_w"][storage_id]
            previous = energies[storage_id]
            if power >= 0:
                replayed = previous + power * dt_h * spec["charge_efficiency"]
            else:
                replayed = previous + power * dt_h / spec["discharge_efficiency"]
            reported = action["storage_energy_wh"][storage_id]
            assert math.isclose(reported, replayed, abs_tol=tolerance_wh)
            if abs(power) <= 1e-3:
                assert reported >= previous - tolerance_wh
            energies[storage_id] = replayed


def assert_nested_close(
    direct: object,
    reference: object,
    *,
    abs_tol: float = 1e-3,
    path: str = "value",
) -> None:
    if isinstance(direct, dict) and isinstance(reference, dict):
        assert direct.keys() == reference.keys(), path
        for key in direct:
            assert_nested_close(
                direct[key],
                reference[key],
                abs_tol=abs_tol,
                path=f"{path}.{key}",
            )
        return
    if isinstance(direct, list) and isinstance(reference, list):
        assert len(direct) == len(reference), path
        for index, (direct_item, reference_item) in enumerate(
            zip(direct, reference)
        ):
            assert_nested_close(
                direct_item,
                reference_item,
                abs_tol=abs_tol,
                path=f"{path}[{index}]",
            )
        return
    if (
        isinstance(direct, (int, float))
        and not isinstance(direct, bool)
        and isinstance(reference, (int, float))
        and not isinstance(reference, bool)
    ):
        assert math.isclose(
            float(direct), float(reference), abs_tol=abs_tol
        ), f"{path}: {direct} != {reference}"
        return
    assert direct == reference, path


def assert_shared_plan_parity(
    direct: dict,
    reference: dict,
    *,
    abs_tol: float = 1e-3,
) -> None:
    direct_plan = direct["plan"]
    reference_plan = reference["plan"]
    for key in (
        "mode",
        "horizon_slots",
        "capacity_wh",
        "initial_soc_pct",
        "total_cost_ore",
    ):
        assert_nested_close(
            direct_plan[key],
            reference_plan[key],
            abs_tol=abs_tol,
            path=f"plan.{key}",
        )
    assert_nested_close(
        direct_plan["actions"],
        reference_plan["actions"],
        abs_tol=abs_tol,
        path="plan.actions",
    )


def test_shared_direct_highs_matches_cvxpy_with_risk_targets_and_costs() -> None:
    request = base_request()
    request["request_id"] = "shared-direct-parity"
    request["settings"].update(
        {
            "shared_backend": "highs",
            "cvar_weight": 0.25,
            "cvar_alpha": 0.75,
            "min_arbitrage_spread_ore_kwh": 7,
        }
    )
    prices = [20, 35, 70, 260, 310, 120]
    loads = [1200, 1800, 900, 2600, 3200, 1600]
    pv = [0, -500, -2400, -800, 0, -300]
    request["slots"] = [
        {
            "start_ms": 1 + index * 900_000,
            "len_min": 15,
            "price_ore": price,
            "spot_ore": 0,
            "confidence": 0.85,
            "pv_w": pv[index],
            "load_w": loads[index],
            "max_import_w": 10_000,
            "max_export_w": 10_000,
        }
        for index, price in enumerate(prices)
    ]
    request["storages"][0].update(
        {
            "initial_energy_wh": 2500,
            "target_energy_wh": 4500,
            "target_slot": 2,
            "throughput_cost_ore_kwh": 2,
        }
    )
    request["storages"].append(
        {
            "id": "garage",
            "capacity_wh": 6000,
            "initial_energy_wh": 3500,
            "min_energy_wh": 800,
            "max_energy_wh": 5600,
            "max_charge_w": 2600,
            "max_discharge_w": 2200,
            "charge_efficiency": 0.92,
            "discharge_efficiency": 0.9,
            "terminal_price_ore_kwh": 14,
            "cycle_cost_ore_kwh": 11,
            "throughput_cost_ore_kwh": 1.5,
        }
    )
    request["scenarios"] = [
        {
            "id": "base",
            "probability": 0.5,
            "load_w": loads,
            "pv_w": pv,
        },
        {
            "id": "cloudy",
            "probability": 0.3,
            "load_w": [value * 1.15 for value in loads],
            "pv_w": [value * 0.65 for value in pv],
        },
        {
            "id": "sunny",
            "probability": 0.2,
            "load_w": [value * 0.9 for value in loads],
            "pv_w": [value * 1.2 for value in pv],
        },
    ]

    reference_request = copy.deepcopy(request)
    reference_request["request_id"] = "shared-cvxpy-parity"
    reference_request["settings"]["shared_backend"] = "cvxpy"
    direct = handle(request)
    reference = handle(reference_request)

    assert direct["ok"], direct
    assert reference["ok"], reference
    assert direct["solver"]["engine"] == "highspy"
    assert reference["solver"]["engine"] == "cvxpy"
    assert direct["solver"]["scenario_policy"] == "shared"
    assert direct["solver"]["formulation"] == "convex"
    assert math.isclose(
        direct["solver"]["objective_ore"],
        reference["solver"]["objective_ore"],
        abs_tol=1e-4,
    )
    for key in (
        "energy",
        "demand_charge_increment",
        "degradation",
        "terminal_energy_value",
    ):
        assert math.isclose(
            direct["solver"]["objective_breakdown_ore"][key],
            reference["solver"]["objective_breakdown_ore"][key],
            abs_tol=1e-4,
        )
    for direct_action, reference_action in zip(
        direct["plan"]["actions"], reference["plan"]["actions"]
    ):
        assert math.isclose(
            direct_action["battery_w"], reference_action["battery_w"], abs_tol=1e-3
        )
        assert math.isclose(
            direct_action["grid_w"], reference_action["grid_w"], abs_tol=1e-3
        )
    assert_storage_replays(request, direct)


@pytest.mark.parametrize(
    "mode",
    ["arbitrage", "cheap_charge", "passive_arbitrage", "self_consumption"],
)
def test_shared_direct_highs_matches_cvxpy_modes(mode: str) -> None:
    request = base_request()
    request["settings"].update({"mode": mode, "shared_backend": "highs"})
    for slot in request["slots"]:
        slot["spot_ore"] = 0
    reference_request = copy.deepcopy(request)
    reference_request["request_id"] = f"shared-{mode}-cvxpy"
    reference_request["settings"]["shared_backend"] = "cvxpy"

    direct = handle(request)
    reference = handle(reference_request)

    assert direct["ok"], direct
    assert reference["ok"], reference
    assert math.isclose(
        direct["solver"]["objective_ore"],
        reference["solver"]["objective_ore"],
        abs_tol=1e-3,
    )
    assert_storage_replays(request, direct)


def test_shared_direct_highs_matches_cvxpy_below_minimum_recovery() -> None:
    request = base_request()
    request["request_id"] = "shared-below-minimum-direct"
    request["settings"].update(
        {
            "mode": "arbitrage",
            "formulation": "relaxed",
            "shared_backend": "highs",
        }
    )
    request["slots"] = [dict(request["slots"][0]) for _ in range(4)]
    for index, slot in enumerate(request["slots"]):
        slot.update(
            {
                "start_ms": 1 + index * 900_000,
                "len_min": 15,
                "price_ore": 20 + index * 40,
                "spot_ore": 0,
            }
        )
    request["storages"][0].update(
        {
            "initial_energy_wh": 500,
            "max_charge_w": 1000,
            "max_discharge_w": 1000,
            "terminal_price_ore_kwh": 0,
        }
    )
    reference_request = copy.deepcopy(request)
    reference_request["request_id"] = "shared-below-minimum-cvxpy"
    reference_request["settings"]["shared_backend"] = "cvxpy"

    direct = handle(request)
    reference = handle(reference_request)

    assert direct["ok"], direct
    assert reference["ok"], reference
    assert direct["solver"]["engine"] == "highspy"
    assert reference["solver"]["engine"] == "cvxpy"
    assert math.isclose(
        direct["solver"]["service_slack"],
        reference["solver"]["service_slack"],
        abs_tol=1e-7,
    )
    assert math.isclose(
        direct["solver"]["objective_ore"],
        reference["solver"]["objective_ore"],
        abs_tol=1e-4,
    )
    assert_shared_plan_parity(direct, reference)
    assert_storage_replays(request, direct)
    assert_storage_replays(reference_request, reference)


def test_shared_direct_highs_matches_strict_pv_surplus_and_limit() -> None:
    request = base_request()
    request["request_id"] = "shared-pv-surplus-direct"
    request["settings"].update(
        {
            "mode": "passive_arbitrage",
            "formulation": "relaxed",
            "shared_backend": "highs",
        }
    )
    request["slots"] = [
        {
            "start_ms": 1,
            "len_min": 60,
            "price_ore": 100,
            "spot_ore": 50,
            "confidence": 1,
            "pv_w": -4000,
            "load_w": 500,
            "max_import_w": 8000,
            "max_export_w": 100,
        }
    ]
    request["storages"][0].update(
        {
            "initial_energy_wh": 9500,
            "max_charge_w": 0,
            "max_discharge_w": 0,
            "terminal_price_ore_kwh": 0,
        }
    )
    reference_request = copy.deepcopy(request)
    reference_request["request_id"] = "shared-pv-surplus-cvxpy"
    reference_request["settings"]["shared_backend"] = "cvxpy"

    direct = handle(request)
    reference = handle(reference_request)

    assert direct["ok"], direct
    assert reference["ok"], reference
    assert_shared_plan_parity(direct, reference)
    action = direct["plan"]["actions"][0]
    assert math.isclose(action["battery_w"], 0, abs_tol=1e-6)
    assert math.isclose(action["grid_w"], -100, abs_tol=1e-6)
    assert math.isclose(action["pv_limit_w"], 600, abs_tol=1e-6)
    assert action["pv_curtail_active"] is True
    assert_storage_replays(request, direct)
    assert_storage_replays(reference_request, reference)


def shared_curtailment_request(mode: str, base_load_w: float) -> dict:
    request = base_request()
    request["request_id"] = f"shared-curtail-{mode}-{base_load_w}"
    request["settings"].update(
        {
            "mode": mode,
            "formulation": "relaxed",
            "shared_backend": "auto",
        }
    )
    request["slots"] = [
        {
            "start_ms": 1,
            "len_min": 60,
            "price_ore": 100,
            "spot_ore": 20,
            "confidence": 1,
            "pv_w": -1000,
            "load_w": base_load_w,
            "max_import_w": 8000,
            "max_export_w": 100,
        }
    ]
    request["scenarios"] = [
        {
            "id": "base",
            "probability": 0.5,
            "pv_w": [-1000],
            "load_w": [base_load_w],
        },
        {
            "id": "sunny",
            "probability": 0.5,
            "pv_w": [-1600],
            "load_w": [500],
        },
    ]
    request["storages"][0].update(
        {
            "initial_energy_wh": 5000,
            "max_charge_w": 0,
            "max_discharge_w": 0,
            "terminal_price_ore_kwh": 0,
        }
    )
    return request


@pytest.mark.parametrize(
    "mode",
    ["self_consumption", "cheap_charge", "passive_arbitrage"],
)
def test_shared_direct_highs_models_post_curtailment_baseline(mode: str) -> None:
    request = shared_curtailment_request(mode, 2000)
    request["request_id"] = f"shared-post-curtail-{mode}-direct"
    request["settings"]["shared_backend"] = "highs"
    reference_request = copy.deepcopy(request)
    reference_request["request_id"] = f"shared-post-curtail-{mode}-cvxpy"
    reference_request["settings"]["shared_backend"] = "cvxpy"

    direct = handle(request)
    reference = handle(reference_request)

    assert direct["ok"], direct
    assert reference["ok"], reference
    assert direct["solver"]["engine"] == "highspy"
    assert reference["solver"]["engine"] == "cvxpy"
    assert_shared_plan_parity(direct, reference)
    assert math.isclose(direct["plan"]["actions"][0]["grid_w"], 2000)
    assert_storage_replays(request, direct)
    assert_storage_replays(reference_request, reference)


@pytest.mark.parametrize(
    "mode",
    ["self_consumption", "cheap_charge", "passive_arbitrage"],
)
def test_shared_curtailment_direction_change_matches_cvxpy(mode: str) -> None:
    request = shared_curtailment_request(mode, 900)
    request["request_id"] = f"shared-curtail-cross-{mode}-auto"
    reference_request = copy.deepcopy(request)
    reference_request["request_id"] = f"shared-curtail-cross-{mode}-cvxpy"
    reference_request["settings"]["shared_backend"] = "cvxpy"

    direct = handle(request)
    reference = handle(reference_request)

    assert direct["ok"], direct
    assert reference["ok"], reference
    assert direct["solver"]["engine"] == "highspy"
    assert direct["solver"]["formulation"] == "convex"
    assert direct["solver"]["mip_gap"] is None
    assert reference["solver"]["engine"] == "cvxpy"
    assert math.isclose(
        direct["solver"]["objective_ore"],
        reference["solver"]["objective_ore"],
        abs_tol=1e-4,
    )
    assert_shared_plan_parity(direct, reference)
    assert math.isclose(direct["plan"]["actions"][0]["grid_w"], 900)
    assert_storage_replays(request, direct)
    assert_storage_replays(reference_request, reference)


def test_shared_baseline_replay_retries_with_exact_highs() -> None:
    request = shared_curtailment_request("self_consumption", 900)
    request["request_id"] = "shared-curtail-exact-retry"
    request["settings"]["shared_backend"] = "highs"
    request["storages"][0].update(
        {
            "max_charge_w": 5000,
            "terminal_price_ore_kwh": 1000,
        }
    )
    reference_request = copy.deepcopy(request)
    reference_request["request_id"] = "shared-curtail-exact-cvxpy"
    reference_request["settings"]["shared_backend"] = "cvxpy"

    direct = handle(request)
    reference = handle(reference_request)

    assert direct["ok"], direct
    assert reference["ok"], reference
    assert direct["solver"]["engine"] == "highspy"
    assert direct["solver"]["formulation"] == "milp"
    assert direct["solver"]["mip_gap"] is not None
    assert direct["solver"]["build_ms"] > 0
    assert direct["solver"]["solver_ms"] > 0
    assert reference["solver"]["engine"] == "cvxpy"
    assert math.isclose(
        direct["solver"]["objective_ore"],
        reference["solver"]["objective_ore"],
        abs_tol=1e-4,
    )
    assert_shared_plan_parity(direct, reference)
    assert math.isclose(direct["plan"]["actions"][0]["battery_w"], 50)
    assert math.isclose(direct["plan"]["actions"][0]["grid_w"], 900)


def test_shared_auto_falls_back_at_each_direct_eligibility_boundary() -> None:
    cases: list[tuple[str, dict]] = []

    commercial = base_request()
    commercial["commercial_constraints"] = {"version": "srcful-commercial-v1"}
    cases.append(("commercial", commercial))

    flex = base_request()
    flex["flex_loads"] = [
        {
            "id": "car",
            "capacity_wh": 40_000,
            "initial_energy_wh": 20_000,
            "max_energy_wh": 40_000,
            "target_energy_wh": 20_000,
            "target_slot": 1,
            "charge_efficiency": 0.9,
            "allowed_steps_w": [0, 2000],
        }
    ]
    cases.append(("flex", flex))

    thermal = base_request()
    thermal["thermal_loads"] = [
        {
            "id": "heater",
            "initial_temp_c": 20,
            "min_temp_c": 18,
            "max_temp_c": 24,
            "outside_temp_c": [10, 10],
            "allowed_steps_w": [0, 1000],
            "gain_c_per_kwh": 1,
            "loss_per_hour": 0.1,
        }
    ]
    cases.append(("thermal", thermal))

    no_storage = base_request()
    no_storage["storages"] = []
    cases.append(("no-storage", no_storage))

    clarabel = base_request()
    clarabel["settings"].update({"solver": "CLARABEL", "formulation": "relaxed"})
    cases.append(("clarabel", clarabel))

    milp = base_request()
    milp["settings"]["formulation"] = "milp"
    cases.append(("milp", milp))

    negative_import = base_request()
    negative_import["slots"][0]["price_ore"] = -10
    cases.append(("unsafe-cycle", negative_import))

    pv_charge_bonus = base_request()
    pv_charge_bonus["settings"].update(
        {"mode": "passive_arbitrage", "pv_charge_bonus_ore_kwh": 1}
    )
    cases.append(("pv-charge-bonus", pv_charge_bonus))

    meter_split = base_request()
    meter_split["settings"]["export_ore_per_kwh"] = 400
    cases.append(("unsafe-meter-split", meter_split))

    above_maximum = base_request()
    above_maximum["storages"][0]["initial_energy_wh"] = 9800
    cases.append(("initial-above-maximum", above_maximum))

    for name, request in cases:
        request["request_id"] = f"shared-auto-boundary-{name}"
        request["settings"]["shared_backend"] = "auto"
        response = handle(request)
        assert response["ok"], (name, response)
        assert response["solver"]["engine"] == "cvxpy", (name, response)
        assert response["solver"]["scenario_policy"] == "shared"


def test_shared_auto_preserves_duplicate_ids_and_first_base_output() -> None:
    request = base_request()
    request["settings"]["shared_backend"] = "auto"
    base_load = [slot["load_w"] for slot in request["slots"]]
    base_pv = [slot["pv_w"] for slot in request["slots"]]
    request["scenarios"] = [
        {"id": "base", "probability": 0.5, "load_w": base_load, "pv_w": base_pv},
        {
            "id": "base",
            "probability": 0.5,
            "load_w": [3500, 6000],
            "pv_w": [-500, -1000],
        },
    ]
    reference_request = copy.deepcopy(request)
    reference_request["request_id"] = "duplicate-base-cvxpy"
    reference_request["settings"]["shared_backend"] = "cvxpy"

    direct = handle(request)
    reference = handle(reference_request)

    assert direct["ok"], direct
    assert reference["ok"], reference
    assert direct["solver"]["engine"] == "highspy"
    assert direct["solver"]["scenario_count"] == 2
    assert math.isclose(
        direct["solver"]["objective_ore"],
        reference["solver"]["objective_ore"],
        abs_tol=1e-4,
    )
    assert_shared_plan_parity(direct, reference)
    for action, load_w, pv_w in zip(
        direct["plan"]["actions"], base_load, base_pv
    ):
        assert math.isclose(
            action["grid_w"],
            load_w + pv_w + action["battery_w"],
            abs_tol=1e-4,
        )
    assert_storage_replays(request, direct)
    assert_storage_replays(reference_request, reference)


@pytest.mark.parametrize("target_slot", [-4, 99])
def test_shared_direct_clamps_target_slot_like_cvxpy(target_slot: int) -> None:
    request = base_request()
    request["settings"]["shared_backend"] = "highs"
    request["storages"][0].update(
        {"target_energy_wh": 5000, "target_slot": target_slot}
    )
    reference_request = copy.deepcopy(request)
    reference_request["request_id"] = f"target-slot-{target_slot}-cvxpy"
    reference_request["settings"]["shared_backend"] = "cvxpy"

    direct = handle(request)
    reference = handle(reference_request)

    assert direct["ok"], direct
    assert reference["ok"], reference
    assert math.isclose(
        direct["solver"]["objective_ore"],
        reference["solver"]["objective_ore"],
        abs_tol=1e-4,
    )
    assert_storage_replays(request, direct)


def realistic_shared_request(scenario_count: int) -> dict:
    request = base_request()
    request["request_id"] = f"realistic-shared-{scenario_count}"
    request["settings"].update(
        {
            "mode": "passive_arbitrage",
            "solver": "HIGHS",
            "formulation": "relaxed",
            "time_limit_s": 8,
            "shared_backend": "highs",
            "cvar_weight": 0.15,
            "cvar_alpha": 0.9,
        }
    )
    slots = []
    base_load = []
    base_pv = []
    for index in range(192):
        hour = (index % 96) / 4.0
        price = 80 + 180 * math.exp(-0.5 * ((hour - 18) / 2) ** 2)
        pv_w = (
            -7000 * math.exp(-0.5 * ((hour - 12.5) / 3) ** 2)
            if 5 < hour < 21
            else 0
        )
        load_w = 500 + 1800 * math.exp(-0.5 * ((hour - 19) / 2) ** 2)
        base_load.append(load_w)
        base_pv.append(pv_w)
        slots.append(
            {
                "start_ms": 1 + index * 900_000,
                "len_min": 15,
                "price_ore": price,
                "spot_ore": price * 0.7,
                "confidence": 1 if index < 96 else 0.6,
                "pv_w": pv_w,
                "load_w": load_w,
                "max_import_w": 11_000,
                "max_export_w": 11_000,
            }
        )
    request["slots"] = slots
    request["storages"] = [
        {
            "id": "home",
            "capacity_wh": 15_000,
            "initial_energy_wh": 7500,
            "min_energy_wh": 1500,
            "max_energy_wh": 14_250,
            "max_charge_w": 5000,
            "max_discharge_w": 5000,
            "charge_efficiency": 0.95,
            "discharge_efficiency": 0.95,
            "terminal_price_ore_kwh": 150,
            "cycle_cost_ore_kwh": 10,
            "throughput_cost_ore_kwh": 1.5,
        }
    ]
    scenarios = []
    for index in range(scenario_count):
        offset = index - (scenario_count - 1) / 2
        scenarios.append(
            {
                "id": "base" if index == 0 else f"scenario-{index}",
                "probability": 1 / scenario_count,
                "load_w": [max(0, value + offset * 250) for value in base_load],
                "pv_w": [min(0, value + offset * 150) for value in base_pv],
            }
        )
    scenarios[0]["load_w"] = base_load
    scenarios[0]["pv_w"] = base_pv
    request["scenarios"] = scenarios
    return request


@pytest.mark.parametrize("scenario_count", [3, 12])
def test_shared_direct_realistic_horizon_matches_cvxpy(
    scenario_count: int,
) -> None:
    request = realistic_shared_request(scenario_count)
    reference_request = copy.deepcopy(request)
    reference_request["request_id"] += "-cvxpy"
    reference_request["settings"]["shared_backend"] = "cvxpy"

    direct = handle(request)
    reference = handle(reference_request)

    assert direct["ok"], direct
    assert reference["ok"], reference
    direct_solver = direct["solver"]
    reference_solver = reference["solver"]
    assert direct_solver["engine"] == "highspy"
    assert direct_solver["backend"] == "highs"
    assert direct_solver["status"] == "optimal"
    assert direct_solver["formulation"] == "convex"
    assert direct_solver["mip_gap"] is None
    assert direct_solver["dpp"] is False
    assert direct_solver["cache_hit"] is False
    assert direct_solver["model_variables"] > 0
    assert direct_solver["model_constraints"] > 0
    assert reference_solver["engine"] == "cvxpy"
    assert reference_solver["backend"] == "highs"
    assert reference_solver["formulation"] == "milp"
    for key, expected in (
        ("scenario_count", scenario_count),
        ("scenario_policy", "shared"),
        ("policy_version", "shared-v1"),
        ("non_anticipative_slots", 192),
        ("cvar_weight", 0.15),
        ("cvar_alpha", 0.9),
    ):
        assert direct_solver[key] == expected
        assert reference_solver[key] == expected
    assert math.isclose(
        direct_solver["service_slack"],
        reference_solver["service_slack"],
        abs_tol=1e-7,
    )
    assert math.isclose(
        direct_solver["objective_ore"],
        reference_solver["objective_ore"],
        abs_tol=1e-3,
    )
    assert_nested_close(
        direct_solver["objective_breakdown_ore"],
        reference_solver["objective_breakdown_ore"],
        abs_tol=1e-3,
        path="solver.objective_breakdown_ore",
    )
    assert_shared_plan_parity(direct, reference, abs_tol=2e-3)
    assert_storage_replays(request, direct)
    assert_storage_replays(reference_request, reference)


def test_shared_auto_retries_with_storage_guard_after_direct_failure(monkeypatch) -> None:
    from ftw_optimizer import direct_highs, shared_highs

    request = base_request()
    request["settings"].update(
        {
            "mode": "arbitrage",
            "formulation": "relaxed",
            "shared_backend": "auto",
        }
    )

    def reject_direct(*args, **kwargs):
        raise direct_highs.DirectHighsError(
            "HiGHS returned simultaneous storage charge and discharge"
        )

    monkeypatch.setattr(shared_highs, "solve_direct_highs", reject_direct)
    response = handle(request)

    assert response["ok"], response
    assert response["solver"]["engine"] == "cvxpy"
    assert response["solver"]["formulation"] == "milp"
    assert response["solver"]["fallback"] is True
    assert "simultaneous" in response["solver"]["fallback_reason"]
    assert_storage_replays(request, response)


def test_shared_auto_retries_with_storage_guard_after_replay_failure(
    monkeypatch,
) -> None:
    from ftw_optimizer import shared_highs
    from ftw_optimizer.model import ReplayConsistencyError

    request = base_request()
    request["settings"].update(
        {
            "mode": "arbitrage",
            "formulation": "relaxed",
            "shared_backend": "auto",
        }
    )

    def reject_direct(*args, **kwargs):
        raise ReplayConsistencyError("direct storage replay failed")

    monkeypatch.setattr(shared_highs, "solve_direct_highs", reject_direct)
    response = handle(request)

    assert response["ok"], response
    assert response["solver"]["engine"] == "cvxpy"
    assert response["solver"]["formulation"] == "milp"
    assert response["solver"]["fallback"] is True
    assert response["solver"]["fallback_reason"] == "direct storage replay failed"
    assert_storage_replays(request, response)


def test_shared_auto_retries_generic_direct_failure_without_storage_guard(
    monkeypatch,
) -> None:
    from ftw_optimizer import shared_highs

    request = base_request()
    request["settings"].update(
        {
            "mode": "arbitrage",
            "formulation": "relaxed",
            "shared_backend": "auto",
        }
    )

    def reject_direct(*args, **kwargs):
        raise RuntimeError("direct API failed")

    monkeypatch.setattr(shared_highs, "solve_direct_highs", reject_direct)
    response = handle(request)

    assert response["ok"], response
    assert response["solver"]["engine"] == "cvxpy"
    assert response["solver"]["formulation"] == "convex"
    assert response["solver"]["fallback"] is True
    assert response["solver"]["fallback_reason"] == "direct API failed"
    assert_storage_replays(request, response)


def test_shared_auto_retries_other_direct_highs_error_without_storage_guard(
    monkeypatch,
) -> None:
    from ftw_optimizer import direct_highs, shared_highs

    request = base_request()
    request["settings"].update(
        {
            "mode": "arbitrage",
            "formulation": "relaxed",
            "shared_backend": "auto",
        }
    )

    def reject_direct(*args, **kwargs):
        raise direct_highs.DirectHighsError(
            "HiGHS economic solve failed with status kTimeLimit"
        )

    monkeypatch.setattr(shared_highs, "solve_direct_highs", reject_direct)
    response = handle(request)

    assert response["ok"], response
    assert response["solver"]["engine"] == "cvxpy"
    assert response["solver"]["formulation"] == "convex"
    assert response["solver"]["fallback"] is True
    assert "kTimeLimit" in response["solver"]["fallback_reason"]
    assert_storage_replays(request, response)


def test_shared_auto_lets_cvxpy_reject_invalid_input_after_direct_error() -> None:
    request = base_request()
    request["settings"].update(
        {"formulation": "relaxed", "shared_backend": "auto"}
    )
    request["storages"][0]["throughput_cost_ore_kwh"] = "invalid"

    response = handle(request)

    assert not response["ok"]
    assert response["error"]["code"] == "invalid_request"
    assert "throughput_cost_ore_kwh must be a number" in response["error"]["message"]


def test_shared_backend_rejects_unknown_value() -> None:
    request = base_request()
    request["settings"]["shared_backend"] = "other"

    response = handle(request)

    assert not response["ok"]
    assert response["error"]["code"] == "invalid_request"
    assert "shared_backend" in response["error"]["message"]


def test_shared_backend_highs_rejects_an_ineligible_request() -> None:
    request = base_request()
    request["settings"]["shared_backend"] = "highs"
    request["storages"] = []

    response = handle(request)

    assert not response["ok"]
    assert response["error"]["code"] == "invalid_request"
    assert "requires at least one storage" in response["error"]["message"]


def test_arbitrage_moves_energy_from_cheap_to_expensive_slot() -> None:
    response = handle(base_request())
    assert response["ok"], response
    actions = response["plan"]["actions"]
    assert actions[0]["battery_w"] > 0
    assert actions[1]["battery_w"] < 0
    assert response["solver"]["backend"] == "highs"
    assert all(math.isfinite(a["grid_w"]) for a in actions)


def test_multiple_discrete_flex_loads_meet_deadlines() -> None:
    request = base_request()
    request["storages"] = []
    request["flex_loads"] = [
        {
            "id": "car-a",
            "capacity_wh": 60000,
            "initial_energy_wh": 12000,
            "max_energy_wh": 60000,
            "target_energy_wh": 15000,
            "target_slot": 1,
            "charge_efficiency": 1,
            "allowed_steps_w": [0, 2000, 4000],
        },
        {
            "id": "car-b",
            "capacity_wh": 40000,
            "initial_energy_wh": 10000,
            "max_energy_wh": 40000,
            "target_energy_wh": 12000,
            "target_slot": 1,
            "charge_efficiency": 1,
            "allowed_steps_w": [0, 2000],
        },
    ]
    response = handle(request)
    assert response["ok"], response
    assert response["solver"]["formulation"] == "milp"
    final = response["plan"]["actions"][-1]["flex_energy_wh"]
    assert final["car-a"] >= 15000 - 1e-4
    assert final["car-b"] >= 12000 - 1e-4


def test_thermal_state_respects_comfort_lexicographically() -> None:
    request = base_request()
    request["storages"] = []
    request["thermal_loads"] = [
        {
            "id": "house",
            "initial_temp_c": 20,
            "min_temp_c": 19,
            "max_temp_c": 22,
            "outside_temp_c": [0, 0],
            "max_power_w": 4000,
            "gain_c_per_kwh": 1,
            "loss_per_hour": 0.05,
        }
    ]
    response = handle(request)
    assert response["ok"], response
    states = [a["thermal_state"]["house"] for a in response["plan"]["actions"]]
    assert min(states) >= 19 - 1e-5
    assert response["solver"]["service_slack"] <= 1e-6


def test_scenario_cvar_uses_shared_asset_schedule() -> None:
    request = base_request()
    request["settings"]["cvar_weight"] = 0.25
    request["scenarios"] = [
        {"id": "base", "probability": 0.7, "load_w": [500, 2500], "pv_w": [0, 0]},
        {"id": "cold", "probability": 0.3, "load_w": [1000, 5000], "pv_w": [0, 0]},
    ]
    response = handle(request)
    assert response["ok"], response
    assert response["solver"]["scenario_count"] == 2


def test_storage_recourse_keeps_first_action_executable_and_improves_wait_and_see_bound() -> None:
    shared = base_request()
    shared["settings"]["mode"] = "self_consumption"
    shared["settings"]["cvar_weight"] = 0
    shared["settings"]["min_arbitrage_spread_ore_kwh"] = 0
    shared["slots"] = [
        {"start_ms": 1, "len_min": 60, "price_ore": 50, "spot_ore": 20, "confidence": 1, "pv_w": 0, "load_w": 0, "max_import_w": 8000, "max_export_w": 8000},
        {"start_ms": 3600001, "len_min": 60, "price_ore": 300, "spot_ore": 100, "confidence": 1, "pv_w": 0, "load_w": 3000, "max_import_w": 8000, "max_export_w": 8000},
        {"start_ms": 7200001, "len_min": 60, "price_ore": 50, "spot_ore": 20, "confidence": 1, "pv_w": 0, "load_w": 0, "max_import_w": 8000, "max_export_w": 8000},
    ]
    shared["storages"][0]["initial_energy_wh"] = 5000
    shared["storages"][0]["terminal_price_ore_kwh"] = 0
    shared["scenarios"] = [
        {"id": "base", "probability": 0.5, "load_w": [0, 3000, 0], "pv_w": [0, 0, 0]},
        {"id": "sunny", "probability": 0.5, "load_w": [0, 0, 0], "pv_w": [0, -3000, 0]},
    ]
    shared_response = handle(shared)
    assert shared_response["ok"], shared_response

    recourse = base_request()
    recourse.update(shared)
    recourse["request_id"] = "recourse-test"
    recourse["settings"] = dict(shared["settings"])
    recourse["settings"]["scenario_policy"] = "recourse"
    recourse["settings"]["non_anticipative_slots"] = 1
    recourse_response = handle(recourse)
    assert recourse_response["ok"], recourse_response
    assert recourse_response["solver"]["scenario_policy"] == "recourse"
    assert recourse_response["solver"]["non_anticipative_slots"] == 1
    assert recourse_response["solver"]["objective_ore"] < shared_response["solver"]["objective_ore"] - 1


def test_recourse_rejects_flexible_assets_instead_of_mis_scoring_them() -> None:
    request = base_request()
    request["settings"]["scenario_policy"] = "recourse"
    request["flex_loads"] = [
        {
            "id": "car",
            "capacity_wh": 40000,
            "initial_energy_wh": 10000,
            "max_energy_wh": 40000,
            "target_energy_wh": 12000,
            "target_slot": 1,
            "charge_efficiency": 1,
            "allowed_steps_w": [0, 2000],
        }
    ]
    response = handle(request)
    assert not response["ok"]
    assert response["error"]["code"] == "invalid_request"
    assert "flex_loads" in response["error"]["message"]


def test_recourse_rejects_fractional_non_anticipative_prefix() -> None:
    request = base_request()
    request["settings"]["scenario_policy"] = "recourse"
    request["settings"]["non_anticipative_slots"] = 1.5
    response = handle(request)
    assert not response["ok"]
    assert response["error"]["code"] == "invalid_request"


def test_multistage_tree_is_hierarchical_and_never_remerges() -> None:
    scenarios = (
        Scenario("base", 0.5, np.asarray([0, 0, 100, 100]), np.zeros(4)),
        Scenario("high", 0.3, np.asarray([0, 0, 500, 500]), np.zeros(4)),
        Scenario("low", 0.2, np.asarray([0, 0, 0, 0]), np.zeros(4)),
    )
    tree = build_scenario_tree(
        scenarios,
        n=4,
        first_stage_slots=1,
        branch_interval_slots=1,
        branch_horizon_slots=4,
        max_branching=2,
    )
    assert len(set(tree.node_at[:, 0])) == 1
    for left in range(len(scenarios)):
        for right in range(left + 1, len(scenarios)):
            separated = False
            for slot in range(4):
                same = tree.node_at[left, slot] == tree.node_at[right, slot]
                assert not (separated and same)
                separated = separated or not same


def test_scenario_reduction_preserves_base_and_probability_mass() -> None:
    scenarios = [
        Scenario(
            "base" if i == 0 else f"path-{i}",
            0.1,
            np.full(8, float(i * 100)),
            np.zeros(8),
        )
        for i in range(10)
    ]
    reduced = reduce_scenarios(scenarios, 4, np.full(8, 0.25))
    assert reduced.original_count == 10
    assert len(reduced.scenarios) == 4
    assert reduced.scenarios[0].id == "base"
    assert math.isclose(sum(s.probability for s in reduced.scenarios), 1.0)
    assert reduced.reduction_error > 0


def test_scenario_geometry_preserves_pv_load_composition() -> None:
    scenarios = (
        Scenario(
            "base",
            0.5,
            np.asarray([1000.0, 1000.0]),
            np.asarray([-500.0, -500.0]),
        ),
        Scenario(
            "same-net",
            0.5,
            np.asarray([500.0, 500.0]),
            np.asarray([0.0, 0.0]),
        ),
    )
    tree = build_scenario_tree(
        scenarios,
        n=2,
        first_stage_slots=1,
        branch_interval_slots=1,
        branch_horizon_slots=2,
        max_branching=2,
    )
    assert tree.node_at[0, 1] != tree.node_at[1, 1]
    reduced = reduce_scenarios(list(scenarios), 1, np.asarray([0.25, 0.25]))
    assert reduced.reduction_error > 0


def test_move_blocks_split_at_every_information_branch() -> None:
    blocks = decision_blocks(
        n=20,
        near_horizon_slots=4,
        mid_horizon_slots=12,
        mid_block_slots=3,
        far_block_slots=6,
        branch_slots=(1, 5, 9, 13),
    )
    assert blocks[:4] == ((0, 1), (1, 2), (2, 3), (3, 4))
    for start, end in blocks:
        assert not any(start < branch < end for branch in (1, 5, 9, 13))


def test_multistage_model_reuses_dpp_cache_and_keeps_first_action_shared() -> None:
    clear_multistage_cache()
    request = base_request()
    request["settings"].update(
        {
            "scenario_policy": "multistage",
            "non_anticipative_slots": 1,
            "branch_interval_slots": 1,
            "branch_horizon_slots": 2,
            "scenario_limit": 4,
            "service_cvar_weight": 1,
            "economic_cvar_weight": 0,
            "multistage_backend": "cvxpy",
        }
    )
    request["scenarios"] = [
        {"id": "base", "probability": 0.6, "load_w": [500, 2500], "pv_w": [0, 0]},
        {"id": "high", "probability": 0.4, "load_w": [500, 5000], "pv_w": [0, 0]},
    ]
    first = handle(request)
    assert first["ok"], first
    assert first["solver"]["scenario_policy"] == "multistage"
    assert first["solver"]["policy_version"] == "storage-multistage-v1"
    assert first["solver"]["dpp"] is True
    assert first["solver"]["cache_hit"] is False
    assert first["solver"]["economic_cvar_weight"] == 0

    request["request_id"] = "test-2"
    second = handle(request)
    assert second["ok"], second
    assert second["solver"]["cache_hit"] is True
    assert second["solver"]["build_ms"] == 0
    assert math.isclose(
        first["plan"]["actions"][0]["battery_w"],
        second["plan"]["actions"][0]["battery_w"],
        abs_tol=1e-3,
    )

    request["request_id"] = "test-3"
    request["slots"][0]["price_ore"] = 400
    request["slots"][1]["price_ore"] = 20
    request["slots"][1]["spot_ore"] = 10
    third = handle(request)
    assert third["ok"], third
    assert third["solver"]["cache_hit"] is True
    assert third["plan"]["actions"][0]["battery_w"] < 0


def test_multistage_reduces_large_ensemble_before_extensive_solve() -> None:
    clear_multistage_cache()
    request = base_request()
    request["settings"].update(
        {
            "scenario_policy": "multistage",
            "scenario_limit": 6,
            "decomposition_threshold": 3,
            "branch_interval_slots": 1,
            "branch_horizon_slots": 2,
        }
    )
    request["scenarios"] = [
        {
            "id": "base" if i == 0 else f"path-{i}",
            "probability": 0.2,
            "load_w": [500 + i * 100, 2500 + i * 200],
            "pv_w": [0, 0],
        }
        for i in range(5)
    ]
    response = handle(request)
    assert response["ok"], response
    assert response["solver"]["scenario_original_count"] == 5
    assert response["solver"]["scenario_count"] == 3
    assert response["solver"]["decomposition"] == "direct-highs-scenario-reduction-extensive"


def test_direct_highs_matches_cvxpy_multistage_reference() -> None:
    request = base_request()
    request["settings"].update(
        {
            "scenario_policy": "multistage",
            "scenario_limit": 4,
            "branch_interval_slots": 1,
            "branch_horizon_slots": 2,
            "multistage_backend": "highs",
            "economic_cvar_weight": 0.25,
        }
    )
    request["storages"][0]["initial_energy_wh"] = 500
    request["storages"].append(
        {
            "id": "shed",
            "capacity_wh": 5000,
            "initial_energy_wh": 2500,
            "min_energy_wh": 500,
            "max_energy_wh": 4500,
            "max_charge_w": 2000,
            "max_discharge_w": 2500,
            "charge_efficiency": 0.92,
            "discharge_efficiency": 0.93,
            "terminal_price_ore_kwh": 25,
            "cycle_cost_ore_kwh": 8,
        }
    )
    request["scenarios"] = [
        {"id": "base", "probability": 0.6, "load_w": [500, 2500], "pv_w": [0, 0]},
        {"id": "high", "probability": 0.4, "load_w": [500, 5000], "pv_w": [0, 0]},
    ]
    reference_request = copy.deepcopy(request)
    reference_request["request_id"] = "cvxpy-reference"
    reference_request["settings"]["multistage_backend"] = "cvxpy"

    direct = handle(request)
    reference = handle(reference_request)
    assert direct["ok"], direct
    assert reference["ok"], reference
    assert direct["solver"]["engine"] == "highspy"
    assert direct["solver"]["backend"] == "highs"
    assert direct["solver"]["status"] == "optimal"
    assert direct["solver"]["formulation"] == "multistage-lp"
    assert direct["solver"]["dpp"] is False
    assert direct["solver"]["cache_hit"] is False
    assert direct["solver"]["mip_gap"] is None
    assert direct["solver"]["scenario_count"] == 2
    assert direct["solver"]["scenario_original_count"] == 2
    assert direct["solver"]["scenario_reduction_error"] == 0
    assert direct["solver"]["scenario_policy"] == "multistage"
    assert direct["solver"]["policy_version"] == "storage-multistage-v1"
    assert direct["solver"]["non_anticipative_slots"] == 1
    assert direct["solver"]["tree_nodes"] == 1
    assert direct["solver"]["move_blocks"] == 2
    assert direct["solver"]["decomposition"] == "direct-highs-extensive"
    assert direct["solver"]["risk_model"] == "service-cvar-then-expected-cost"
    assert direct["solver"]["service_cvar_weight"] == 1
    assert direct["solver"]["service_cvar_alpha"] == 0.95
    assert direct["solver"]["economic_cvar_weight"] == 0.25
    assert direct["solver"]["economic_cvar_alpha"] == 0.9
    assert direct["solver"]["model_variables"] > 0
    assert direct["solver"]["model_constraints"] > 0
    direct_policy = json.loads(direct["solver"]["policy_config"])
    reference_policy = json.loads(reference["solver"]["policy_config"])
    assert direct_policy.pop("backend") == "highs"
    assert reference_policy.pop("backend") == "cvxpy"
    assert direct_policy == reference_policy
    for key in (
        "scenario_count",
        "scenario_original_count",
        "scenario_reduction_error",
        "scenario_policy",
        "policy_version",
        "non_anticipative_slots",
        "tree_nodes",
        "move_blocks",
        "risk_model",
        "service_cvar_weight",
        "service_cvar_alpha",
        "economic_cvar_weight",
        "economic_cvar_alpha",
    ):
        assert direct["solver"][key] == reference["solver"][key]
    assert math.isclose(
        direct["solver"]["objective_ore"],
        reference["solver"]["objective_ore"],
        abs_tol=1e-4,
    )
    assert math.isclose(
        direct["plan"]["actions"][0]["battery_w"],
        reference["plan"]["actions"][0]["battery_w"],
        abs_tol=1e-3,
    )
    assert_storage_replays(request, direct)
    assert_storage_replays(reference_request, reference)


def test_multistage_auto_keeps_binary_guards_for_unsafe_incentives() -> None:
    negative_price = base_request()
    negative_price["settings"]["scenario_policy"] = "multistage"
    negative_price["slots"][0]["price_ore"] = -10
    response = handle(negative_price)
    assert response["ok"], response
    assert response["solver"]["engine"] == "cvxpy"
    assert response["solver"]["formulation"] == "multistage-milp"

    shared_bonus = base_request()
    shared_bonus["settings"].update(
        {"mode": "passive_arbitrage", "pv_charge_bonus_ore_kwh": 1}
    )
    response = handle(shared_bonus)
    assert response["ok"], response
    assert response["solver"]["formulation"] == "milp"

    recourse_bonus = base_request()
    recourse_bonus["settings"].update(
        {
            "mode": "passive_arbitrage",
            "scenario_policy": "recourse",
            "pv_charge_bonus_ore_kwh": 1,
        }
    )
    response = handle(recourse_bonus)
    assert response["ok"], response
    assert response["solver"]["formulation"] == "stochastic-recourse-milp"

    pv_bonus = base_request()
    pv_bonus["settings"].update(
        {
            "mode": "passive_arbitrage",
            "scenario_policy": "multistage",
            "pv_charge_bonus_ore_kwh": 1,
        }
    )
    response = handle(pv_bonus)
    assert response["ok"], response
    assert response["solver"]["engine"] == "cvxpy"
    assert response["solver"]["formulation"] == "multistage-milp"


_RELAXED_POLICY_FORMULATIONS = {
    "shared": ("convex", "milp"),
    "recourse": ("stochastic-recourse-convex", "stochastic-recourse-milp"),
    "multistage": ("multistage-lp", "multistage-milp"),
}


@pytest.mark.parametrize(
    ("formulation", "relaxation_unsafe", "expected"),
    [
        ("auto", False, False),
        ("auto", True, True),
        ("relaxed", False, False),
        ("relaxed", True, True),
        ("milp", False, True),
        ("milp", True, True),
    ],
)
def test_direction_binary_requirement(
    formulation: str, relaxation_unsafe: bool, expected: bool
) -> None:
    assert _requires_direction_binary(formulation, relaxation_unsafe) is expected


@pytest.mark.parametrize(
    ("import_price", "export_price", "bonus", "terminal_price", "expected"),
    [
        (10, 0, 0, 0, False),
        (-1, -2, 0, 0, True),
        (10, -1, 0, 0, True),
        (10, 0, 1, 0, True),
        (10, 0, 0, -1, True),
        (-0.5e-9, -0.5e-9, 0, -0.5e-9, False),
    ],
)
def test_storage_relaxation_risk_sources(
    import_price: float,
    export_price: float,
    bonus: float,
    terminal_price: float,
    expected: bool,
) -> None:
    storage = {"terminal_price_ore_kwh": terminal_price}
    assert (
        _storage_relaxation_is_unsafe(
            np.asarray([import_price]),
            np.asarray([export_price]),
            bonus,
            [storage],
        )
        is expected
    )
    assert not _storage_relaxation_is_unsafe(
        np.asarray([import_price]),
        np.asarray([export_price]),
        bonus,
        [],
    )


def _relaxed_flow_guard_request(scenario_policy: str) -> dict:
    request = base_request()
    request["request_id"] = f"relaxed-flow-guard-{scenario_policy}"
    request["settings"].update(
        {
            "mode": "arbitrage",
            "solver": "HIGHS",
            "formulation": "relaxed",
            "shared_backend": "cvxpy",
        }
    )
    if scenario_policy != "shared":
        request["settings"]["scenario_policy"] = scenario_policy
    if scenario_policy == "multistage":
        request["settings"]["multistage_backend"] = "cvxpy"
        clear_multistage_cache()
    request["slots"] = [
        {
            "start_ms": 1,
            "len_min": 60,
            "price_ore": 20,
            "spot_ore": 10,
            "confidence": 1,
            "pv_w": 0,
            "load_w": 0,
            "max_import_w": 8000,
            "max_export_w": 8000,
        }
    ]
    request["storages"][0].update(
        {
            "capacity_wh": 10_000,
            "initial_energy_wh": 10_000,
            "min_energy_wh": 0,
            "max_energy_wh": 10_000,
            "max_charge_w": 5000,
            "max_discharge_w": 5000,
            "charge_efficiency": 1,
            "discharge_efficiency": 1,
            "terminal_price_ore_kwh": 0,
            "cycle_cost_ore_kwh": 0,
            "throughput_cost_ore_kwh": 0,
        }
    )
    return request


@pytest.mark.parametrize("scenario_policy", _RELAXED_POLICY_FORMULATIONS)
def test_safe_relaxed_formulation_remains_continuous(
    scenario_policy: str,
) -> None:
    request = _relaxed_flow_guard_request(scenario_policy)

    response = handle(request)

    assert response["ok"], response
    expected, _ = _RELAXED_POLICY_FORMULATIONS[scenario_policy]
    assert response["solver"]["formulation"] == expected
    assert_storage_replays(request, response)


@pytest.mark.parametrize("scenario_policy", _RELAXED_POLICY_FORMULATIONS)
def test_relaxed_formulation_guards_negative_price_storage_cycles(
    scenario_policy: str,
) -> None:
    request = _relaxed_flow_guard_request(scenario_policy)
    request["slots"][0].update({"price_ore": -100, "spot_ore": -200})
    request["storages"][0].update(
        {"charge_efficiency": 0.95, "discharge_efficiency": 0.95}
    )

    response = handle(request)

    assert response["ok"], response
    _, expected = _RELAXED_POLICY_FORMULATIONS[scenario_policy]
    assert response["solver"]["formulation"] == expected
    assert response["solver"].get("fallback", False) is False
    assert_storage_replays(request, response)


@pytest.mark.parametrize("scenario_policy", _RELAXED_POLICY_FORMULATIONS)
def test_relaxed_formulation_guards_pv_bonus_storage_cycles(
    scenario_policy: str,
) -> None:
    request = _relaxed_flow_guard_request(scenario_policy)
    request["slots"][0].update({"price_ore": 0, "spot_ore": 0, "pv_w": -5000})
    request["settings"]["mode"] = "passive_arbitrage"
    request["settings"]["pv_charge_bonus_ore_kwh"] = 100

    response = handle(request)

    assert response["ok"], response
    _, expected = _RELAXED_POLICY_FORMULATIONS[scenario_policy]
    assert response["solver"]["formulation"] == expected
    assert math.isclose(
        response["solver"]["objective_ore"],
        response["plan"]["total_cost_ore"],
        abs_tol=1e-5,
    )
    assert_storage_replays(request, response)


@pytest.mark.parametrize("scenario_policy", _RELAXED_POLICY_FORMULATIONS)
def test_relaxed_formulation_guards_profitable_meter_splits(
    scenario_policy: str,
) -> None:
    request = _relaxed_flow_guard_request(scenario_policy)
    request["slots"][0].update({"price_ore": 10, "spot_ore": 100})
    request["storages"][0].update(
        {
            "initial_energy_wh": 5000,
            "max_charge_w": 0,
            "max_discharge_w": 0,
        }
    )

    response = handle(request)

    assert response["ok"], response
    _, expected = _RELAXED_POLICY_FORMULATIONS[scenario_policy]
    assert response["solver"]["formulation"] == expected
    assert math.isclose(response["plan"]["actions"][0]["grid_w"], 0, abs_tol=1e-5)
    assert math.isclose(
        response["solver"]["objective_ore"],
        response["plan"]["total_cost_ore"],
        abs_tol=1e-5,
    )
    assert_storage_replays(request, response)


def test_relaxed_formulation_guards_negative_export_storage_cycles() -> None:
    request = _relaxed_flow_guard_request("shared")
    request["slots"][0].update(
        {"price_ore": 100, "spot_ore": -100, "pv_w": -5000}
    )
    request["storages"][0].update(
        {"charge_efficiency": 0.95, "discharge_efficiency": 0.95}
    )
    request["commercial_constraints"] = {
        "version": "srcful-commercial-v1",
        "allow_pv_curtailment": False,
    }

    response = handle(request)

    assert response["ok"], response
    assert response["solver"]["formulation"] == "milp"
    assert_storage_replays(request, response)


@pytest.mark.parametrize("scenario_policy", _RELAXED_POLICY_FORMULATIONS)
def test_relaxed_formulation_guards_negative_terminal_value(
    scenario_policy: str,
) -> None:
    request = _relaxed_flow_guard_request(scenario_policy)
    request["settings"]["mode"] = "self_consumption"
    request["storages"][0].update(
        {
            "charge_efficiency": 0.95,
            "discharge_efficiency": 0.95,
            "terminal_price_ore_kwh": -100,
        }
    )

    response = handle(request)

    assert response["ok"], response
    _, expected = _RELAXED_POLICY_FORMULATIONS[scenario_policy]
    assert response["solver"]["formulation"] == expected
    assert_storage_replays(request, response)


@pytest.mark.parametrize("scenario_policy", _RELAXED_POLICY_FORMULATIONS)
def test_neutral_relaxed_storage_cycle_retries_with_direction_guard(
    scenario_policy: str,
) -> None:
    request = _relaxed_flow_guard_request(scenario_policy)
    request["settings"]["solver"] = "CLARABEL"
    request["slots"][0].update({"price_ore": 0, "spot_ore": 0})
    request["storages"][0].update(
        {
            "initial_energy_wh": 5000,
            "charge_efficiency": 0.95,
            "discharge_efficiency": 0.95,
        }
    )

    response = handle(request)

    assert response["ok"], response
    _, expected = _RELAXED_POLICY_FORMULATIONS[scenario_policy]
    assert response["solver"]["formulation"] == expected
    assert response["solver"]["fallback"] is True
    assert "inconsistent with replay" in response["solver"]["fallback_reason"]
    assert_storage_replays(request, response)


def test_progressive_hedging_rejects_required_physical_direction_guards() -> None:
    request = _relaxed_flow_guard_request("multistage")
    request["settings"].update(
        {
            "decomposition_method": "progressive_hedging",
            "multistage_backend": "auto",
        }
    )
    request["slots"][0]["spot_ore"] = -2000
    request["storages"][0].update(
        {
            "charge_efficiency": 0.95,
            "discharge_efficiency": 0.95,
            "terminal_price_ore_kwh": -1000,
        }
    )

    response = handle(request)

    assert not response["ok"]
    assert response["error"]["code"] == "invalid_request"
    assert "physical direction guards" in response["error"]["message"]


def test_multistage_curtailment_cannot_create_room_for_passive_export() -> None:
    request = base_request()
    request["settings"].update(
        {
            "mode": "passive_arbitrage",
            "scenario_policy": "multistage",
            "near_horizon_slots": 1,
            "mid_horizon_slots": 1,
            "far_block_slots": 2,
            "branch_horizon_slots": 1,
        }
    )
    request["slots"] = [
        {
            "start_ms": 1, "len_min": 60, "price_ore": 10, "spot_ore": 0,
            "confidence": 1, "pv_w": 0, "load_w": 500,
            "max_import_w": 8000, "max_export_w": 8000,
        },
        {
            "start_ms": 3600001, "len_min": 60, "price_ore": 300,
            "spot_ore": 200, "confidence": 1, "pv_w": -1000, "load_w": 500,
            "max_import_w": 8000, "max_export_w": 8000,
        },
        {
            "start_ms": 7200001, "len_min": 60, "price_ore": 300,
            "spot_ore": 200, "confidence": 1, "pv_w": 0, "load_w": 2000,
            "max_import_w": 8000, "max_export_w": 8000,
        },
    ]
    request["storages"][0]["initial_energy_wh"] = 8000
    request["storages"][0]["terminal_price_ore_kwh"] = 0

    for backend in ("highs", "cvxpy"):
        candidate = copy.deepcopy(request)
        candidate["request_id"] = f"passive-curtail-{backend}"
        candidate["settings"]["multistage_backend"] = backend
        response = handle(candidate)
        assert response["ok"], response
        for action in response["plan"]["actions"]:
            post_curtail_baseline = action["grid_w"] - action["battery_w"]
            assert action["grid_w"] >= min(post_curtail_baseline, 0.0) - 1e-3
        assert response["plan"]["actions"][1]["battery_w"] >= -1e-3


def test_multistage_uses_progressive_hedging_only_for_eligible_large_convex_case() -> None:
    request = base_request()
    request["settings"].update(
        {
            "scenario_policy": "multistage",
            "formulation": "relaxed",
            "scenario_limit": 6,
            "decomposition_threshold": 3,
            "decomposition_method": "progressive_hedging",
            "branch_interval_slots": 1,
            "branch_horizon_slots": 2,
            "ph_max_iterations": 4,
            "ph_tolerance_w": 10,
        }
    )
    request["scenarios"] = [
        {
            "id": "base" if i == 0 else f"path-{i}",
            "probability": 0.2,
            "load_w": [500 + i * 50, 2500 + i * 100],
            "pv_w": [0, 0],
        }
        for i in range(5)
    ]
    response = handle(request)
    assert response["ok"], response
    assert response["solver"]["decomposition"] == "progressive-hedging"
    assert response["solver"]["formulation"] == "multistage-ph-qp"
    assert response["solver"]["ph_residual_w"] <= 10


def test_progressive_hedging_skips_iteration_for_converged_initial_solution(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from ftw_optimizer import progressive

    request = base_request()
    request["request_id"] = "ph-initial-consensus"
    request["slots"] = request["slots"][:1]
    request["settings"].update(
        {
            "scenario_policy": "multistage",
            "formulation": "relaxed",
            "decomposition_method": "progressive_hedging",
            "ph_max_iterations": 4,
            "ph_tolerance_w": 5,
        }
    )
    request["scenarios"] = [
        {
            "id": "base",
            "probability": 1,
            "load_w": [500],
            "pv_w": [0],
        }
    ]

    original_solve = progressive._solve_problem
    solve_calls = 0

    def solve_once(*args, **kwargs) -> None:
        nonlocal solve_calls
        solve_calls += 1
        if solve_calls > 1:
            raise AssertionError("converged initial PH solution ran another solve")
        original_solve(*args, **kwargs)

    monkeypatch.setattr(progressive, "_solve_problem", solve_once)

    response = handle(request)

    assert response["ok"], response
    assert solve_calls == 1
    assert response["solver"]["status"] == "optimal-ph"
    assert response["solver"]["ph_iterations"] == 0
    assert response["solver"]["ph_residual_w"] == pytest.approx(0)
    assert_storage_replays(request, response)


def test_progressive_hedging_refuses_discrete_mode() -> None:
    request = base_request()
    request["settings"].update(
        {
            "scenario_policy": "multistage",
            "decomposition_method": "progressive_hedging",
        }
    )
    response = handle(request)
    assert not response["ok"]
    assert "not eligible" in response["error"]["message"]


def test_multistage_rejects_unobserved_flexible_assets() -> None:
    request = base_request()
    request["settings"]["scenario_policy"] = "multistage"
    request["flex_loads"] = [
        {
            "id": "car",
            "capacity_wh": 40000,
            "initial_energy_wh": 10000,
            "max_energy_wh": 40000,
            "target_energy_wh": 12000,
            "target_slot": 1,
            "charge_efficiency": 1,
            "allowed_steps_w": [0, 2000],
        }
    ]
    response = handle(request)
    assert not response["ok"]
    assert "flex_loads" in response["error"]["message"]


def test_rejects_wrong_site_sign() -> None:
    request = base_request()
    request["slots"][0]["pv_w"] = 500
    response = handle(request)
    assert not response["ok"]
    assert response["error"]["code"] == "invalid_request"


def test_clarabel_solves_continuous_formulation() -> None:
    request = base_request()
    request["settings"]["solver"] = "CLARABEL"
    request["settings"]["formulation"] = "relaxed"
    response = handle(request)
    assert response["ok"], response
    assert response["solver"]["backend"] == "clarabel"
    assert response["solver"]["formulation"] == "convex"


def test_surplus_only_ev_does_not_block_home_battery_grid_charge() -> None:
    """A plugged-in surplus-only EV must not import, but the home battery may.

    Forbidding grid-funded battery charge while the car sat on the charger
    made active arbitrage idle for the whole connection. Battery→EV is
    already blocked by no_storage_to_load / surplus_only on the flex load.
    """

    request = base_request()
    request["flex_loads"] = [
        {
            "id": "surplus-car",
            "capacity_wh": 60000,
            "initial_energy_wh": 30000,
            "max_energy_wh": 60000,
            "target_energy_wh": 30000,
            "target_slot": 1,
            "charge_efficiency": 0.9,
            "allowed_steps_w": [0, 3000],
            "surplus_only": True,
            "no_storage_to_load": True,
        }
    ]
    response = handle(request)
    assert response["ok"], response
    actions = response["plan"]["actions"]
    assert all(a["flex_power_w"]["surplus-car"] <= 1e-5 for a in actions)
    assert actions[0]["battery_w"] > 100
    assert actions[0]["grid_w"] > 100


def test_surplus_only_ev_takes_pv_while_battery_grid_charges() -> None:
    """Leftover PV may go to the car while the home battery buys from the grid.

    Forbidding site import whenever the EV was active idled the car on
    every cheap slot the battery wanted to charge.
    """

    request = base_request()
    request["slots"][0]["pv_w"] = -6500
    request["slots"][0]["max_import_w"] = 16000
    request["storages"][0]["max_charge_w"] = 10000
    request["flex_loads"] = [
        {
            "id": "surplus-car",
            "capacity_wh": 40000,
            "initial_energy_wh": 8000,
            "max_energy_wh": 40000,
            "target_energy_wh": 16000,
            "target_slot": 1,
            "charge_efficiency": 1,
            "allowed_steps_w": [0, 3000],
            "surplus_only": True,
            "no_storage_to_load": True,
        }
    ]
    response = handle(request)
    assert response["ok"], response
    action = response["plan"]["actions"][0]
    assert action["flex_power_w"]["surplus-car"] > 100
    assert action["battery_w"] > 100
    assert action["grid_w"] > 100
    leftover = max(0.0, 6500 - 500)
    assert action["flex_power_w"]["surplus-car"] <= leftover + 50 + 1e-5


def test_surplus_only_ev_still_cannot_import() -> None:
    request = base_request()
    request["slots"] = [request["slots"][1]]  # expensive slot only
    request["slots"][0]["pv_w"] = -3000  # leftover 500 W, below the 2 kW step
    request["storages"][0]["initial_energy_wh"] = 2000
    request["flex_loads"] = [
        {
            "id": "surplus-car",
            "capacity_wh": 40000,
            "initial_energy_wh": 10000,
            "max_energy_wh": 40000,
            "target_energy_wh": 20000,
            "target_slot": 0,
            "charge_efficiency": 1,
            "allowed_steps_w": [0, 2000],
            "surplus_only": True,
        }
    ]
    response = handle(request)
    assert response["ok"], response
    action = response["plan"]["actions"][0]
    leftover = max(0.0, 3000 - 2500)
    assert action["flex_power_w"]["surplus-car"] <= leftover + 50 + 1e-5
    assert action["flex_power_w"]["surplus-car"] <= 1e-5


def test_ev_charge_never_coincides_with_battery_export() -> None:
    request = base_request()
    request["slots"] = [request["slots"][1]]
    request["storages"][0]["initial_energy_wh"] = 9000
    request["flex_loads"] = [
        {
            "id": "car",
            "capacity_wh": 40000,
            "initial_energy_wh": 10000,
            "max_energy_wh": 40000,
            "target_energy_wh": 12000,
            "target_slot": 0,
            "charge_efficiency": 1,
            "allowed_steps_w": [0, 2000],
        }
    ]
    response = handle(request)
    assert response["ok"], response
    action = response["plan"]["actions"][0]
    assert action["flex_power_w"]["car"] > 0
    assert not (action["battery_w"] < 0 and action["grid_w"] < -1e-5)


def test_storage_below_minimum_recovers_without_worsening() -> None:
    request = base_request()
    request["slots"] = [dict(request["slots"][0]) for _ in range(4)]
    for i, slot in enumerate(request["slots"]):
        slot["start_ms"] = 1 + i * 15 * 60 * 1000
        slot["len_min"] = 15
    request["storages"][0]["initial_energy_wh"] = 500
    request["storages"][0]["max_charge_w"] = 1000
    response = handle(request)
    assert response["ok"], response
    energies = [action["storage_energy_wh"]["home"] for action in response["plan"]["actions"]]
    assert energies[0] >= 500 - 1e-5
    assert energies == sorted(energies)
    assert energies[-1] >= 1000 - 0.01


def test_storage_above_maximum_replays_without_simultaneous_energy_loss() -> None:
    expected_formulations = {
        "shared": "milp",
        "recourse": "stochastic-recourse-milp",
        "multistage": "multistage-milp",
    }
    for scenario_policy in ("shared", "recourse", "multistage"):
        for formulation in ("auto", "relaxed"):
            request = base_request()
            request["request_id"] = f"soc-above-max-{scenario_policy}-{formulation}"
            request["settings"]["formulation"] = formulation
            if scenario_policy != "shared":
                request["settings"]["scenario_policy"] = scenario_policy
            request["storages"][0]["initial_energy_wh"] = 9800

            response = handle(request)

            assert response["ok"], response
            assert response["solver"]["formulation"] == expected_formulations[scenario_policy]
            energy_wh = request["storages"][0]["initial_energy_wh"]
            storage = request["storages"][0]
            for slot, action in zip(request["slots"], response["plan"]["actions"]):
                power_w = action["storage_power_w"]["home"]
                previous_energy_wh = energy_wh
                dt_h = slot["len_min"] / 60.0
                if power_w >= 0:
                    energy_wh += power_w * dt_h * storage["charge_efficiency"]
                else:
                    energy_wh += power_w * dt_h / storage["discharge_efficiency"]
                assert math.isclose(
                    action["storage_energy_wh"]["home"],
                    energy_wh,
                    abs_tol=0.1,
                )
                if abs(power_w) <= 1e-6:
                    assert action["storage_energy_wh"]["home"] >= previous_energy_wh - 0.1
            assert energy_wh <= storage["max_energy_wh"] + 0.1


def test_storage_just_above_maximum_uses_replay_safe_guard() -> None:
    expected_formulations = {
        "shared": "milp",
        "recourse": "stochastic-recourse-milp",
        "multistage": "multistage-milp",
    }
    for delta in (0.5e-6, 1e-6):
        for scenario_policy in ("shared", "recourse", "multistage"):
            request = base_request()
            request["request_id"] = f"soc-just-above-max-{scenario_policy}-{delta}"
            request["settings"].update(
                {"mode": "cheap_charge", "formulation": "relaxed"}
            )
            if scenario_policy != "shared":
                request["settings"]["scenario_policy"] = scenario_policy
            request["storages"][0]["initial_energy_wh"] = 9500 + delta

            response = handle(request)

            assert response["ok"], response
            assert response["solver"]["formulation"] == expected_formulations[scenario_policy]
            assert_storage_replays(request, response)


def _multistage_soc_boundary_request(initial_energy_wh: float, backend: str) -> dict:
    request = base_request()
    request["settings"].update(
        {
            "mode": "cheap_charge",
            "formulation": "relaxed",
            "scenario_policy": "multistage",
            "multistage_backend": backend,
        }
    )
    request["slots"] = [
        {
            "start_ms": 1,
            "len_min": 60,
            "price_ore": 20,
            "spot_ore": 10,
            "confidence": 1,
            "pv_w": 0,
            "load_w": 0,
        },
        {
            "start_ms": 3600001,
            "len_min": 60,
            "price_ore": 20,
            "spot_ore": 10,
            "confidence": 1,
            "pv_w": 0,
            "load_w": 0,
        },
    ]
    request["storages"][0].update(
        {
            "capacity_wh": 10000,
            "min_energy_wh": 1000,
            "max_energy_wh": 9500,
            "initial_energy_wh": initial_energy_wh,
            "charge_efficiency": 0.95,
            "discharge_efficiency": 0.95,
            "cycle_cost_ore_kwh": 0,
            "terminal_price_ore_kwh": 0,
        }
    )
    return request


_MULTISTAGE_SOC_BOUNDARY_DELTAS = (0.1e-6, 0.25e-6, 0.5e-6, 0.99e-6, 1e-6)


def _assert_multistage_soc_boundary_case(backend: str, delta: float) -> None:
    request = _multistage_soc_boundary_request(9500 + delta, backend)
    canonical = _canonicalize_storage_payload(request)
    storage = canonical["storages"][0]
    assert storage["initial_energy_wh"] == storage["max_energy_wh"] == 9500.0

    response = handle(request)

    assert response["ok"], response
    assert response["solver"]["formulation"] == "multistage-milp"
    assert_storage_replays(request, response)


@pytest.mark.parametrize("backend", ["auto", "cvxpy"])
@pytest.mark.parametrize(
    "delta",
    _MULTISTAGE_SOC_BOUNDARY_DELTAS,
)
def test_multistage_normalizes_tiny_initial_over_maximum(
    backend: str, delta: float
) -> None:
    clear_multistage_cache()
    _assert_multistage_soc_boundary_case(backend, delta)


@pytest.mark.parametrize("backend", ["auto", "cvxpy"])
def test_multistage_soc_boundary_grid_is_deterministic(backend: str) -> None:
    clear_multistage_cache()
    for _ in range(4):
        for delta in _MULTISTAGE_SOC_BOUNDARY_DELTAS:
            _assert_multistage_soc_boundary_case(backend, delta)


def _burn_cpu(stop: threading.Event) -> None:
    while not stop.is_set():
        sum(value * value for value in range(20_000))


def test_multistage_soc_boundary_grid_survives_cpu_load() -> None:
    clear_multistage_cache()
    stop = threading.Event()
    worker = threading.Thread(target=_burn_cpu, args=(stop,))
    worker.start()
    try:
        for _ in range(2):
            for backend in ("auto", "cvxpy"):
                for delta in _MULTISTAGE_SOC_BOUNDARY_DELTAS:
                    _assert_multistage_soc_boundary_case(backend, delta)
    finally:
        stop.set()
        worker.join(timeout=5)
    assert not worker.is_alive()


@pytest.mark.parametrize("backend", ["auto", "cvxpy"])
def test_multistage_keeps_discrete_guard_for_material_initial_over_maximum(
    backend: str,
) -> None:
    request = _multistage_soc_boundary_request(9800, backend)
    clear_multistage_cache()

    response = handle(request)

    assert response["ok"], response
    assert response["solver"]["formulation"] == "multistage-milp"
    assert_storage_replays(request, response)


def test_multistage_auto_retries_with_storage_guard_after_direct_cycle(monkeypatch) -> None:
    from ftw_optimizer import direct_highs

    clear_multistage_cache()
    request = base_request()
    request["settings"].update(
        {
            "mode": "passive_arbitrage",
            "formulation": "relaxed",
            "scenario_policy": "multistage",
            "multistage_backend": "auto",
        }
    )
    request["storages"][0]["initial_energy_wh"] = 2000

    def reject_simultaneous_cycle(*args, **kwargs):
        raise direct_highs.DirectHighsError(
            "HiGHS returned simultaneous storage charge and discharge"
        )

    monkeypatch.setattr(direct_highs, "solve_direct_highs", reject_simultaneous_cycle)
    response = handle(request)

    assert response["ok"], response
    assert response["solver"]["formulation"] == "multistage-milp"
    assert "simultaneous" in response["solver"]["fallback_reason"]
    assert_storage_replays(request, response)

    cvxpy_request = copy.deepcopy(request)
    cvxpy_request["request_id"] = "multistage-cvxpy-replay-guard"
    cvxpy_request["settings"]["multistage_backend"] = "cvxpy"
    cvxpy_response = handle(cvxpy_request)
    assert cvxpy_response["ok"], cvxpy_response
    assert_storage_replays(cvxpy_request, cvxpy_response)
