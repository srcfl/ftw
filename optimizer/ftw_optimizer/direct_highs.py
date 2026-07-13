from __future__ import annotations

import math
import time
from dataclasses import dataclass
from typing import Any, TYPE_CHECKING

import highspy
import numpy as np

from . import SCHEMA_VERSION
from .model import _solver_options
from .protocol import finite_number

if TYPE_CHECKING:
    from .multistage import PreparedMultistage


class DirectHighsError(RuntimeError):
    pass


@dataclass
class DirectScenarioVars:
    charge: list[list[int]]
    discharge: list[list[int]]
    energy: list[list[int]]
    curtail: list[int]
    grid_import: list[int]
    grid_export: list[int]


class SparseModel:
    def __init__(self) -> None:
        self.lower: list[float] = []
        self.upper: list[float] = []
        self.rows: list[tuple[dict[int, float], float, float]] = []

    def variable(self, lower: float = 0.0, upper: float = highspy.kHighsInf) -> int:
        index = len(self.lower)
        self.lower.append(lower)
        self.upper.append(upper)
        return index

    def row(
        self,
        coefficients: dict[int, float],
        lower: float = -highspy.kHighsInf,
        upper: float = highspy.kHighsInf,
    ) -> int:
        index = len(self.rows)
        self.rows.append((coefficients, lower, upper))
        return index

    def build(self, costs: np.ndarray, settings: dict[str, Any]) -> highspy.Highs:
        highs = highspy.Highs()
        highs.setOptionValue("output_flag", False)
        options = _solver_options(settings, "HIGHS")
        highs.setOptionValue("time_limit", float(options["time_limit"]))
        highs.setOptionValue("mip_rel_gap", float(options["mip_rel_gap"]))
        lower = np.asarray(self.lower, dtype=np.float64)
        upper = np.asarray(self.upper, dtype=np.float64)
        _require_ok(highs.addVars(len(lower), lower, upper), "add variables")
        indices = np.arange(len(lower), dtype=np.int32)
        _require_ok(highs.changeColsCost(len(lower), indices, costs), "set objective")

        starts = np.zeros(len(self.rows) + 1, dtype=np.int32)
        row_indices: list[int] = []
        values: list[float] = []
        row_lower = np.empty(len(self.rows), dtype=np.float64)
        row_upper = np.empty(len(self.rows), dtype=np.float64)
        for row_index, (coefficients, lower_bound, upper_bound) in enumerate(self.rows):
            for column, value in sorted(coefficients.items()):
                if abs(value) > 1e-14:
                    row_indices.append(column)
                    values.append(value)
            starts[row_index + 1] = len(row_indices)
            row_lower[row_index] = lower_bound
            row_upper[row_index] = upper_bound
        _require_ok(
            highs.addRows(
                len(self.rows),
                row_lower,
                row_upper,
                len(row_indices),
                starts,
                np.asarray(row_indices, dtype=np.int32),
                np.asarray(values, dtype=np.float64),
            ),
            "add constraints",
        )
        return highs


