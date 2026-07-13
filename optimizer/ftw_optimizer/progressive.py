from __future__ import annotations

import math
import time
from dataclasses import dataclass
from typing import Any, TYPE_CHECKING

import cvxpy as cp
import numpy as np

from . import SCHEMA_VERSION
from .model import OPTIMAL_STATUSES, _solver_options
from .protocol import ProtocolError, finite_number

if TYPE_CHECKING:
    from .multistage import PreparedMultistage


class ProgressiveHedgingNotConverged(RuntimeError):
    pass


@dataclass
class PHStorageVars:
    charge: cp.Variable
    discharge: cp.Variable
    energy: cp.Variable


@dataclass
class PHSubproblem:
    storages: list[PHStorageVars]
    curtail: cp.Variable
    grid_import: cp.Variable
    grid_export: cp.Variable
    decisions_kw: cp.Expression
    consensus_kw: cp.Parameter
    dual_kw: cp.Parameter
    consensus_mask: np.ndarray
    economic: cp.Expression
    initial_problem: cp.Problem
    problem: cp.Problem


def ph_eligible(prepared: "PreparedMultistage") -> tuple[bool, str]:
    settings = prepared.settings
    if str(settings.get("formulation", "auto")) != "relaxed":
        return False, "formulation is not relaxed"
    if prepared.mode != "arbitrage":
        return False, "mode is not unconstrained arbitrage"
    if prepared.economic_cvar_weight > 0:
        return False, "economic CVaR couples scenario subproblems"
    if finite_number(settings.get("pv_charge_bonus_ore_kwh", 0), "settings.pv_charge_bonus_ore_kwh") != 0:
        return False, "PV charge bonus can incentivize simultaneous cycling"
    if np.any(prepared.effective_import < -1e-9):
        return False, "negative import prices require a discrete cycling guard"
    if np.any(prepared.effective_import < prepared.effective_export - 1e-9):
        return False, "import/export prices require a discrete meter guard"
    for i, spec in enumerate(prepared.storages):
        if spec.get("target_energy_wh") is not None:
            return False, f"storage {i} has a service target"
        initial = float(spec["initial_energy_wh"])
        minimum = float(spec.get("min_energy_wh", 0))
        maximum = float(spec.get("max_energy_wh", spec["capacity_wh"]))
        if initial < minimum - 1e-6 or initial > maximum + 1e-6:
            return False, f"storage {i} needs operating-band recovery"
    return True, "eligible"


def solve_progressive_hedging(
    prepared: "PreparedMultistage", started: float, prepare_ms: float
) -> dict[str, Any]:
    eligible, reason = ph_eligible(prepared)
    if not eligible:
        raise ProtocolError(f"progressive hedging is not eligible: {reason}")
    settings = prepared.settings
    max_iterations = _positive_int(settings.get("ph_max_iterations", 8), "settings.ph_max_iterations")
    rho_value = max(1e-6, finite_number(settings.get("ph_rho", 50), "settings.ph_rho"))
    tolerance_w = max(0.1, finite_number(settings.get("ph_tolerance_w", 5), "settings.ph_tolerance_w"))
    build_started = time.perf_counter()
    subproblems = [
        _build_subproblem(prepared, si, rho_value)
        for si in range(len(prepared.scenario_set.scenarios))
    ]
    build_ms = (time.perf_counter() - build_started) * 1000.0

    probabilities = np.asarray([scenario.probability for scenario in prepared.scenario_set.scenarios])
    decisions = [np.zeros((2 * len(prepared.storages) + 1, prepared.n)) for _ in subproblems]
    consensus = [np.zeros_like(decision) for decision in decisions]
    dual = [np.zeros_like(decision) for decision in decisions]

    solver_started = time.perf_counter()
    for subproblem in subproblems:
        subproblem.consensus_kw.value = np.zeros_like(decisions[0])
        subproblem.dual_kw.value = np.zeros_like(decisions[0])
        _solve_problem(
            subproblem.initial_problem, settings, len(subproblems), max_iterations
        )
    decisions = [_decision_value(subproblem) for subproblem in subproblems]
    consensus = _consensus_values(prepared, decisions, probabilities)
    residual_w = _nonanticipativity_residual_w(prepared, decisions, consensus)

    iterations = 0
    for iteration in range(1, max_iterations + 1):
        iterations = iteration
        for si, subproblem in enumerate(subproblems):
            subproblem.consensus_kw.value = consensus[si]
            subproblem.dual_kw.value = dual[si]
            _solve_problem(subproblem.problem, settings, len(subproblems), max_iterations)
            decisions[si] = _decision_value(subproblem)
        consensus = _consensus_values(prepared, decisions, probabilities)
        residual_w = _nonanticipativity_residual_w(prepared, decisions, consensus)
        for si, subproblem in enumerate(subproblems):
            dual[si] += subproblem.consensus_mask * (decisions[si] - consensus[si])
        if residual_w <= tolerance_w:
            break
    solver_ms = (time.perf_counter() - solver_started) * 1000.0
    if residual_w > tolerance_w:
        raise ProgressiveHedgingNotConverged(
            f"progressive hedging residual {residual_w:.3f} W exceeds {tolerance_w:.3f} W"
        )
    return _response(
        prepared,
        subproblems,
        started,
        prepare_ms,
        build_ms,
        solver_ms,
        iterations,
        residual_w,
        rho_value,
    )


