from __future__ import annotations

import math
import time
from dataclasses import dataclass
from typing import Any

import cvxpy as cp
import numpy as np

from . import SCHEMA_VERSION
from .protocol import ProtocolError, finite_number, positive_number, require_dict, require_list


OPTIMAL_STATUSES = {cp.OPTIMAL, cp.OPTIMAL_INACCURATE, cp.USER_LIMIT}


@dataclass
class StorageVars:
    spec: dict[str, Any]
    charge: cp.Variable
    discharge: cp.Variable
    energy: cp.Variable


@dataclass
class FlexVars:
    spec: dict[str, Any]
    power: cp.Expression
    energy: cp.Variable
    selection: cp.Variable | None
    shortfall: cp.Variable | None


@dataclass
class ThermalVars:
    spec: dict[str, Any]
    power: cp.Expression
    temperature: cp.Variable
    lower_slack: cp.Variable
    upper_slack: cp.Variable


def _vector(value: Any, n: int, field: str) -> np.ndarray:
    items = require_list(value, field)
    if len(items) != n:
        raise ProtocolError(f"{field} must have {n} entries")
    return np.asarray([finite_number(v, f"{field}[{i}]") for i, v in enumerate(items)])


def _mode(payload: dict[str, Any]) -> str:
    settings = require_dict(payload.get("settings", {}), "settings")
    mode = settings.get("mode", "self_consumption")
    allowed = {"self_consumption", "cheap_charge", "passive_arbitrage", "arbitrage"}
    if mode not in allowed:
        raise ProtocolError(f"unsupported settings.mode {mode!r}")
    return mode


def _export_price(slot: dict[str, Any], settings: dict[str, Any]) -> float:
    flat = finite_number(settings.get("export_ore_per_kwh", 0), "settings.export_ore_per_kwh")
    if flat > 0:
        return flat
    price = finite_number(slot.get("spot_ore", 0), "slot.spot_ore")
    price += finite_number(settings.get("export_bonus_ore_kwh", 0), "settings.export_bonus_ore_kwh")
    price -= finite_number(settings.get("export_fee_ore_kwh", 0), "settings.export_fee_ore_kwh")
    floor = settings.get("export_floor_ore_kwh")
    if floor is not None:
        price = max(price, finite_number(floor, "settings.export_floor_ore_kwh"))
    return price


def _solver_options(settings: dict[str, Any], solver: str) -> dict[str, Any]:
    time_limit = positive_number(settings.get("time_limit_s", 2.0), "settings.time_limit_s")
    if solver == cp.HIGHS:
        return {
            "time_limit": max(0.05, time_limit),
            "mip_rel_gap": max(
                0.0,
                finite_number(settings.get("mip_rel_gap", 0.005), "settings.mip_rel_gap"),
            ),
        }
    return {"time_limit": max(0.05, time_limit)}