def solve_direct_highs(
    prepared: "PreparedMultistage",
    started: float,
    prepare_ms: float,
    decomposition: str,
) -> dict[str, Any]:
    if prepared.discrete or prepared.unsafe_cycle or prepared.unsafe_meter_split:
        raise DirectHighsError("direct HiGHS path requires a cycle-safe continuous tariff")
    build_started = time.perf_counter()
    model = SparseModel()
    m = len(prepared.scenario_set.scenarios)
    n = prepared.n
    probabilities = np.asarray(
        [scenario.probability for scenario in prepared.scenario_set.scenarios]
    )
    service_terms: list[dict[int, float]] = []
    economic_terms: list[dict[int, float]] = []
    scenario_vars: list[DirectScenarioVars] = []

    block_start_at = np.zeros(n, dtype=np.int64)
    for block_start, block_end in prepared.blocks:
        block_start_at[block_start:block_end] = block_start

    storage_actions: dict[tuple[int, int, int], tuple[int, int]] = {}
    curtail_upper: dict[tuple[int, int], float] = {}
    for si, scenario in enumerate(prepared.scenario_set.scenarios):
        pv_generation = np.maximum(0.0, -scenario.pv)
        for t in range(n):
            key = (int(prepared.tree.node_at[si, t]), t)
            curtail_upper[key] = min(curtail_upper.get(key, math.inf), float(pv_generation[t]))
    curtail_actions = {
        key: model.variable(0.0, upper) for key, upper in sorted(curtail_upper.items())
    }

    spread = max(
        0.0,
        finite_number(
            prepared.settings.get("min_arbitrage_spread_ore_kwh", 0),
            "settings.min_arbitrage_spread_ore_kwh",
        ),
    )
    for si, scenario in enumerate(prepared.scenario_set.scenarios):
        pv_generation = np.maximum(0.0, -scenario.pv)
        pv_surplus = np.maximum(0.0, pv_generation - scenario.load)
        base_import = np.maximum(0.0, scenario.load - pv_generation)
        charges: list[list[int]] = []
        discharges: list[list[int]] = []
        energies: list[list[int]] = []
        total_charge: list[list[int]] = [[] for _ in range(n)]
        total_discharge: list[list[int]] = [[] for _ in range(n)]
        service: dict[int, float] = {}
        economic: dict[int, float] = {}

        for storage_index, spec in enumerate(prepared.storages):
            capacity = float(spec["capacity_wh"])
            minimum = float(spec.get("min_energy_wh", 0))
            maximum = float(spec.get("max_energy_wh", capacity))
            initial = float(spec["initial_energy_wh"])
            max_charge = max(0.0, float(spec.get("max_charge_w", 0)))
            max_discharge = max(0.0, float(spec.get("max_discharge_w", 0)))
            eta_c = float(spec.get("charge_efficiency", 0.95))
            eta_d = float(spec.get("discharge_efficiency", 0.95))
            charge: list[int] = []
            discharge: list[int] = []
            for t in range(n):
                block_start = int(block_start_at[t])
                node = int(prepared.tree.node_at[si, block_start])
                action_key = (storage_index, node, block_start)
                action = storage_actions.get(action_key)
                if action is None:
                    action = (
                        model.variable(0.0, max_charge),
                        model.variable(0.0, max_discharge),
                    )
                    storage_actions[action_key] = action
                charge.append(action[0])
                discharge.append(action[1])
                total_charge[t].append(action[0])
                total_discharge[t].append(action[1])

            energy = [model.variable(0.0, capacity) for _ in range(n + 1)]
            lower_recovery = [model.variable() for _ in range(n + 1)]
            upper_recovery = [model.variable() for _ in range(n + 1)]
            model.row({energy[0]: 1.0}, initial, initial)
            model.row(
                {lower_recovery[0]: 1.0},
                max(0.0, minimum - initial),
                max(0.0, minimum - initial),
            )
            model.row(
                {upper_recovery[0]: 1.0},
                max(0.0, initial - maximum),
                max(0.0, initial - maximum),
            )
            for t in range(n):
                model.row(
                    {
                        energy[t + 1]: 1.0,
                        energy[t]: -1.0,
                        charge[t]: -float(prepared.dt_h[t]) * eta_c,
                        discharge[t]: float(prepared.dt_h[t]) / eta_d,
                    },
                    0.0,
                    0.0,
                )
            for t in range(n + 1):
                model.row({lower_recovery[t]: 1.0, energy[t]: 1.0}, minimum)
                model.row({upper_recovery[t]: 1.0, energy[t]: -1.0}, -maximum)
                if t > 0:
                    model.row(
                        {lower_recovery[t]: 1.0, lower_recovery[t - 1]: -1.0},
                        upper=0.0,
                    )
                    model.row(
                        {upper_recovery[t]: 1.0, upper_recovery[t - 1]: -1.0},
                        upper=0.0,
                    )
                    _add(service, lower_recovery[t], 1.0 / (capacity * n))
                    _add(service, upper_recovery[t], 1.0 / (capacity * n))

            if spec.get("target_energy_wh") is not None:
                deadline = int(spec.get("target_slot", n - 1))
                shortfall = model.variable()
                target = float(spec["target_energy_wh"])
                model.row({energy[deadline + 1]: 1.0, shortfall: 1.0}, target)
                _add(service, shortfall, 1.0 / capacity)

            cycle_coefficient = spread + max(0.0, float(spec.get("cycle_cost_ore_kwh", 0)))
            for t in range(n):
                _add(
                    economic,
                    discharge[t],
                    cycle_coefficient * float(prepared.dt_h[t]) / 1000.0,
                )
            _add(
                economic,
                energy[-1],
                -float(spec.get("terminal_price_ore_kwh", 0)) / 1000.0,
            )
            charges.append(charge)
            discharges.append(discharge)
            energies.append(energy)

        curtail = [
            curtail_actions[(int(prepared.tree.node_at[si, t]), t)] for t in range(n)
        ]
        grid_import: list[int] = []
        grid_export: list[int] = []
        for t in range(n):
            import_upper = float(prepared.import_bound[t])
            export_upper = float(prepared.export_bound[t])
            if prepared.mode == "self_consumption":
                import_upper = min(import_upper, float(base_import[t]) + 50.0)
            import_index = model.variable(0.0, import_upper)
            export_index = model.variable(0.0, export_upper)
            grid_import.append(import_index)
            grid_export.append(export_index)
            balance = {
                import_index: 1.0,
                export_index: -1.0,
                curtail[t]: -1.0,
            }
            for index in total_charge[t]:
                _add(balance, index, -1.0)
            for index in total_discharge[t]:
                _add(balance, index, 1.0)
            net = float(scenario.load[t] - pv_generation[t])
            model.row(balance, net, net)
            if prepared.mode == "self_consumption":
                model.row(
                    {export_index: 1.0, curtail[t]: 1.0},
                    upper=float(pv_surplus[t]) + 50.0,
                )
            elif prepared.mode in {"cheap_charge", "passive_arbitrage"}:
                model.row(
                    {export_index: 1.0, curtail[t]: 1.0},
                    upper=float(pv_surplus[t]) + 1e-6,
                )
            _add(
                economic,
                import_index,
                float(prepared.effective_import[t] * prepared.dt_h[t] / 1000.0),
            )
            _add(
                economic,
                export_index,
                -float(prepared.effective_export[t] * prepared.dt_h[t] / 1000.0),
            )

            if prepared.mode in {"self_consumption", "passive_arbitrage"}:
                house_import = model.variable()
                row = {house_import: 1.0, curtail[t]: -1.0}
                for index in total_charge[t]:
                    _add(row, index, -1.0)
                for index in total_discharge[t]:
                    _add(row, index, 1.0)
                model.row(row, net)
                _add(
                    economic,
                    house_import,
                    float(
                        2.0
                        * max(prepared.effective_import[t], 0.0)
                        * prepared.dt_h[t]
                        / 1000.0
                    ),
                )

        service_terms.append(service)
        economic_terms.append(economic)
        scenario_vars.append(
            DirectScenarioVars(
                charges, discharges, energies, curtail, grid_import, grid_export
            )
        )

    service_costs = np.zeros(len(model.lower), dtype=np.float64)
    for probability, service in zip(probabilities, service_terms):
        _accumulate(service_costs, service, float(probability))
    if prepared.service_cvar_weight > 0:
        threshold = model.variable(-highspy.kHighsInf, highspy.kHighsInf)
        excess = [model.variable() for _ in range(m)]
        service_costs = np.pad(service_costs, (0, 1 + m))
        service_costs[threshold] += prepared.service_cvar_weight
        for si, service in enumerate(service_terms):
            row = {excess[si]: 1.0, threshold: 1.0}
            for index, value in service.items():
                _add(row, index, -value)
            model.row(row, 0.0)
            service_costs[excess[si]] += (
                prepared.service_cvar_weight
                * float(probabilities[si])
                / (1.0 - prepared.service_cvar_alpha)
            )

    service_metric = {
        index: float(value)
        for index, value in enumerate(service_costs)
        if abs(value) > 1e-14
    }
    service_cap_row = model.row(service_metric)

    economic_costs = np.zeros(len(model.lower), dtype=np.float64)
    for probability, economic in zip(probabilities, economic_terms):
        _accumulate(economic_costs, economic, float(probability))
    if prepared.economic_cvar_weight > 0 and m > 1:
        threshold = model.variable(-highspy.kHighsInf, highspy.kHighsInf)
        excess = [model.variable() for _ in range(m)]
        service_costs = np.pad(service_costs, (0, 1 + m))
        economic_costs = np.pad(economic_costs, (0, 1 + m))
        economic_costs[threshold] += prepared.economic_cvar_weight
        for si, economic in enumerate(economic_terms):
            row = {excess[si]: 1.0, threshold: 1.0}
            for index, value in economic.items():
                _add(row, index, -value)
            model.row(row, 0.0)
            economic_costs[excess[si]] += (
                prepared.economic_cvar_weight
                * float(probabilities[si])
                / (1.0 - prepared.economic_cvar_alpha)
            )
    if len(service_costs) < len(model.lower):
        service_costs = np.pad(service_costs, (0, len(model.lower) - len(service_costs)))
    if len(economic_costs) < len(model.lower):
        economic_costs = np.pad(economic_costs, (0, len(model.lower) - len(economic_costs)))

    highs = model.build(service_costs, prepared.settings)
    build_ms = (time.perf_counter() - build_started) * 1000.0
    solver_started = time.perf_counter()
    _run_optimal(highs, "service")
    best_service = max(0.0, float(highs.getObjectiveValue()))
    _require_ok(
        highs.changeRowsBounds(
            1,
            np.asarray([service_cap_row], dtype=np.int32),
            np.asarray([-highspy.kHighsInf]),
            np.asarray([best_service + 1e-7]),
        ),
        "set service cap",
    )
    column_indices = np.arange(len(model.lower), dtype=np.int32)
    _require_ok(
        highs.changeColsCost(len(model.lower), column_indices, economic_costs),
        "set economic objective",
    )
    _run_optimal(highs, "economic")
    solver_ms = (time.perf_counter() - solver_started) * 1000.0
    solution = np.asarray(highs.getSolution().col_value, dtype=np.float64)
    if len(solution) != len(model.lower) or not np.all(np.isfinite(solution)):
        raise DirectHighsError("HiGHS returned a non-finite solution")

    return _response(
        prepared,
        scenario_vars,
        solution,
        float(highs.getObjectiveValue()),
        best_service,
        started,
        prepare_ms,
        build_ms,
        solver_ms,
        decomposition,
        len(model.lower),
        len(model.rows),
    )


