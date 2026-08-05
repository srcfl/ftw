from __future__ import annotations

import copy
import math
import threading

import numpy as np
import pytest

from ftw_optimizer.multistage import clear_multistage_cache
from ftw_optimizer.model import _canonicalize_storage_payload
from ftw_optimizer.scenario_tree import (
    Scenario,
    build_scenario_tree,
    decision_blocks,
    reduce_scenarios,
)
from ftw_optimizer.worker import handle, handshake


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
    assert direct["solver"]["formulation"] == "multistage-lp"
    assert direct["solver"]["dpp"] is False
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


def test_multistage_auto_keeps_binary_guards_for_unsafe_incentives() -> None:
    negative_price = base_request()
    negative_price["settings"]["scenario_policy"] = "multistage"
    negative_price["slots"][0]["price_ore"] = -10
    response = handle(negative_price)
    assert response["ok"], response
    assert response["solver"]["engine"] == "cvxpy"
    assert response["solver"]["formulation"] == "multistage-milp"

    shared_bonus = base_request()
    shared_bonus["settings"]["pv_charge_bonus_ore_kwh"] = 1
    response = handle(shared_bonus)
    assert response["ok"], response
    assert response["solver"]["formulation"] == "milp"

    recourse_bonus = base_request()
    recourse_bonus["settings"].update(
        {"scenario_policy": "recourse", "pv_charge_bonus_ore_kwh": 1}
    )
    response = handle(recourse_bonus)
    assert response["ok"], response
    assert response["solver"]["formulation"] == "stochastic-recourse-milp"

    pv_bonus = base_request()
    pv_bonus["settings"].update(
        {"scenario_policy": "multistage", "pv_charge_bonus_ore_kwh": 1}
    )
    response = handle(pv_bonus)
    assert response["ok"], response
    assert response["solver"]["engine"] == "cvxpy"
    assert response["solver"]["formulation"] == "multistage-milp"


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


def test_surplus_only_connection_blocks_grid_funded_battery_charge() -> None:
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
        }
    ]
    response = handle(request)
    assert response["ok"], response
    assert all(a["battery_w"] <= 1e-5 for a in response["plan"]["actions"])


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