def _build_subproblem(
    prepared: "PreparedMultistage", scenario_index: int, rho_value: float
) -> PHSubproblem:
    n = prepared.n
    scenario = prepared.scenario_set.scenarios[scenario_index]
    constraints: list[cp.Constraint] = []
    storages: list[PHStorageVars] = []
    total_charge: cp.Expression = cp.Constant(np.zeros(n))
    total_discharge: cp.Expression = cp.Constant(np.zeros(n))
    cycle_cost: cp.Expression = cp.Constant(0.0)
    terminal_credit: cp.Expression = cp.Constant(0.0)
    decision_rows: list[cp.Expression] = []
    spread = max(
        0.0,
        finite_number(
            prepared.settings.get("min_arbitrage_spread_ore_kwh", 0),
            "settings.min_arbitrage_spread_ore_kwh",
        ),
    )
    for i, spec in enumerate(prepared.storages):
        charge = cp.Variable(n, nonneg=True, name=f"ph_s{scenario_index}_b{i}_charge")
        discharge = cp.Variable(n, nonneg=True, name=f"ph_s{scenario_index}_b{i}_discharge")
        energy = cp.Variable(n + 1, name=f"ph_s{scenario_index}_b{i}_energy")
        minimum = float(spec.get("min_energy_wh", 0))
        maximum = float(spec.get("max_energy_wh", spec["capacity_wh"]))
        eta_c = float(spec.get("charge_efficiency", 0.95))
        eta_d = float(spec.get("discharge_efficiency", 0.95))
        constraints += [
            energy[0] == float(spec["initial_energy_wh"]),
            energy[1:]
            == energy[:-1]
            + cp.multiply(prepared.dt_h, eta_c * charge - discharge / eta_d),
            energy >= minimum,
            energy <= maximum,
            charge <= float(spec.get("max_charge_w", 0)),
            discharge <= float(spec.get("max_discharge_w", 0)),
        ]
        cycle_ore = spread + max(0.0, float(spec.get("cycle_cost_ore_kwh", 0)))
        cycle_cost += cycle_ore * cp.sum(cp.multiply(prepared.dt_h, discharge)) / 1000.0
        terminal_credit += float(spec.get("terminal_price_ore_kwh", 0)) * energy[-1] / 1000.0
        total_charge += charge
        total_discharge += discharge
        decision_rows.extend((charge / 1000.0, discharge / 1000.0))
        storages.append(PHStorageVars(charge, discharge, energy))

    pv_generation = np.maximum(0.0, -scenario.pv)
    curtail = cp.Variable(n, nonneg=True, name=f"ph_s{scenario_index}_curtail")
    grid_import = cp.Variable(n, nonneg=True, name=f"ph_s{scenario_index}_import")
    grid_export = cp.Variable(n, nonneg=True, name=f"ph_s{scenario_index}_export")
    constraints += [
        curtail <= pv_generation,
        grid_import - grid_export
        == scenario.load - pv_generation + curtail + total_charge - total_discharge,
        grid_import <= prepared.import_bound,
        grid_export <= prepared.export_bound,
    ]
    decision_rows.append(curtail / 1000.0)
    decisions_kw = cp.vstack(decision_rows)
    for start, end in prepared.blocks:
        for t in range(start + 1, end):
            for storage in storages:
                constraints += [
                    storage.charge[t] == storage.charge[start],
                    storage.discharge[t] == storage.discharge[start],
                ]

    shape = (2 * len(storages) + 1, n)
    consensus_kw = cp.Parameter(shape, name=f"ph_s{scenario_index}_consensus")
    dual_kw = cp.Parameter(shape, name=f"ph_s{scenario_index}_dual")
    mask = _consensus_mask(prepared, scenario_index, shape)
    import_coeff = prepared.effective_import * prepared.dt_h / 1000.0
    export_coeff = prepared.effective_export * prepared.dt_h / 1000.0
    economic = cp.sum(cp.multiply(import_coeff, grid_import) - cp.multiply(export_coeff, grid_export))
    economic += cycle_cost - terminal_credit
    penalty = 0.5 * rho_value * cp.sum_squares(
        cp.multiply(mask, decisions_kw - consensus_kw + dual_kw)
    )
    initial_problem = cp.Problem(cp.Minimize(economic), constraints)
    problem = cp.Problem(cp.Minimize(economic + penalty), constraints)
    if not initial_problem.is_dpp() or not problem.is_dpp():
        raise RuntimeError("progressive hedging subproblem is not DPP-compliant")
    return PHSubproblem(
        storages,
        curtail,
        grid_import,
        grid_export,
        decisions_kw,
        consensus_kw,
        dual_kw,
        mask,
        economic,
        initial_problem,
        problem,
    )