def _response(
    prepared: "PreparedMultistage",
    scenario_vars: list[DirectScenarioVars],
    solution: np.ndarray,
    objective: float,
    best_service: float,
    started: float,
    prepare_ms: float,
    build_ms: float,
    solver_ms: float,
    decomposition: str,
    variables: int,
    constraints: int,
) -> dict[str, Any]:
    from .multistage import policy_config

    scenarios = prepared.scenario_set.scenarios
    base_index = next((i for i, scenario in enumerate(scenarios) if scenario.id == "base"), 0)
    base = scenarios[base_index]
    base_vars = scenario_vars[base_index]
    total_capacity = sum(float(spec["capacity_wh"]) for spec in prepared.storages)
    initial_total = sum(float(spec["initial_energy_wh"]) for spec in prepared.storages)
    actions: list[dict[str, Any]] = []
    raw_total_cost = 0.0
    for t, slot in enumerate(prepared.slots):
        storage_power: dict[str, float] = {}
        storage_energy: dict[str, float] = {}
        battery_w = 0.0
        stored_wh = 0.0
        for i, spec in enumerate(prepared.storages):
            power = float(
                solution[base_vars.charge[i][t]] - solution[base_vars.discharge[i][t]]
            )
            energy = float(solution[base_vars.energy[i][t + 1]])
            storage_power[str(spec["id"])] = power
            storage_energy[str(spec["id"])] = energy
            battery_w += power
            stored_wh += energy
        grid_w = float(
            solution[base_vars.grid_import[t]] - solution[base_vars.grid_export[t]]
        )
        grid_kwh = grid_w * prepared.dt_h[t] / 1000.0
        raw_cost = prepared.price[t] * max(grid_kwh, 0.0) - prepared.export_price[t] * max(
            -grid_kwh, 0.0
        )
        raw_total_cost += raw_cost
        curtailed_w = max(0.0, float(solution[base_vars.curtail[t]]))
        actions.append(
            {
                "slot_start_ms": int(slot.get("start_ms", 0)),
                "slot_len_min": int(slot["len_min"]),
                "battery_w": battery_w,
                "grid_w": grid_w,
                "soc_pct": stored_wh / total_capacity * 100.0,
                "cost_ore": raw_cost,
                "pv_limit_w": max(0.0, -base.pv[t] - curtailed_w)
                if curtailed_w > 1e-5
                else 0.0,
                "storage_power_w": storage_power,
                "storage_energy_wh": storage_energy,
                "flex_power_w": {},
                "flex_energy_wh": {},
                "thermal_power_w": {},
                "thermal_state": {},
            }
        )
    solve_ms = (time.perf_counter() - started) * 1000.0
    return {
        "schema_version": SCHEMA_VERSION,
        "request_id": str(prepared.payload["request_id"]),
        "ok": True,
        "solver": {
            "engine": "highspy",
            "backend": "highs",
            "status": "optimal",
            "formulation": "multistage-lp",
            "objective_ore": objective,
            "service_slack": best_service,
            "solve_ms": solve_ms,
            "prepare_ms": prepare_ms,
            "build_ms": build_ms,
            "solver_ms": solver_ms,
            "cache_hit": False,
            "dpp": False,
            "mip_gap": None,
            "scenario_count": len(scenarios),
            "scenario_original_count": prepared.scenario_set.original_count,
            "scenario_reduction_error": prepared.scenario_set.reduction_error,
            "scenario_policy": "multistage",
            "policy_version": "storage-multistage-v1",
            "policy_config": policy_config(prepared),
            "non_anticipative_slots": prepared.first_stage_slots,
            "tree_nodes": prepared.tree.node_count,
            "move_blocks": len(prepared.blocks),
            "decomposition": f"direct-highs-{decomposition}",
            "risk_model": "service-cvar-then-expected-cost",
            "service_cvar_weight": prepared.service_cvar_weight,
            "service_cvar_alpha": prepared.service_cvar_alpha,
            "economic_cvar_weight": prepared.economic_cvar_weight,
            "economic_cvar_alpha": prepared.economic_cvar_alpha,
            "model_variables": variables,
            "model_constraints": constraints,
        },
        "plan": {
            "mode": prepared.mode,
            "horizon_slots": prepared.n,
            "capacity_wh": total_capacity,
            "initial_soc_pct": initial_total / total_capacity * 100.0,
            "total_cost_ore": raw_total_cost,
            "actions": actions,
        },
    }


def _add(coefficients: dict[int, float], index: int, value: float) -> None:
    coefficients[index] = coefficients.get(index, 0.0) + value


def _accumulate(target: np.ndarray, terms: dict[int, float], weight: float) -> None:
    for index, value in terms.items():
        target[index] += weight * value


def _require_ok(status: highspy.HighsStatus, operation: str) -> None:
    if status != highspy.HighsStatus.kOk:
        raise DirectHighsError(f"HiGHS failed to {operation}: {status}")


def _run_optimal(highs: highspy.Highs, phase: str) -> None:
    _require_ok(highs.run(), f"run {phase} solve")
    status = highs.getModelStatus()
    if status != highspy.HighsModelStatus.kOptimal:
        raise DirectHighsError(f"HiGHS {phase} solve failed with status {status}")