def solve(payload: dict[str, Any]) -> dict[str, Any]:
    settings = require_dict(payload.get("settings", {}), "settings")
    scenario_policy = settings.get("scenario_policy", "shared")
    if scenario_policy == "recourse":
        # Imported lazily to keep the shared champion model independent. The
        # challenger is deliberately storage-only until scenario-dependent EV
        # and thermal state can be evaluated against equally stateful telemetry.
        from .recourse import solve_storage_recourse

        return solve_storage_recourse(payload)
    if scenario_policy == "multistage":
        from .multistage import solve_storage_multistage

        return solve_storage_multistage(payload)
    if scenario_policy != "shared":
        raise ProtocolError("settings.scenario_policy must be shared, recourse, or multistage")

    started = time.perf_counter()
    slots = [require_dict(v, f"slots[{i}]") for i, v in enumerate(require_list(payload["slots"], "slots"))]
    n = len(slots)
    mode = _mode(payload)
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
    pv_charge_bonus_ore = max(
        0.0,
        finite_number(
            settings.get("pv_charge_bonus_ore_kwh", 0),
            "settings.pv_charge_bonus_ore_kwh",
        ),
    )
    constraints: list[cp.Constraint] = []
    discrete = False

    storages: list[StorageVars] = []
    asset_ids: set[str] = set()
    total_charge: cp.Expression = cp.Constant(np.zeros(n))
    total_discharge: cp.Expression = cp.Constant(np.zeros(n))
    service_slack: cp.Expression = cp.Constant(0.0)
    for i, raw in enumerate(require_list(payload.get("storages", []), "storages")):
        spec = require_dict(raw, f"storages[{i}]")
        asset_id = spec.get("id")
        if not isinstance(asset_id, str) or not asset_id or asset_id in asset_ids:
            raise ProtocolError(f"storages[{i}].id must be non-empty and unique")
        asset_ids.add(asset_id)
        capacity = positive_number(spec.get("capacity_wh"), f"storages[{i}].capacity_wh")
        min_energy = finite_number(spec.get("min_energy_wh", 0), f"storages[{i}].min_energy_wh")
        max_energy = finite_number(spec.get("max_energy_wh", capacity), f"storages[{i}].max_energy_wh")
        initial = finite_number(spec.get("initial_energy_wh"), f"storages[{i}].initial_energy_wh")
        if not (
            0 <= min_energy <= max_energy <= capacity + 1e-6
            and 0 <= initial <= capacity + 1e-6
        ):
            raise ProtocolError(f"storages[{i}] energy bounds are inconsistent")
        max_charge = max(0.0, finite_number(spec.get("max_charge_w", 0), f"storages[{i}].max_charge_w"))
        max_discharge = max(0.0, finite_number(spec.get("max_discharge_w", 0), f"storages[{i}].max_discharge_w"))
        eta_c = positive_number(spec.get("charge_efficiency", 0.95), f"storages[{i}].charge_efficiency")
        eta_d = positive_number(spec.get("discharge_efficiency", 0.95), f"storages[{i}].discharge_efficiency")
        if eta_c > 1 or eta_d > 1:
            raise ProtocolError(f"storages[{i}] efficiencies must be <= 1")
        charge = cp.Variable(n, nonneg=True, name=f"storage_{i}_charge")
        discharge = cp.Variable(n, nonneg=True, name=f"storage_{i}_discharge")
        energy = cp.Variable(n + 1, name=f"storage_{i}_energy")
        lower_recovery = cp.Variable(n + 1, nonneg=True, name=f"storage_{i}_lower_recovery")
        upper_recovery = cp.Variable(n + 1, nonneg=True, name=f"storage_{i}_upper_recovery")
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
        # A physical SoC can legitimately start beyond a newly configured
        # operating bound. Treat only that initial violation as recoverable:
        # it may never worsen, and once cleared it can never return. In-bound
        # starts have zero recovery allowance, preserving hard min/max bounds.
        service_slack += cp.sum(lower_recovery[1:] + upper_recovery[1:]) / (capacity * n)
        unsafe_cycle = bool(np.any(eff_import < 0)) or pv_charge_bonus_ore > 0
        if force_milp or (formulation == "auto" and unsafe_cycle):
            direction = cp.Variable(n, boolean=True, name=f"storage_{i}_charge_mode")
            constraints += [charge <= max_charge * direction, discharge <= max_discharge * (1 - direction)]
            discrete = True
        target = spec.get("target_energy_wh")
        deadline = int(spec.get("target_slot", n - 1))
        if target is not None:
            deadline = min(n - 1, max(0, deadline))
            shortfall = cp.Variable(nonneg=True, name=f"storage_{i}_shortfall")
            constraints.append(energy[deadline + 1] + shortfall >= finite_number(target, f"storages[{i}].target_energy_wh"))
            service_slack += shortfall / capacity
            spec["_shortfall"] = shortfall
        total_charge += charge
        total_discharge += discharge
        storages.append(StorageVars(spec, charge, discharge, energy))

    flex_loads: list[FlexVars] = []
    total_flex: cp.Expression = cp.Constant(np.zeros(n))
    for i, raw in enumerate(require_list(payload.get("flex_loads", []), "flex_loads")):
        spec = require_dict(raw, f"flex_loads[{i}]")
        asset_id = spec.get("id")
        if not isinstance(asset_id, str) or not asset_id or asset_id in asset_ids:
            raise ProtocolError(f"flex_loads[{i}].id must be non-empty and unique")
        asset_ids.add(asset_id)
        capacity = positive_number(spec.get("capacity_wh"), f"flex_loads[{i}].capacity_wh")
        initial = finite_number(spec.get("initial_energy_wh", 0), f"flex_loads[{i}].initial_energy_wh")
        max_energy = finite_number(spec.get("max_energy_wh", capacity), f"flex_loads[{i}].max_energy_wh")
        eta = positive_number(spec.get("charge_efficiency", 0.9), f"flex_loads[{i}].charge_efficiency")
        raw_steps = require_list(spec.get("allowed_steps_w", []), f"flex_loads[{i}].allowed_steps_w")
        steps = sorted(set(finite_number(v, f"flex_loads[{i}].allowed_steps_w") for v in raw_steps))
        if not steps:
            steps = [0.0, finite_number(spec.get("max_charge_w", 0), f"flex_loads[{i}].max_charge_w")]
        if steps[0] < 0 or 0.0 not in steps:
            raise ProtocolError(f"flex_loads[{i}].allowed_steps_w must contain 0 and be non-negative")
        selection: cp.Variable | None = None
        if formulation == "relaxed":
            power_var = cp.Variable(n, nonneg=True, name=f"flex_{i}_power")
            constraints.append(power_var <= max(steps))
            power: cp.Expression = power_var
        else:
            selection = cp.Variable((len(steps), n), boolean=True, name=f"flex_{i}_step")
            constraints.append(cp.sum(selection, axis=0) == 1)
            power = np.asarray(steps) @ selection
            discrete = True
        spec["_max_charge_w"] = max(steps)
        energy = cp.Variable(n + 1, name=f"flex_{i}_energy")
        constraints += [
            energy[0] == initial,
            energy[1:] == energy[:-1] + cp.multiply(dt_h, eta * power),
            energy >= 0,
            energy <= max_energy,
        ]
        shortfall: cp.Variable | None = None
        target = spec.get("target_energy_wh")
        if target is not None:
            deadline = min(n - 1, max(0, int(spec.get("target_slot", n - 1))))
            shortfall = cp.Variable(nonneg=True, name=f"flex_{i}_shortfall")
            constraints.append(energy[deadline + 1] + shortfall >= finite_number(target, f"flex_loads[{i}].target_energy_wh"))
            service_slack += shortfall / capacity
        total_flex += power
        flex_loads.append(FlexVars(spec, power, energy, selection, shortfall))

    thermal_loads: list[ThermalVars] = []
    total_thermal: cp.Expression = cp.Constant(np.zeros(n))
    for i, raw in enumerate(require_list(payload.get("thermal_loads", []), "thermal_loads")):
        spec = require_dict(raw, f"thermal_loads[{i}]")
        asset_id = spec.get("id")
        if not isinstance(asset_id, str) or not asset_id or asset_id in asset_ids:
            raise ProtocolError(f"thermal_loads[{i}].id must be non-empty and unique")
        asset_ids.add(asset_id)
        initial = finite_number(spec.get("initial_temp_c"), f"thermal_loads[{i}].initial_temp_c")
        min_temp = finite_number(spec.get("min_temp_c"), f"thermal_loads[{i}].min_temp_c")
        max_temp = finite_number(spec.get("max_temp_c"), f"thermal_loads[{i}].max_temp_c")
        if min_temp >= max_temp:
            raise ProtocolError(f"thermal_loads[{i}] temperature bounds are inconsistent")
        outside = _vector(spec.get("outside_temp_c", [initial] * n), n, f"thermal_loads[{i}].outside_temp_c")
        steps_raw = require_list(spec.get("allowed_steps_w", []), f"thermal_loads[{i}].allowed_steps_w")
        if steps_raw and formulation != "relaxed":
            steps = sorted(set(finite_number(v, f"thermal_loads[{i}].allowed_steps_w") for v in steps_raw))
            if steps[0] < 0 or 0.0 not in steps:
                raise ProtocolError(f"thermal_loads[{i}].allowed_steps_w must contain 0")
            selection = cp.Variable((len(steps), n), boolean=True, name=f"thermal_{i}_step")
            constraints.append(cp.sum(selection, axis=0) == 1)
            power = np.asarray(steps) @ selection
            discrete = True
        else:
            power_var = cp.Variable(n, nonneg=True, name=f"thermal_{i}_power")
            constraints.append(power_var <= positive_number(spec.get("max_power_w"), f"thermal_loads[{i}].max_power_w"))
            power = power_var
        gain = positive_number(spec.get("gain_c_per_kwh"), f"thermal_loads[{i}].gain_c_per_kwh")
        loss = max(0.0, finite_number(spec.get("loss_per_hour", 0), f"thermal_loads[{i}].loss_per_hour"))
        temp = cp.Variable(n + 1, name=f"thermal_{i}_temp")
        lower_slack = cp.Variable(n + 1, nonneg=True, name=f"thermal_{i}_lower_slack")
        upper_slack = cp.Variable(n + 1, nonneg=True, name=f"thermal_{i}_upper_slack")
        constraints.append(temp[0] == initial)
        for t in range(n):
            constraints.append(
                temp[t + 1]
                == temp[t] + gain * power[t] * dt_h[t] / 1000.0 - loss * (temp[t] - outside[t]) * dt_h[t]
            )
        constraints += [temp + lower_slack >= min_temp, temp - upper_slack <= max_temp]
        service_slack += cp.sum(lower_slack + upper_slack) / ((max_temp - min_temp) * (n + 1))
        total_thermal += power
        thermal_loads.append(ThermalVars(spec, power, temp, lower_slack, upper_slack))

    # Curtailment is a shared schedule, so it must be feasible in every
    # scenario. Bounding it by base PV alone would turn excess curtailment into
    # a phantom load in a downside-PV scenario.
    pv_generation = np.minimum.reduce(
        [np.maximum(0.0, -scenario["pv"]) for scenario in scenarios]
    )
    curtail = cp.Variable(n, nonneg=True, name="pv_curtail")
    constraints.append(curtail <= pv_generation)

    scenario_vars: list[dict[str, Any]] = []
    expected_cost: cp.Expression = cp.Constant(0.0)
    strict_sc_penalty: cp.Expression = cp.Constant(0.0)
    max_site_power = max(
        1000.0,
        float(np.max(base_load + pv_generation))
        + sum(float(s.get("max_charge_w", 0)) + float(s.get("max_discharge_w", 0)) for s in payload.get("storages", []))
        + sum(max([float(f.get("max_charge_w", 0))] + [float(v) for v in f.get("allowed_steps_w", [])]) for f in payload.get("flex_loads", []))
        + sum(max([float(t.get("max_power_w", 0))] + [float(v) for v in t.get("allowed_steps_w", [])]) for t in payload.get("thermal_loads", [])),
    )
    for si, scenario in enumerate(scenarios):
        grid_import = cp.Variable(n, nonneg=True, name=f"scenario_{si}_import")
        grid_export = cp.Variable(n, nonneg=True, name=f"scenario_{si}_export")
        net_without_storage = scenario["load"] + scenario["pv"] + curtail + total_flex + total_thermal
        constraints.append(grid_import - grid_export == net_without_storage + total_charge - total_discharge)

        import_limit = np.asarray(
            [max(0.0, finite_number(s.get("max_import_w", 0), f"slots[{t}].max_import_w")) for t, s in enumerate(slots)]
        )
        export_limit = np.asarray(
            [max(0.0, finite_number(s.get("max_export_w", 0), f"slots[{t}].max_export_w")) for t, s in enumerate(slots)]
        )
        constraints += [
            grid_import <= np.where(import_limit > 0, import_limit, max_site_power),
            grid_export <= np.where(export_limit > 0, export_limit, max_site_power),
        ]
        unsafe_meter_split = bool(np.any(eff_import < eff_export - 1e-9))
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
        expected_cost += scenario["probability"] * scenario_cost
        if mode in {"self_consumption", "passive_arbitrage"}:
            house_import = cp.Variable(n, nonneg=True, name=f"scenario_{si}_house_import")
            constraints.append(house_import >= scenario["load"] + scenario["pv"] + curtail + total_charge - total_discharge)
            strict_sc_penalty += scenario["probability"] * cp.sum(
                cp.multiply(dt_h / 1000.0, cp.multiply(2.0 * np.maximum(eff_import, 0.0), house_import))
            )
        scenario_vars.append({"import": grid_import, "export": grid_export, "cost": scenario_cost})

    storage_charge_active: cp.Variable | None = None
    storage_discharge_active: cp.Variable | None = None
    if storages and flex_loads and formulation != "relaxed":
        storage_charge_active = cp.Variable(n, boolean=True, name="fleet_charge_active")
        storage_discharge_active = cp.Variable(n, boolean=True, name="fleet_discharge_active")
        constraints += [
            total_charge <= max_site_power * storage_charge_active,
            total_discharge <= max_site_power * storage_discharge_active,
            storage_charge_active + storage_discharge_active <= 1,
        ]
        discrete = True

    has_surplus_only = any(bool(flex.spec.get("surplus_only", False)) for flex in flex_loads)
    if has_surplus_only and storage_charge_active is not None:
        # A connected surplus-only loadpoint also forbids grid-funded home-
        # battery charging, even in a slot where the EV happens to be off.
        for sv in scenario_vars:
            constraints.append(sv["import"] <= max_site_power * (1 - storage_charge_active))

    for flex in flex_loads:
        if flex.selection is None:
            active = flex.power / max(1.0, float(flex.spec["_max_charge_w"]))
        else:
            steps = sorted(set(float(v) for v in flex.spec.get("allowed_steps_w", [])))
            zero_idx = steps.index(0.0)
            active = 1 - flex.selection[zero_idx, :]
        if bool(flex.spec.get("surplus_only", False)):
            for sv in scenario_vars:
                constraints.append(sv["import"] <= max_site_power * (1 - active))
        if bool(flex.spec.get("no_storage_to_load", False)) and storages:
            house_residual = np.maximum(0.0, base_load + base_pv)
            constraints.append(total_discharge <= house_residual + max_site_power * (1 - active))
        if storage_discharge_active is not None:
            # EV charging may coexist with house-covering discharge, but not
            # with battery-driven site export.
            for sv in scenario_vars:
                constraints.append(
                    sv["export"]
                    <= max_site_power * (2 - active - storage_discharge_active)
                )

    cycle_cost = cp.Constant(0.0)
    terminal_credit = cp.Constant(0.0)
    for storage in storages:
        cycle_ore = max(0.0, finite_number(storage.spec.get("cycle_cost_ore_kwh", 0), "storage.cycle_cost_ore_kwh"))
        cycle_ore += max(0.0, finite_number(settings.get("min_arbitrage_spread_ore_kwh", 0), "settings.min_arbitrage_spread_ore_kwh"))
        cycle_cost += cycle_ore * cp.sum(cp.multiply(dt_h, storage.discharge)) / 1000.0
        terminal_price = finite_number(storage.spec.get("terminal_price_ore_kwh", 0), "storage.terminal_price_ore_kwh")
        terminal_credit += terminal_price * storage.energy[-1] / 1000.0

    pv_bonus = cp.Constant(0.0)
    bonus_ore = pv_charge_bonus_ore
    if bonus_ore > 0 and storages:
        charge_from_pv = cp.Variable(n, nonneg=True, name="charge_from_pv")
        constraints += [charge_from_pv <= total_charge, charge_from_pv <= np.maximum(0.0, -base_pv - base_load)]
        pv_bonus = bonus_ore * cp.sum(cp.multiply(dt_h, charge_from_pv)) / 1000.0

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

    cost_objective = expected_cost + strict_sc_penalty + cycle_cost - terminal_credit - pv_bonus + risk_cost
    slack_problem = cp.Problem(cp.Minimize(service_slack), constraints)
    preferred_solver = str(settings.get("solver", "HIGHS")).upper()
    if preferred_solver not in {"HIGHS", "CLARABEL"}:
        raise ProtocolError("settings.solver must be HIGHS or CLARABEL")
    if discrete and preferred_solver == "CLARABEL":
        preferred_solver = "HIGHS"

    def run_problem(problem: cp.Problem, solver_name: str) -> None:
        solver = cp.HIGHS if solver_name == "HIGHS" else cp.CLARABEL
        problem.solve(solver=solver, warm_start=True, **_solver_options(settings, solver))

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
    constraints.append(service_slack <= best_slack + 1e-7)

    cost_problem = cp.Problem(cp.Minimize(cost_objective), constraints)
    try:
        run_problem(cost_problem, solver_used)
    except cp.error.SolverError:
        if discrete or solver_used == "CLARABEL":
            raise
        solver_used = "CLARABEL"
        run_problem(cost_problem, solver_used)
    if cost_problem.status not in OPTIMAL_STATUSES or cost_problem.value is None:
        raise RuntimeError(f"economic solve failed with status {cost_problem.status}")

    base_scenario_index = next((i for i, s in enumerate(scenarios) if s["id"] == "base"), 0)
    base_scenario = scenarios[base_scenario_index]
    base_vars = scenario_vars[base_scenario_index]
    total_capacity = sum(float(s.spec["capacity_wh"]) for s in storages)
    initial_total = sum(float(s.spec["initial_energy_wh"]) for s in storages)
    actions: list[dict[str, Any]] = []
    raw_total_cost = 0.0
    for t, slot in enumerate(slots):
        storage_power: dict[str, float] = {}
        storage_energy: dict[str, float] = {}
        battery_w = 0.0
        stored_wh = 0.0
        for i, storage in enumerate(storages):
            power = float(storage.charge.value[t] - storage.discharge.value[t])
            energy = float(storage.energy.value[t + 1])
            storage_id = str(storage.spec.get("id", f"storage-{i}"))
            storage_power[storage_id] = power
            storage_energy[storage_id] = energy
            battery_w += power
            stored_wh += energy
        flex_power: dict[str, float] = {}
        flex_energy: dict[str, float] = {}
        for i, flex in enumerate(flex_loads):
            flex_id = str(flex.spec.get("id", f"flex-{i}"))
            flex_power[flex_id] = float(flex.power.value[t])
            flex_energy[flex_id] = float(flex.energy.value[t + 1])
        thermal_power: dict[str, float] = {}
        thermal_state: dict[str, float] = {}
        for i, thermal in enumerate(thermal_loads):
            thermal_id = str(thermal.spec.get("id", f"thermal-{i}"))
            thermal_power[thermal_id] = float(thermal.power.value[t])
            thermal_state[thermal_id] = float(thermal.temperature.value[t + 1])
        grid_w = float(base_vars["import"].value[t] - base_vars["export"].value[t])
        grid_kwh = grid_w * dt_h[t] / 1000.0
        raw_cost = price[t] * max(grid_kwh, 0.0) - export_price[t] * max(-grid_kwh, 0.0)
        raw_total_cost += raw_cost
        curtailed_w = max(0.0, float(curtail.value[t]))
        actions.append(
            {
                "slot_start_ms": int(slot.get("start_ms", 0)),
                "slot_len_min": int(slot["len_min"]),
                "battery_w": battery_w,
                "grid_w": grid_w,
                "soc_pct": (stored_wh / total_capacity * 100.0) if total_capacity > 0 else 0.0,
                "cost_ore": raw_cost,
                "pv_limit_w": max(0.0, -base_pv[t] - curtailed_w) if curtailed_w > 1e-5 else 0.0,
                "storage_power_w": storage_power,
                "storage_energy_wh": storage_energy,
                "flex_power_w": flex_power,
                "flex_energy_wh": flex_energy,
                "thermal_power_w": thermal_power,
                "thermal_state": thermal_state,
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
            "formulation": "milp" if discrete else "convex",
            "objective_ore": float(cost_problem.value),
            "service_slack": best_slack,
            "solve_ms": solve_ms,
            "mip_gap": mip_gap,
            "scenario_count": len(scenarios),
            "scenario_policy": "shared",
            "policy_version": "shared-v1",
            "non_anticipative_slots": n,
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