def _consensus_mask(
    prepared: "PreparedMultistage", scenario_index: int, shape: tuple[int, int]
) -> np.ndarray:
    mask = np.zeros(shape)
    for t in range(prepared.n):
        node = prepared.tree.node_at[scenario_index, t]
        if int(np.sum(prepared.tree.node_at[:, t] == node)) > 1:
            mask[:, t] = 1.0
    return mask


def _solve_problem(
    problem: cp.Problem,
    settings: dict[str, Any],
    scenario_count: int,
    max_iterations: int,
) -> None:
    options_settings = dict(settings)
    total_limit = max(0.1, finite_number(settings.get("time_limit_s", 2), "settings.time_limit_s"))
    options_settings["time_limit_s"] = max(
        0.05, total_limit / max(1, scenario_count * (max_iterations + 1))
    )
    problem.solve(
        solver=cp.HIGHS,
        warm_start=True,
        enforce_dpp=True,
        **_solver_options(options_settings, cp.HIGHS),
    )
    if problem.status not in OPTIMAL_STATUSES or problem.value is None:
        raise ProgressiveHedgingNotConverged(
            f"PH subproblem failed with status {problem.status}"
        )


def _decision_value(subproblem: PHSubproblem) -> np.ndarray:
    value = subproblem.decisions_kw.value
    if value is None or not np.all(np.isfinite(value)):
        raise ProgressiveHedgingNotConverged("PH subproblem returned non-finite decisions")
    return np.asarray(value, dtype=float)


def _consensus_values(
    prepared: "PreparedMultistage",
    decisions: list[np.ndarray],
    probabilities: np.ndarray,
) -> list[np.ndarray]:
    consensus = [decision.copy() for decision in decisions]
    for t in range(prepared.n):
        nodes: dict[int, list[int]] = {}
        for si in range(len(decisions)):
            nodes.setdefault(int(prepared.tree.node_at[si, t]), []).append(si)
        for members in nodes.values():
            weights = probabilities[members]
            weights = weights / np.sum(weights)
            value = sum(weights[i] * decisions[si][:, t] for i, si in enumerate(members))
            for si in members:
                consensus[si][:, t] = value
    return consensus


def _nonanticipativity_residual_w(
    prepared: "PreparedMultistage",
    decisions: list[np.ndarray],
    consensus: list[np.ndarray],
) -> float:
    residual_kw = 0.0
    for si in range(len(decisions)):
        masked = prepared.tree.node_at[si]
        for t in range(prepared.n):
            node = masked[t]
            if int(np.sum(prepared.tree.node_at[:, t] == node)) > 1:
                residual_kw = max(
                    residual_kw,
                    float(np.max(np.abs(decisions[si][:, t] - consensus[si][:, t]))),
                )
    return residual_kw * 1000.0


