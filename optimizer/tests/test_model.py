from __future__ import annotations

import math

from ftw_optimizer.worker import handle


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


def test_storage_above_maximum_recovers_without_worsening() -> None:
    request = base_request()
    request["storages"][0]["initial_energy_wh"] = 9800
    response = handle(request)
    assert response["ok"], response
    energies = [action["storage_energy_wh"]["home"] for action in response["plan"]["actions"]]
    assert energies[0] <= 9800 + 1e-5
    assert energies[-1] <= 9500 + 1e-5
