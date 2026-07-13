from __future__ import annotations

import math
import time
from dataclasses import dataclass
from typing import Any

import cvxpy as cp
import numpy as np

from . import SCHEMA_VERSION
from .model import OPTIMAL_STATUSES, _export_price, _mode, _solver_options, _vector
from .protocol import ProtocolError, finite_number, positive_number, require_dict, require_list


@dataclass
class ScenarioStorage:
    spec: dict[str, Any]
    charge: cp.Variable
    discharge: cp.Variable
    energy: cp.Variable


def solve_storage_recourse(payload: dict[str, Any]) -> dict[str, Any]:
    """Solve a two-stage stochastic storage problem.

    Decisions in the configured non-anticipative prefix are shared across all
    scenarios. Storage, grid, and curtailment decisions after that prefix are
    scenario-specific recourse. The base-scenario path is returned, but only
    its shared first-stage action is intended for execution before replanning.
    """

    started = time.perf_counter()
    settings = require_dict(payload.get("settings", {}), "settings")
    if require_list(payload.get("flex_loads", []), "flex_loads"):
        raise ProtocolError("recourse shadow does not yet support flex_loads")
    if require_list(payload.get("thermal_loads", []), "thermal_loads"):
        raise ProtocolError("recourse shadow does not yet support thermal_loads")

    slots = [
        require_dict(v, f"slots[{i}]")
        for i, v in enumerate(require_list(payload["slots"], "slots"))
    ]
    n = len(slots)
    mode = _mode(payload)
    prefix_value = finite_number(settings.get("non_anticipative_slots", 1), "settings.non_anticipative_slots")
    prefix = int(prefix_value)
    if prefix_value != prefix:
        raise ProtocolError("settings.non_anticipative_slots must be an integer")
    if prefix < 1 or prefix > n:
        raise ProtocolError("settings.non_anticipative_slots must be in [1, len(slots)]")

    dt_h = np.asarray(
        [positive_number(s.get("len_min", 0), f"slots[{i}].len_min") / 60.0 for i, s in enumerate(slots)]
    )
    price = np.asarray(
        [finite_number(s.get("price_ore"), f"slots[{i}].price_ore") for i, s in enumerate(slots)]
    )
    confidence = np.asarray(
        [min(1.0, max(0.0, finite_number(s.get("confidence", 1), f"slots[{i}].confidence"))) for i, s in enumerate(slots)]
    )
    confidence[confidence == 0] = 1.0
    export_price = np.asarray([_export_price(s, settings) for s in slots])
    eff_import = confidence * price + (1.0 - confidence) * float(np.mean(price))
    eff_export = confidence * export_price + (1.0 - confidence) * float(np.mean(export_price))

    base_load = np.asarray(
        [finite_number(s.get("load_w", 0), f"slots[{i}].load_w") for i, s in enumerate(slots)]
    )
    base_pv = np.asarray(
        [finite_number(s.get("pv_w", 0), f"slots[{i}].pv_w") for i, s in enumerate(slots)]
    )
    if np.any(base_load < -1e-9) or np.any(base_pv > 1e-9):
        raise ProtocolError("site convention requires load_w >= 0 and pv_w <= 0")

    raw_scenarios = require_list(payload.get("scenarios", []), "scenarios")
    scenarios: list[dict[str, Any]] = []
    if raw_scenarios:
        for i, raw in enumerate(raw_scenarios):
            spec = require_dict(raw, f"scenarios[{i}]")
            scenarios.append(
                {
                    "id": str(spec.get("id", f"scenario-{i}")),
                    "probability": positive_number(spec.get("probability", 0), f"scenarios[{i}].probability"),
                    "load": _vector(spec.get("load_w"), n, f"scenarios[{i}].load_w"),
                    "pv": _vector(spec.get("pv_w"), n, f"scenarios[{i}].pv_w"),
                }
            )
    else:
        scenarios.append({"id": "base", "probability": 1.0, "load": base_load, "pv": base_pv})
    probability_sum = sum(s["probability"] for s in scenarios)
    for scenario in scenarios:
        scenario["probability"] /= probability_sum
        if np.any(scenario["load"] < -1e-9) or np.any(scenario["pv"] > 1e-9):
            raise ProtocolError(f"scenario {scenario['id']} violates site sign convention")

    formulation = settings.get("formulation", "auto")
    if formulation not in {"auto", "milp", "relaxed"}:
        raise ProtocolError("settings.formulation must be auto, milp, or relaxed")
    force_milp = formulation == "milp"
    constraints: list[cp.Constraint] = []
    discrete = False
    storage_specs = [
        require_dict(raw, f"storages[{i}]")
        for i, raw in enumerate(require_list(payload.get("storages", []), "storages"))
    ]
    asset_ids: set[str] = set()
    for i, spec in enumerate(storage_specs):
        asset_id = spec.get("id")
        if not isinstance(asset_id, str) or not asset_id or asset_id in asset_ids:
            raise ProtocolError(f"storages[{i}].id must be non-empty and unique")
        asset_ids.add(asset_id)

    max_site_power = max(
        1000.0,
        max(float(np.max(s["load"] + np.maximum(0.0, -s["pv"]))) for s in scenarios)
        + sum(float(s.get("max_charge_w", 0)) + float(s.get("max_discharge_w", 0)) for s in storage_specs),
    )
    import_limit = np.asarray(
        [max(0.0, finite_number(s.get("max_import_w", 0), f"slots[{t}].max_import_w")) for t, s in enumerate(slots)]
    )
    export_limit = np.asarray(
        [max(0.0, finite_number(s.get("max_export_w", 0), f"slots[{t}].max_export_w")) for t, s in enumerate(slots)]
    )

    scenario_vars: list[dict[str, Any]] = []
    expected_cost: cp.Expression = cp.Constant(0.0)
    expected_cycle_cost: cp.Expression = cp.Constant(0.0)
    expected_terminal_credit: cp.Expression = cp.Constant(0.0)
    expected_pv_bonus: cp.Expression = cp.Constant(0.0)
    strict_sc_penalty: cp.Expression = cp.Constant(0.0)
    worst_service_slack = cp.Variable(nonneg=True, name="worst_service_slack")
    unsafe_cycle = bool(np.any(eff_import < 0))
    unsafe_meter_split = bool(np.any(eff_import < eff_export - 1e-9))
    bonus_ore = max(0.0, finite_number(settings.get("pv_charge_bonus_ore_kwh", 0), "settings.pv_charge_bonus_ore_kwh"))

    for si, scenario in enumerate(scenarios):
        probability = float(scenario["probability"])
        storages: list[ScenarioStorage] = []
        total_charge: cp.Expression = cp.Constant(np.zeros(n))
        total_discharge: cp.Expression = cp.Constant(np.zeros(n))
        scenario_service: cp.Expression = cp.Constant(0.0)
        scenario_cycle: cp.Expression = cp.Constant(0.0)
        scenario_terminal: cp.Expression = cp.Constant(0.0)

        for i, spec in enumerate(storage_specs):
            capacity = positive_number(spec.get("capacity_wh"), f"storages[{i}].capacity_wh")
            min_energy = finite_number(spec.get("min_energy_wh", 0), f"storages[{i}].min_energy_wh")
            max_energy = finite_number(spec.get("max_energy_wh", capacity), f"storages[{i}].max_energy_wh")
            initial = finite_number(spec.get("initial_energy_wh"), f"storages[{i}].initial_energy_wh")
            if not (0 <= min_energy <= max_energy <= capacity + 1e-6 and 0 <= initial <= capacity + 1e-6):
                raise ProtocolError(f"storages[{i}] energy bounds are inconsistent")
            max_charge = max(0.0, finite_number(spec.get("max_charge_w", 0), f"storages[{i}].max_charge_w"))
            max_discharge = max(0.0, finite_number(spec.get("max_discharge_w", 0), f"storages[{i}].max_discharge_w"))
            eta_c = positive_number(spec.get("charge_efficiency", 0.95), f"storages[{i}].charge_efficiency")
            eta_d = positive_number(spec.get("discharge_efficiency", 0.95), f"storages[{i}].discharge_efficiency")
            if eta_c > 1 or eta_d > 1:
                raise ProtocolError(f"storages[{i}] efficiencies must be <= 1")

            charge = cp.Variable(n, nonneg=True, name=f"scenario_{si}_storage_{i}_charge")
            discharge = cp.Variable(n, nonneg=True, name=f"scenario_{si}_storage_{i}_discharge")
            energy = cp.Variable(n + 1, name=f"scenario_{si}_storage_{i}_energy")
            lower_recovery = cp.Variable(n + 1, nonneg=True, name=f"scenario_{si}_storage_{i}_lower_recovery")
            upper_recovery = cp.Variable(n + 1, nonneg=True, name=f"scenario_{si}_storage_{i}_upper_recovery")
            constraints += [
                energy[0] == initial,
                energy[1:] == energy[:-1] + cp.multiply(dt_h, eta_c * charge - discharge / eta_d),
                energy >= 0,
                energy <= capacity,
                charge <= max_charge,
                discharge <= max_discharge,
                lower_recovery[0] == max(0.0, min_energy - initial),
                upper_recovery[0] == max(0.0, initial - max_energy),
                lower_recovery >= min_energy - energy,
                upper_recovery >= energy - max_energy,
                lower_recovery[1:] <= lower_recovery[:-1],
                upper_recovery[1:] <= upper_recovery[:-1],
            ]
            scenario_service += cp.sum(lower_recovery[1:] + upper_recovery[1:]) / (capacity * n)
            if force_milp or (formulation == "auto" and unsafe_cycle):
                direction = cp.Variable(n, boolean=True, name=f"scenario_{si}_storage_{i}_charge_mode")
                constraints += [charge <= max_charge * direction, discharge <= max_discharge * (1 - direction)]
                discrete = True
            target = spec.get("target_energy_wh")
            if target is not None:
                deadline = min(n - 1, max(0, int(spec.get("target_slot", n - 1))))
                shortfall = cp.Variable(nonneg=True, name=f"scenario_{si}_storage_{i}_shortfall")
                constraints.append(energy[deadline + 1] + shortfall >= finite_number(target, f"storages[{i}].target_energy_wh"))
                scenario_service += shortfall / capacity

            cycle_ore = max(0.0, finite_number(spec.get("cycle_cost_ore_kwh", 0), "storage.cycle_cost_ore_kwh"))
            cycle_ore += max(0.0, finite_number(settings.get("min_arbitrage_spread_ore_kwh", 0), "settings.min_arbitrage_spread_ore_kwh"))
            scenario_cycle += cycle_ore * cp.sum(cp.multiply(dt_h, discharge)) / 1000.0
            terminal_price = finite_number(spec.get("terminal_price_ore_kwh", 0), "storage.terminal_price_ore_kwh")
            scenario_terminal += terminal_price * energy[-1] / 1000.0
            total_charge += charge
            total_discharge += discharge
            storages.append(ScenarioStorage(spec, charge, discharge, energy))

        constraints.append(scenario_service <= worst_service_slack)
        pv_generation = np.maximum(0.0, -scenario["pv"])
        curtail = cp.Variable(n, nonneg=True, name=f"scenario_{si}_pv_curtail")
        constraints.append(curtail <= pv_generation)
        grid_import = cp.Variable(n, nonneg=True, name=f"scenario_{si}_import")
        grid_export = cp.Variable(n, nonneg=True, name=f"scenario_{si}_export")
        net_without_storage = scenario["load"] + scenario["pv"] + curtail
        constraints += [
            grid_import - grid_export == net_without_storage + total_charge - total_discharge,
            grid_import <= np.where(import_limit > 0, import_limit, max_site_power),
            grid_export <= np.where(export_limit > 0, export_limit, max_site_power),
        ]
        if force_milp or (formulation == "auto" and unsafe_meter_split):
            direction = cp.Variable(n, boolean=True, name=f"scenario_{si}_import_mode")
            constraints += [grid_import <= max_site_power * direction, grid_export <= max_site_power * (1 - direction)]
            discrete = True

        if mode in {"self_consumption", "cheap_charge", "passive_arbitrage"}:
            base_import = cp.Variable(n, nonneg=True, name=f"scenario_{si}_base_import")
            base_export = cp.Variable(n, nonneg=True, name=f"scenario_{si}_base_export")
            constraints.append(base_import - base_export == net_without_storage)
            base_direction = cp.Variable(n, boolean=True, name=f"scenario_{si}_base_import_mode")
            constraints += [
                base_import <= max_site_power * base_direction,
                base_export <= max_site_power * (1 - base_direction),
            ]
            discrete = True
            if mode == "self_consumption":
                constraints += [grid_import <= base_import + 50.0, grid_export <= base_export + 50.0]
            else:
                constraints.append(grid_export <= base_export + 1e-6)

        scenario_cost = cp.sum(
            cp.multiply(dt_h / 1000.0, cp.multiply(eff_import, grid_import) - cp.multiply(eff_export, grid_export))
        )
        expected_cost += probability * scenario_cost
        expected_cycle_cost += probability * scenario_cycle
        expected_terminal_credit += probability * scenario_terminal
        if mode in {"self_consumption", "passive_arbitrage"}:
            house_import = cp.Variable(n, nonneg=True, name=f"scenario_{si}_house_import")
            constraints.append(house_import >= scenario["load"] + scenario["pv"] + curtail + total_charge - total_discharge)
            strict_sc_penalty += probability * cp.sum(
                cp.multiply(dt_h / 1000.0, cp.multiply(2.0 * np.maximum(eff_import, 0.0), house_import))
            )
        if bonus_ore > 0 and storages:
            charge_from_pv = cp.Variable(n, nonneg=True, name=f"scenario_{si}_charge_from_pv")
            constraints += [charge_from_pv <= total_charge, charge_from_pv <= np.maximum(0.0, -scenario["pv"] - scenario["load"])]
            expected_pv_bonus += probability * bonus_ore * cp.sum(cp.multiply(dt_h, charge_from_pv)) / 1000.0
        scenario_vars.append(
            {
                "storages": storages,
                "charge": total_charge,
                "discharge": total_discharge,
                "import": grid_import,
                "export": grid_export,
                "curtail": curtail,
                "cost": scenario_cost,
            }
        )

    # All decisions made before the first new observation must be identical.
    # Tying charge and discharge (rather than only net power) also prevents
    # hidden anticipativity through simultaneous cycling in relaxed models.
    for si in range(1, len(scenarios)):
        for storage_i in range(len(storage_specs)):
            base_storage = scenario_vars[0]["storages"][storage_i]
            other_storage = scenario_vars[si]["storages"][storage_i]
            constraints += [
                other_storage.charge[:prefix] == base_storage.charge[:prefix],
                other_storage.discharge[:prefix] == base_storage.discharge[:prefix],
            ]
        constraints.append(scenario_vars[si]["curtail"][:prefix] == scenario_vars[0]["curtail"][:prefix])

    risk_weight = max(0.0, finite_number(settings.get("cvar_weight", 0), "settings.cvar_weight"))
    risk_cost: cp.Expression = cp.Constant(0.0)
    if risk_weight > 0 and len(scenarios) > 1:
        alpha = finite_number(settings.get("cvar_alpha", 0.9), "settings.cvar_alpha")
        if not 0 < alpha < 1:
            raise ProtocolError("settings.cvar_alpha must be between 0 and 1")
        threshold = cp.Variable(name="cvar_threshold")
        excess = cp.Variable(len(scenarios), nonneg=True, name="cvar_excess")
        constraints += [excess[i] >= scenario_vars[i]["cost"] - threshold for i in range(len(scenarios))]
        probabilities = np.asarray([s["probability"] for s in scenarios])
        risk_cost = risk_weight * (threshold + probabilities @ excess / (1.0 - alpha))

    preferred_solver = str(settings.get("solver", "HIGHS")).upper()
    if preferred_solver not in {"HIGHS", "CLARABEL"}:
        raise ProtocolError("settings.solver must be HIGHS or CLARABEL")
    if discrete and preferred_solver == "CLARABEL":
        preferred_solver = "HIGHS"

    def run_problem(problem: cp.Problem, solver_name: str) -> None:
        solver = cp.HIGHS if solver_name == "HIGHS" else cp.CLARABEL
        problem.solve(solver=solver, warm_start=True, **_solver_options(settings, solver))

    slack_problem = cp.Problem(cp.Minimize(worst_service_slack), constraints)
    solver_used = preferred_solver
    try:
        run_problem(slack_problem, solver_used)
    except cp.error.SolverError:
        if discrete or solver_used == "CLARABEL":
            raise
        solver_used = "CLARABEL"
        run_problem(slack_problem, solver_used)
    if slack_problem.status not in OPTIMAL_STATUSES or slack_problem.value is None:
        raise RuntimeError(f"service-level solve failed with status {slack_problem.status}")
    best_slack = max(0.0, float(slack_problem.value))
    constraints.append(worst_service_slack <= best_slack + 1e-7)

    objective = expected_cost + strict_sc_penalty + expected_cycle_cost - expected_terminal_credit - expected_pv_bonus + risk_cost
    cost_problem = cp.Problem(cp.Minimize(objective), constraints)
    try:
        run_problem(cost_problem, solver_used)
    except cp.error.SolverError:
        if discrete or solver_used == "CLARABEL":
            raise
        solver_used = "CLARABEL"
        run_problem(cost_problem, solver_used)
    if cost_problem.status not in OPTIMAL_STATUSES or cost_problem.value is None:
        raise RuntimeError(f"economic solve failed with status {cost_problem.status}")

    base_index = next((i for i, s in enumerate(scenarios) if s["id"] == "base"), 0)
    base = scenarios[base_index]
    base_vars = scenario_vars[base_index]
    total_capacity = sum(float(s["capacity_wh"]) for s in storage_specs)
    initial_total = sum(float(s["initial_energy_wh"]) for s in storage_specs)
    actions: list[dict[str, Any]] = []
    raw_total_cost = 0.0
    for t, slot in enumerate(slots):
        storage_power: dict[str, float] = {}
        storage_energy: dict[str, float] = {}
        battery_w = 0.0
        stored_wh = 0.0
        for i, storage in enumerate(base_vars["storages"]):
            power = float(storage.charge.value[t] - storage.discharge.value[t])
            energy = float(storage.energy.value[t + 1])
            storage_id = str(storage.spec.get("id", f"storage-{i}"))
            storage_power[storage_id] = power
            storage_energy[storage_id] = energy
            battery_w += power
            stored_wh += energy
        grid_w = float(base_vars["import"].value[t] - base_vars["export"].value[t])
        grid_kwh = grid_w * dt_h[t] / 1000.0
        raw_cost = price[t] * max(grid_kwh, 0.0) - export_price[t] * max(-grid_kwh, 0.0)
        raw_total_cost += raw_cost
        curtailed_w = max(0.0, float(base_vars["curtail"].value[t]))
        actions.append(
            {
                "slot_start_ms": int(slot.get("start_ms", 0)),
                "slot_len_min": int(slot["len_min"]),
                "battery_w": battery_w,
                "grid_w": grid_w,
                "soc_pct": (stored_wh / total_capacity * 100.0) if total_capacity > 0 else 0.0,
                "cost_ore": raw_cost,
                "pv_limit_w": max(0.0, -base["pv"][t] - curtailed_w) if curtailed_w > 1e-5 else 0.0,
                "storage_power_w": storage_power,
                "storage_energy_wh": storage_energy,
                "flex_power_w": {},
                "flex_energy_wh": {},
                "thermal_power_w": {},
                "thermal_state": {},
            }
        )

    extra = getattr(cost_problem.solver_stats, "extra_stats", None)
    mip_gap = None
    if extra is not None:
        for name in ("mip_gap", "mip_rel_gap"):
            value = getattr(extra, name, None)
            if value is not None and math.isfinite(float(value)):
                mip_gap = float(value)
                break
    solve_ms = (time.perf_counter() - started) * 1000.0
    return {
        "schema_version": SCHEMA_VERSION,
        "request_id": str(payload["request_id"]),
        "ok": True,
        "solver": {
            "engine": "cvxpy",
            "backend": solver_used.lower(),
            "status": str(cost_problem.status),
            "formulation": "stochastic-recourse-milp" if discrete else "stochastic-recourse-convex",
            "objective_ore": float(cost_problem.value),
            "service_slack": best_slack,
            "solve_ms": solve_ms,
            "mip_gap": mip_gap,
            "scenario_count": len(scenarios),
            "scenario_policy": "recourse",
            "policy_version": "storage-recourse-v1",
            "non_anticipative_slots": prefix,
            "cvar_weight": risk_weight,
            "cvar_alpha": finite_number(settings.get("cvar_alpha", 0.9), "settings.cvar_alpha"),
        },
        "plan": {
            "mode": mode,
            "horizon_slots": n,
            "capacity_wh": total_capacity,
            "initial_soc_pct": (initial_total / total_capacity * 100.0) if total_capacity > 0 else 0.0,
            "total_cost_ore": raw_total_cost,
            "actions": actions,
        },
    }