def _response(
    prepared: "PreparedMultistage",
    subproblems: list[PHSubproblem],
    started: float,
    prepare_ms: float,
    build_ms: float,
    solver_ms: float,
    iterations: int,
    residual_w: float,
    rho_value: float,
) -> dict[str, Any]:
    from .multistage import policy_config

    scenarios = prepared.scenario_set.scenarios
    base_index = next((i for i, scenario in enumerate(scenarios) if scenario.id == "base"), 0)
    base = scenarios[base_index]
    base_problem = subproblems[base_index]
    total_capacity = sum(float(spec["capacity_wh"]) for spec in prepared.storages)
    initial_total = sum(float(spec["initial_energy_wh"]) for spec in prepared.storages)
    actions: list[dict[str, Any]] = []
    raw_total_cost = 0.0
    for t, slot in enumerate(prepared.slots):
        storage_power: dict[str, float] = {}
        storage_energy: dict[str, float] = {}
        battery_w = 0.0
        stored_wh = 0.0
        for i, storage in enumerate(base_problem.storages):
            power = float(storage.charge.value[t] - storage.discharge.value[t])
            energy = float(storage.energy.value[t + 1])
            storage_id = str(prepared.storages[i]["id"])
            storage_power[storage_id] = power
            storage_energy[storage_id] = energy
            battery_w += power
            stored_wh += energy
        grid_w = float(base_problem.grid_import.value[t] - base_problem.grid_export.value[t])
        grid_kwh = grid_w * prepared.dt_h[t] / 1000.0
        raw_cost = prepared.price[t] * max(grid_kwh, 0.0) - prepared.export_price[t] * max(-grid_kwh, 0.0)
        raw_total_cost += raw_cost
        curtailed_w = max(0.0, float(base_problem.curtail.value[t]))
        actions.append(
            {
                "slot_start_ms": int(slot.get("start_ms", 0)),
                "slot_len_min": int(slot["len_min"]),
                "battery_w": battery_w,
                "grid_w": grid_w,
                "soc_pct": stored_wh / total_capacity * 100.0,
                "cost_ore": raw_cost,
                "pv_limit_w": max(0.0, -base.pv[t] - curtailed_w) if curtailed_w > 1e-5 else 0.0,
                "storage_power_w": storage_power,
                "storage_energy_wh": storage_energy,
                "flex_power_w": {},
                "flex_energy_wh": {},
                "thermal_power_w": {},
                "thermal_state": {},
            }
        )
    probabilities = np.asarray([scenario.probability for scenario in scenarios])
    expected_objective = float(
        probabilities
        @ np.asarray([float(subproblem.economic.value) for subproblem in subproblems])
    )
    solve_ms = (time.perf_counter() - started) * 1000.0
    return {
        "schema_version": SCHEMA_VERSION,
        "request_id": str(prepared.payload["request_id"]),
        "ok": True,
        "solver": {
            "engine": "cvxpy",
            "backend": "highs",
            "status": "optimal-ph",
            "formulation": "multistage-ph-qp",
            "objective_ore": expected_objective,
            "solve_ms": solve_ms,
            "prepare_ms": prepare_ms,
            "build_ms": build_ms,
            "solver_ms": solver_ms,
            "cache_hit": False,
            "dpp": True,
            "scenario_count": len(scenarios),
            "scenario_original_count": prepared.scenario_set.original_count,
            "scenario_reduction_error": prepared.scenario_set.reduction_error,
            "scenario_policy": "multistage",
            "policy_version": "storage-multistage-v1",
            "policy_config": policy_config(prepared),
            "non_anticipative_slots": prepared.first_stage_slots,
            "tree_nodes": prepared.tree.node_count,
            "move_blocks": len(prepared.blocks),
            "decomposition": "progressive-hedging",
            "risk_model": "hard-service-expected-cost",
            "service_cvar_weight": prepared.service_cvar_weight,
            "service_cvar_alpha": prepared.service_cvar_alpha,
            "economic_cvar_weight": 0.0,
            "economic_cvar_alpha": prepared.economic_cvar_alpha,
            "ph_iterations": iterations,
            "ph_residual_w": residual_w,
            "ph_rho": rho_value,
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


def _positive_int(value: Any, field: str) -> int:
    number = finite_number(value, field)
    integer = int(number)
    if number != integer or integer < 1:
        raise ProtocolError(f"{field} must be a positive integer")
    return integer
