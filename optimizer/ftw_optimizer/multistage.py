from __future__ import annotations

import json
import math
import time
from collections import OrderedDict
from dataclasses import dataclass
from typing import Any

import cvxpy as cp
import numpy as np

from . import SCHEMA_VERSION
from .model import OPTIMAL_STATUSES, _export_price, _mode, _solver_options
from .protocol import ProtocolError, finite_number, positive_number, require_dict, require_list
from .scenario_tree import (
    ScenarioSet,
    ScenarioTree,
    build_scenario_tree,
    decision_blocks,
    parse_scenarios,
    reduce_scenarios,
)


POLICY_VERSION = "storage-multistage-v1"
_CACHE_LIMIT = 1
_MODEL_CACHE: OrderedDict[tuple[Any, ...], "CompiledMultistage"] = OrderedDict()


@dataclass(frozen=True)
class PreparedMultistage:
    payload: dict[str, Any]
    settings: dict[str, Any]
    slots: tuple[dict[str, Any], ...]
    n: int
    mode: str
    formulation: str
    unsafe_cycle: bool
    unsafe_meter_split: bool
    storage_discrete: bool
    meter_discrete: bool
    discrete: bool
    dt_h: np.ndarray
    price: np.ndarray
    export_price: np.ndarray
    effective_import: np.ndarray
    effective_export: np.ndarray
    base_load: np.ndarray
    base_pv: np.ndarray
    scenario_set: ScenarioSet
    tree: ScenarioTree
    blocks: tuple[tuple[int, int], ...]
    storages: tuple[dict[str, Any], ...]
    first_stage_slots: int
    service_cvar_weight: float
    service_cvar_alpha: float
    economic_cvar_weight: float
    economic_cvar_alpha: float
    max_site_power: float
    import_bound: np.ndarray
    export_bound: np.ndarray


@dataclass
class ScenarioStorageVars:
    charge: cp.Expression
    discharge: cp.Expression
    energy: cp.Variable


@dataclass
class ScenarioVars:
    storages: list[ScenarioStorageVars]
    curtail: cp.Variable
    grid_import: cp.Variable
    grid_export: cp.Variable
    service: cp.Expression
    economic: cp.Expression


@dataclass
class CompiledMultistage:
    key: tuple[Any, ...]
    prepared_shape: PreparedMultistage
    load: cp.Parameter
    pv_generation: cp.Parameter
    pv_surplus: cp.Parameter
    base_import: cp.Parameter
    import_bound: cp.Parameter
    export_bound: cp.Parameter
    big_m: cp.Parameter
    import_coeff: cp.Parameter
    export_coeff: cp.Parameter
    strict_coeff: cp.Parameter
    initial_energy: list[cp.Parameter]
    lower_recovery: list[cp.Parameter]
    upper_recovery: list[cp.Parameter]
    target_energy: list[cp.Parameter | None]
    cycle_coeff: list[cp.Parameter]
    terminal_price: list[cp.Parameter]
    pv_bonus: cp.Parameter
    service_cap: cp.Parameter
    scenario_vars: list[ScenarioVars]
    service_metric: cp.Expression
    service_problem: cp.Problem
    economic_problem: cp.Problem
    build_ms: float

    def assign(self, prepared: PreparedMultistage) -> None:
        scenarios = prepared.scenario_set.scenarios
        self.load.value = np.stack([scenario.load for scenario in scenarios])
        self.pv_generation.value = np.stack([np.maximum(0.0, -scenario.pv) for scenario in scenarios])
        self.pv_surplus.value = np.stack(
            [np.maximum(0.0, -scenario.pv - scenario.load) for scenario in scenarios]
        )
        self.base_import.value = np.stack(
            [np.maximum(0.0, scenario.load + scenario.pv) for scenario in scenarios]
        )
        self.import_bound.value = prepared.import_bound
        self.export_bound.value = prepared.export_bound
        self.big_m.value = prepared.max_site_power
        self.import_coeff.value = prepared.effective_import * prepared.dt_h / 1000.0
        self.export_coeff.value = prepared.effective_export * prepared.dt_h / 1000.0
        self.strict_coeff.value = 2.0 * np.maximum(prepared.effective_import, 0.0) * prepared.dt_h / 1000.0
        self.pv_bonus.value = max(
            0.0,
            finite_number(
                prepared.settings.get("pv_charge_bonus_ore_kwh", 0),
                "settings.pv_charge_bonus_ore_kwh",
            ),
        )
        spread = max(
            0.0,
            finite_number(
                prepared.settings.get("min_arbitrage_spread_ore_kwh", 0),
                "settings.min_arbitrage_spread_ore_kwh",
            ),
        )
        for i, spec in enumerate(prepared.storages):
            initial = finite_number(spec.get("initial_energy_wh"), f"storages[{i}].initial_energy_wh")
            minimum = finite_number(spec.get("min_energy_wh", 0), f"storages[{i}].min_energy_wh")
            maximum = finite_number(spec.get("max_energy_wh", spec["capacity_wh"]), f"storages[{i}].max_energy_wh")
            self.initial_energy[i].value = initial
            self.lower_recovery[i].value = max(0.0, minimum - initial)
            self.upper_recovery[i].value = max(0.0, initial - maximum)
            if self.target_energy[i] is not None:
                self.target_energy[i].value = finite_number(
                    spec.get("target_energy_wh"), f"storages[{i}].target_energy_wh"
                )
            self.cycle_coeff[i].value = spread + max(
                0.0,
                finite_number(
                    spec.get("cycle_cost_ore_kwh", 0),
                    f"storages[{i}].cycle_cost_ore_kwh",
                ),
            )
            self.terminal_price[i].value = finite_number(
                spec.get("terminal_price_ore_kwh", 0),
                f"storages[{i}].terminal_price_ore_kwh",
            )


def solve_storage_multistage(payload: dict[str, Any]) -> dict[str, Any]:
    started = time.perf_counter()
    prepared_started = time.perf_counter()
    prepared = _prepare(payload)
    prepare_ms = (time.perf_counter() - prepared_started) * 1000.0

    decomposition_threshold = _positive_int(
        prepared.settings.get("decomposition_threshold", 20),
        "settings.decomposition_threshold",
    )
    decomposition_method = str(prepared.settings.get("decomposition_method", "auto"))
    if decomposition_method not in {"auto", "extensive", "progressive_hedging"}:
        raise ProtocolError(
            "settings.decomposition_method must be auto, extensive, or progressive_hedging"
        )
    decomposition = "extensive-dpp"
    use_ph = decomposition_method == "progressive_hedging" or (
        decomposition_method == "auto"
        and len(prepared.scenario_set.scenarios) > decomposition_threshold
    )
    if use_ph:
        from .progressive import (
            ProgressiveHedgingNotConverged,
            ph_eligible,
            solve_progressive_hedging,
        )

        eligible, reason = ph_eligible(prepared)
        if eligible:
            try:
                return solve_progressive_hedging(prepared, started, prepare_ms)
            except ProgressiveHedgingNotConverged:
                if decomposition_method == "progressive_hedging":
                    raise
                decomposition = "ph-fallback-scenario-reduction-extensive-dpp"
        elif decomposition_method == "progressive_hedging":
            raise ProtocolError(f"progressive hedging is not eligible: {reason}")

    if (
        len(prepared.scenario_set.scenarios) > decomposition_threshold
        and decomposition_method != "extensive"
    ):
        # The exact extensive model is deliberately bounded on edge. PH is
        # selected only by the continuous implementation in progressive.py;
        # discrete auto mode reduces to the configured extensive budget.
        reduced_pass = reduce_scenarios(
            list(prepared.scenario_set.scenarios), decomposition_threshold, prepared.dt_h
        )
        reduced = ScenarioSet(
            reduced_pass.scenarios,
            prepared.scenario_set.original_count,
            prepared.scenario_set.reduction_error + reduced_pass.reduction_error,
        )
        prepared = _replace_scenarios(prepared, reduced)
        if decomposition == "extensive-dpp":
            decomposition = "scenario-reduction-extensive-dpp"

    solver_name = str(prepared.settings.get("solver", "HIGHS")).upper()
    if solver_name not in {"HIGHS", "CLARABEL"}:
        raise ProtocolError("settings.solver must be HIGHS or CLARABEL")
    multistage_backend = str(prepared.settings.get("multistage_backend", "auto"))
    if multistage_backend not in {"auto", "highs", "cvxpy"}:
        raise ProtocolError("settings.multistage_backend must be auto, highs, or cvxpy")
    direct_eligible = (
        not prepared.discrete
        and not prepared.unsafe_cycle
        and not prepared.unsafe_meter_split
        and solver_name == "HIGHS"
    )
    direct_fallback_reason = ""
    if multistage_backend == "highs" and not direct_eligible:
        raise ProtocolError(
            "direct HiGHS multistage backend requires a continuous HIGHS formulation"
        )
    if multistage_backend in {"auto", "highs"} and direct_eligible:
        from .direct_highs import DirectHighsError, solve_direct_highs

        try:
            return solve_direct_highs(
                prepared, started, prepare_ms, decomposition.replace("-dpp", "")
            )
        except DirectHighsError as exc:
            if multistage_backend == "highs":
                raise
            direct_fallback_reason = str(exc)
            decomposition = f"direct-highs-fallback-{decomposition}"

    key = _cache_key(prepared)
    compiled = _MODEL_CACHE.get(key)
    cache_hit = compiled is not None
    if compiled is None:
        compiled = _compile(prepared, key)
        _MODEL_CACHE[key] = compiled
        while len(_MODEL_CACHE) > _CACHE_LIMIT:
            _MODEL_CACHE.popitem(last=False)
    else:
        _MODEL_CACHE.move_to_end(key)
    compiled.assign(prepared)

    if prepared.discrete and solver_name == "CLARABEL":
        solver_name = "HIGHS"
    solver_started = time.perf_counter()
    try:
        _run_problem(compiled.service_problem, prepared.settings, solver_name)
    except cp.error.SolverError:
        if prepared.discrete or solver_name == "CLARABEL":
            raise
        solver_name = "CLARABEL"
        _run_problem(compiled.service_problem, prepared.settings, solver_name)
    if compiled.service_problem.status not in OPTIMAL_STATUSES or compiled.service_problem.value is None:
        raise RuntimeError(
            f"multistage service-level solve failed with status {compiled.service_problem.status}"
        )
    best_service = max(0.0, float(compiled.service_problem.value))
    compiled.service_cap.value = best_service + 1e-7
    try:
        _run_problem(compiled.economic_problem, prepared.settings, solver_name)
    except cp.error.SolverError:
        if prepared.discrete or solver_name == "CLARABEL":
            raise
        solver_name = "CLARABEL"
        _run_problem(compiled.economic_problem, prepared.settings, solver_name)
    if compiled.economic_problem.status not in OPTIMAL_STATUSES or compiled.economic_problem.value is None:
        raise RuntimeError(
            f"multistage economic solve failed with status {compiled.economic_problem.status}"
        )
    solver_ms = (time.perf_counter() - solver_started) * 1000.0

    response = _response(
        prepared,
        compiled,
        best_service,
        solver_name,
        started,
        prepare_ms,
        solver_ms,
        cache_hit,
        decomposition,
        direct_fallback_reason,
    )
    return response


def clear_multistage_cache() -> None:
    _MODEL_CACHE.clear()


def _prepare(payload: dict[str, Any]) -> PreparedMultistage:
    settings = require_dict(payload.get("settings", {}), "settings")
    if require_list(payload.get("flex_loads", []), "flex_loads"):
        raise ProtocolError("multistage shadow does not yet support flex_loads")
    if require_list(payload.get("thermal_loads", []), "thermal_loads"):
        raise ProtocolError("multistage shadow does not yet support thermal_loads")
    slots = tuple(
        require_dict(raw, f"slots[{i}]")
        for i, raw in enumerate(require_list(payload.get("slots", []), "slots"))
    )
    if not slots:
        raise ProtocolError("slots must not be empty")
    n = len(slots)
    mode = _mode(payload)
    dt_h = np.asarray(
        [positive_number(slot.get("len_min", 0), f"slots[{i}].len_min") / 60.0 for i, slot in enumerate(slots)]
    )
    price = np.asarray(
        [finite_number(slot.get("price_ore"), f"slots[{i}].price_ore") for i, slot in enumerate(slots)]
    )
    export_price = np.asarray([_export_price(slot, settings) for slot in slots])
    confidence = np.asarray(
        [
            min(1.0, max(0.0, finite_number(slot.get("confidence", 1), f"slots[{i}].confidence")))
            for i, slot in enumerate(slots)
        ]
    )
    confidence[confidence == 0] = 1.0
    effective_import = confidence * price + (1.0 - confidence) * float(np.mean(price))
    effective_export = confidence * export_price + (1.0 - confidence) * float(np.mean(export_price))
    formulation = str(settings.get("formulation", "auto"))
    if formulation not in {"auto", "milp", "relaxed"}:
        raise ProtocolError("settings.formulation must be auto, milp, or relaxed")
    force_milp = formulation == "milp"
    pv_charge_bonus = max(
        0.0,
        finite_number(
            settings.get("pv_charge_bonus_ore_kwh", 0),
            "settings.pv_charge_bonus_ore_kwh",
        ),
    )
    unsafe_cycle = bool(np.any(effective_import < -1e-9)) or pv_charge_bonus > 0
    unsafe_meter_split = bool(
        np.any(effective_import < effective_export - 1e-9)
    )
    storage_discrete = force_milp or (formulation == "auto" and unsafe_cycle)
    meter_discrete = force_milp or (
        formulation == "auto" and unsafe_meter_split
    )
    base_load = np.asarray(
        [finite_number(slot.get("load_w", 0), f"slots[{i}].load_w") for i, slot in enumerate(slots)]
    )
    base_pv = np.asarray(
        [finite_number(slot.get("pv_w", 0), f"slots[{i}].pv_w") for i, slot in enumerate(slots)]
    )
    if np.any(base_load < -1e-9) or np.any(base_pv > 1e-9):
        raise ProtocolError("site convention requires load_w >= 0 and pv_w <= 0")

    scenario_limit = _positive_int(settings.get("scenario_limit", 12), "settings.scenario_limit")
    parsed_scenarios = parse_scenarios(payload, n, base_load, base_pv)
    scenario_set = reduce_scenarios(parsed_scenarios, scenario_limit, dt_h)
    first_stage_slots = _positive_int(
        settings.get("non_anticipative_slots", 1),
        "settings.non_anticipative_slots",
    )
    branch_interval = _positive_int(
        settings.get("branch_interval_slots", 4), "settings.branch_interval_slots"
    )
    branch_horizon = _positive_int(
        settings.get("branch_horizon_slots", min(n, 48)), "settings.branch_horizon_slots"
    )
    max_branching = _positive_int(settings.get("max_branching", 2), "settings.max_branching")
    tree = build_scenario_tree(
        scenario_set.scenarios,
        n,
        first_stage_slots,
        branch_interval,
        branch_horizon,
        max_branching,
    )
    near_horizon = _positive_int(
        settings.get("near_horizon_slots", min(n, 16)), "settings.near_horizon_slots"
    )
    mid_horizon = _positive_int(
        settings.get("mid_horizon_slots", min(n, 96)), "settings.mid_horizon_slots"
    )
    blocks = decision_blocks(
        n,
        min(n, near_horizon),
        min(n, max(near_horizon, mid_horizon)),
        _positive_int(settings.get("mid_block_slots", 2), "settings.mid_block_slots"),
        _positive_int(settings.get("far_block_slots", 4), "settings.far_block_slots"),
        tree.branch_slots,
    )

    storage_specs = tuple(
        require_dict(raw, f"storages[{i}]")
        for i, raw in enumerate(require_list(payload.get("storages", []), "storages"))
    )
    _validate_storages(storage_specs, n)
    if not storage_specs:
        raise ProtocolError("multistage shadow requires at least one storage")

    max_site_power = max(
        1000.0,
        max(float(np.max(scenario.load + np.maximum(0.0, -scenario.pv))) for scenario in scenario_set.scenarios)
        + sum(
            float(spec.get("max_charge_w", 0)) + float(spec.get("max_discharge_w", 0))
            for spec in storage_specs
        ),
    )
    raw_import_limit = np.asarray(
        [
            max(
                0.0,
                finite_number(
                    slot.get("max_import_w", 0), f"slots[{i}].max_import_w"
                ),
            )
            for i, slot in enumerate(slots)
        ]
    )
    raw_export_limit = np.asarray(
        [
            max(
                0.0,
                finite_number(
                    slot.get("max_export_w", 0), f"slots[{i}].max_export_w"
                ),
            )
            for i, slot in enumerate(slots)
        ]
    )
    import_bound = np.where(raw_import_limit > 0, raw_import_limit, max_site_power)
    export_bound = np.where(raw_export_limit > 0, raw_export_limit, max_site_power)

    service_alpha = finite_number(
        settings.get("service_cvar_alpha", 0.95), "settings.service_cvar_alpha"
    )
    economic_alpha = finite_number(
        settings.get("economic_cvar_alpha", 0.9), "settings.economic_cvar_alpha"
    )
    if not 0 < service_alpha < 1 or not 0 < economic_alpha < 1:
        raise ProtocolError("CVaR alpha values must be between 0 and 1")
    return PreparedMultistage(
        payload=payload,
        settings=settings,
        slots=slots,
        n=n,
        mode=mode,
        formulation=formulation,
        unsafe_cycle=unsafe_cycle,
        unsafe_meter_split=unsafe_meter_split,
        storage_discrete=storage_discrete,
        meter_discrete=meter_discrete,
        discrete=storage_discrete or meter_discrete,
        dt_h=dt_h,
        price=price,
        export_price=export_price,
        effective_import=effective_import,
        effective_export=effective_export,
        base_load=base_load,
        base_pv=base_pv,
        scenario_set=scenario_set,
        tree=tree,
        blocks=blocks,
        storages=storage_specs,
        first_stage_slots=first_stage_slots,
        service_cvar_weight=max(
            0.0,
            finite_number(settings.get("service_cvar_weight", 1.0), "settings.service_cvar_weight"),
        ),
        service_cvar_alpha=service_alpha,
        economic_cvar_weight=max(
            0.0,
            finite_number(settings.get("economic_cvar_weight", 0), "settings.economic_cvar_weight"),
        ),
        economic_cvar_alpha=economic_alpha,
        max_site_power=max_site_power,
        import_bound=import_bound,
        export_bound=export_bound,
    )


def _replace_scenarios(prepared: PreparedMultistage, scenario_set: ScenarioSet) -> PreparedMultistage:
    tree = build_scenario_tree(
        scenario_set.scenarios,
        prepared.n,
        prepared.first_stage_slots,
        _positive_int(prepared.settings.get("branch_interval_slots", 4), "settings.branch_interval_slots"),
        _positive_int(
            prepared.settings.get("branch_horizon_slots", min(prepared.n, 48)),
            "settings.branch_horizon_slots",
        ),
        _positive_int(prepared.settings.get("max_branching", 2), "settings.max_branching"),
    )
    near_slots = min(
        prepared.n,
        _positive_int(prepared.settings.get("near_horizon_slots", min(prepared.n, 16)), "settings.near_horizon_slots"),
    )
    mid_slots = min(
        prepared.n,
        max(
            near_slots,
            _positive_int(
                prepared.settings.get("mid_horizon_slots", min(prepared.n, 96)),
                "settings.mid_horizon_slots",
            ),
        ),
    )
    blocks = decision_blocks(
        prepared.n,
        near_slots,
        mid_slots,
        _positive_int(prepared.settings.get("mid_block_slots", 2), "settings.mid_block_slots"),
        _positive_int(prepared.settings.get("far_block_slots", 4), "settings.far_block_slots"),
        tree.branch_slots,
    )
    return PreparedMultistage(**{**prepared.__dict__, "scenario_set": scenario_set, "tree": tree, "blocks": blocks})


def _validate_storages(storages: tuple[dict[str, Any], ...], n: int) -> None:
    ids: set[str] = set()
    for i, spec in enumerate(storages):
        storage_id = spec.get("id")
        if not isinstance(storage_id, str) or not storage_id or storage_id in ids:
            raise ProtocolError(f"storages[{i}].id must be non-empty and unique")
        ids.add(storage_id)
        capacity = positive_number(spec.get("capacity_wh"), f"storages[{i}].capacity_wh")
        minimum = finite_number(spec.get("min_energy_wh", 0), f"storages[{i}].min_energy_wh")
        maximum = finite_number(spec.get("max_energy_wh", capacity), f"storages[{i}].max_energy_wh")
        initial = finite_number(spec.get("initial_energy_wh"), f"storages[{i}].initial_energy_wh")
        if not (0 <= minimum <= maximum <= capacity + 1e-6 and 0 <= initial <= capacity + 1e-6):
            raise ProtocolError(f"storages[{i}] energy bounds are inconsistent")
        eta_c = positive_number(spec.get("charge_efficiency", 0.95), f"storages[{i}].charge_efficiency")
        eta_d = positive_number(spec.get("discharge_efficiency", 0.95), f"storages[{i}].discharge_efficiency")
        if eta_c > 1 or eta_d > 1:
            raise ProtocolError(f"storages[{i}] efficiencies must be <= 1")
        if spec.get("target_energy_wh") is not None:
            deadline = int(spec.get("target_slot", n - 1))
            if deadline < 0 or deadline >= n:
                raise ProtocolError(f"storages[{i}].target_slot must be within the horizon")


def _cache_key(prepared: PreparedMultistage) -> tuple[Any, ...]:
    storage_shape = tuple(
        (
            str(spec["id"]),
            float(spec["capacity_wh"]),
            float(spec.get("min_energy_wh", 0)),
            float(spec.get("max_energy_wh", spec["capacity_wh"])),
            float(spec.get("max_charge_w", 0)),
            float(spec.get("max_discharge_w", 0)),
            float(spec.get("charge_efficiency", 0.95)),
            float(spec.get("discharge_efficiency", 0.95)),
            spec.get("target_energy_wh") is not None,
            int(spec.get("target_slot", prepared.n - 1)),
        )
        for spec in prepared.storages
    )
    return (
        prepared.n,
        tuple(float(value) for value in prepared.dt_h),
        prepared.mode,
        prepared.formulation,
        prepared.storage_discrete,
        prepared.meter_discrete,
        tuple(round(scenario.probability, 12) for scenario in prepared.scenario_set.scenarios),
        tuple(int(value) for value in prepared.tree.node_at.flat),
        prepared.blocks,
        storage_shape,
        round(prepared.service_cvar_weight, 12),
        round(prepared.service_cvar_alpha, 12),
        round(prepared.economic_cvar_weight, 12),
        round(prepared.economic_cvar_alpha, 12),
    )


def _compile(prepared: PreparedMultistage, key: tuple[Any, ...]) -> CompiledMultistage:
    started = time.perf_counter()
    m = len(prepared.scenario_set.scenarios)
    n = prepared.n
    probabilities = np.asarray([scenario.probability for scenario in prepared.scenario_set.scenarios])
    load = cp.Parameter((m, n), nonneg=True, name="ms_load")
    pv_generation = cp.Parameter((m, n), nonneg=True, name="ms_pv_generation")
    pv_surplus = cp.Parameter((m, n), nonneg=True, name="ms_pv_surplus")
    base_import = cp.Parameter((m, n), nonneg=True, name="ms_base_import")
    import_bound = cp.Parameter(n, nonneg=True, name="ms_import_bound")
    export_bound = cp.Parameter(n, nonneg=True, name="ms_export_bound")
    big_m = cp.Parameter(nonneg=True, name="ms_big_m")
    import_coeff = cp.Parameter(n, name="ms_import_coeff")
    export_coeff = cp.Parameter(n, name="ms_export_coeff")
    strict_coeff = cp.Parameter(n, nonneg=True, name="ms_strict_coeff")
    pv_bonus = cp.Parameter(nonneg=True, name="ms_pv_bonus")
    service_cap = cp.Parameter(nonneg=True, name="ms_service_cap")
    service_cap.value = 1e9

    initial_energy: list[cp.Parameter] = []
    lower_recovery: list[cp.Parameter] = []
    upper_recovery: list[cp.Parameter] = []
    target_energy: list[cp.Parameter | None] = []
    cycle_coeff: list[cp.Parameter] = []
    terminal_price: list[cp.Parameter] = []
    for i, spec in enumerate(prepared.storages):
        initial_energy.append(cp.Parameter(nonneg=True, name=f"ms_storage_{i}_initial"))
        lower_recovery.append(cp.Parameter(nonneg=True, name=f"ms_storage_{i}_lower_initial"))
        upper_recovery.append(cp.Parameter(nonneg=True, name=f"ms_storage_{i}_upper_initial"))
        target_energy.append(
            cp.Parameter(nonneg=True, name=f"ms_storage_{i}_target")
            if spec.get("target_energy_wh") is not None
            else None
        )
        cycle_coeff.append(cp.Parameter(nonneg=True, name=f"ms_storage_{i}_cycle"))
        terminal_price.append(cp.Parameter(name=f"ms_storage_{i}_terminal"))

    constraints: list[cp.Constraint] = []
    scenario_vars: list[ScenarioVars] = []
    scenario_services: list[cp.Expression] = []
    scenario_economics: list[cp.Expression] = []
    block_start_at = np.zeros(n, dtype=np.int64)
    for start, end in prepared.blocks:
        block_start_at[start:end] = start

    # Physical actions are information-node decisions, not per-scenario
    # copies joined by equalities. This is the compact extensive form: only
    # state trajectories and meter flows are scenario-specific.
    storage_actions: dict[tuple[int, int, int], tuple[cp.Variable, cp.Variable]] = {}
    curtail_actions: dict[tuple[int, int], cp.Variable] = {}
    for si in range(m):
        storage_vars: list[ScenarioStorageVars] = []
        total_charge: cp.Expression = cp.Constant(np.zeros(n))
        total_discharge: cp.Expression = cp.Constant(np.zeros(n))
        service: cp.Expression = cp.Constant(0.0)
        cycle_cost: cp.Expression = cp.Constant(0.0)
        terminal_credit: cp.Expression = cp.Constant(0.0)
        for storage_index, spec in enumerate(prepared.storages):
            capacity = float(spec["capacity_wh"])
            minimum = float(spec.get("min_energy_wh", 0))
            maximum = float(spec.get("max_energy_wh", capacity))
            max_charge = max(0.0, float(spec.get("max_charge_w", 0)))
            max_discharge = max(0.0, float(spec.get("max_discharge_w", 0)))
            eta_c = float(spec.get("charge_efficiency", 0.95))
            eta_d = float(spec.get("discharge_efficiency", 0.95))
            charge_values: list[cp.Expression] = []
            discharge_values: list[cp.Expression] = []
            for t in range(n):
                block_start = int(block_start_at[t])
                node = int(prepared.tree.node_at[si, block_start])
                key = (storage_index, node, block_start)
                action = storage_actions.get(key)
                if action is None:
                    charge_var = cp.Variable(
                        nonneg=True,
                        name=f"ms_b{storage_index}_n{node}_t{block_start}_charge",
                    )
                    discharge_var = cp.Variable(
                        nonneg=True,
                        name=f"ms_b{storage_index}_n{node}_t{block_start}_discharge",
                    )
                    if prepared.storage_discrete:
                        direction = cp.Variable(
                            boolean=True,
                            name=f"ms_b{storage_index}_n{node}_t{block_start}_direction",
                        )
                        constraints += [
                            charge_var <= max_charge * direction,
                            discharge_var <= max_discharge * (1 - direction),
                        ]
                    else:
                        constraints += [
                            charge_var <= max_charge,
                            discharge_var <= max_discharge,
                        ]
                    action = (charge_var, discharge_var)
                    storage_actions[key] = action
                charge_values.append(action[0])
                discharge_values.append(action[1])
            charge = cp.hstack(charge_values)
            discharge = cp.hstack(discharge_values)
            energy = cp.Variable(n + 1, name=f"ms_s{si}_b{storage_index}_energy")
            lower = cp.Variable(n + 1, nonneg=True, name=f"ms_s{si}_b{storage_index}_lower")
            upper = cp.Variable(n + 1, nonneg=True, name=f"ms_s{si}_b{storage_index}_upper")
            constraints += [
                energy[0] == initial_energy[storage_index],
                energy[1:]
                == energy[:-1]
                + cp.multiply(prepared.dt_h, eta_c * charge - discharge / eta_d),
                energy >= 0,
                energy <= capacity,
                lower[0] == lower_recovery[storage_index],
                upper[0] == upper_recovery[storage_index],
                lower >= minimum - energy,
                upper >= energy - maximum,
                lower[1:] <= lower[:-1],
                upper[1:] <= upper[:-1],
            ]
            service += cp.sum(lower[1:] + upper[1:]) / (capacity * n)
            target = target_energy[storage_index]
            if target is not None:
                deadline = int(spec.get("target_slot", n - 1))
                shortfall = cp.Variable(nonneg=True, name=f"ms_s{si}_b{storage_index}_shortfall")
                constraints.append(energy[deadline + 1] + shortfall >= target)
                service += shortfall / capacity
            cycle_cost += cycle_coeff[storage_index] * cp.sum(
                cp.multiply(prepared.dt_h, discharge)
            ) / 1000.0
            terminal_credit += terminal_price[storage_index] * energy[-1] / 1000.0
            total_charge += charge
            total_discharge += discharge
            storage_vars.append(ScenarioStorageVars(charge, discharge, energy))

        curtail_values: list[cp.Expression] = []
        for t in range(n):
            node = int(prepared.tree.node_at[si, t])
            key = (node, t)
            curtail_var = curtail_actions.get(key)
            if curtail_var is None:
                curtail_var = cp.Variable(nonneg=True, name=f"ms_n{node}_t{t}_curtail")
                curtail_actions[key] = curtail_var
            constraints.append(curtail_var <= pv_generation[si, t])
            curtail_values.append(curtail_var)
        curtail = cp.hstack(curtail_values)
        grid_import = cp.Variable(n, nonneg=True, name=f"ms_s{si}_import")
        grid_export = cp.Variable(n, nonneg=True, name=f"ms_s{si}_export")
        net_without_storage = load[si] - pv_generation[si] + curtail
        constraints += [
            grid_import - grid_export == net_without_storage + total_charge - total_discharge,
            grid_import <= import_bound,
            grid_export <= export_bound,
        ]
        if prepared.meter_discrete:
            meter_direction = cp.Variable(
                n, boolean=True, name=f"ms_s{si}_meter_direction"
            )
            constraints += [
                grid_import <= big_m * meter_direction,
                grid_export <= big_m * (1 - meter_direction),
            ]

        if prepared.mode in {"self_consumption", "cheap_charge", "passive_arbitrage"}:
            if prepared.mode == "self_consumption":
                constraints += [
                    grid_import <= base_import[si] + 50.0,
                    grid_export <= pv_surplus[si] + 50.0,
                ]
            else:
                constraints.append(grid_export <= pv_surplus[si] + 1e-6)

        strict_penalty: cp.Expression = cp.Constant(0.0)
        if prepared.mode in {"self_consumption", "passive_arbitrage"}:
            house_import = cp.Variable(n, nonneg=True, name=f"ms_s{si}_house_import")
            constraints.append(house_import >= net_without_storage + total_charge - total_discharge)
            strict_penalty = cp.sum(cp.multiply(strict_coeff, house_import))

        charge_from_pv = cp.Variable(n, nonneg=True, name=f"ms_s{si}_charge_from_pv")
        constraints += [
            charge_from_pv <= total_charge,
            charge_from_pv <= pv_surplus[si],
        ]
        pv_credit = pv_bonus * cp.sum(cp.multiply(prepared.dt_h, charge_from_pv)) / 1000.0
        raw_cost = cp.sum(
            cp.multiply(import_coeff, grid_import) - cp.multiply(export_coeff, grid_export)
        )
        economic = raw_cost + strict_penalty + cycle_cost - terminal_credit - pv_credit
        scenario_vars.append(
            ScenarioVars(storage_vars, curtail, grid_import, grid_export, service, economic)
        )
        scenario_services.append(service)
        scenario_economics.append(economic)

    services = cp.hstack(scenario_services)
    economics = cp.hstack(scenario_economics)
    expected_service = probabilities @ services
    service_threshold = cp.Variable(name="ms_service_cvar_threshold")
    service_excess = cp.Variable(m, nonneg=True, name="ms_service_cvar_excess")
    constraints += [service_excess >= services - service_threshold]
    service_cvar = service_threshold + probabilities @ service_excess / (
        1.0 - prepared.service_cvar_alpha
    )
    service_metric = expected_service + prepared.service_cvar_weight * service_cvar

    expected_economic = probabilities @ economics
    economic_objective: cp.Expression = expected_economic
    if prepared.economic_cvar_weight > 0 and m > 1:
        economic_threshold = cp.Variable(name="ms_economic_cvar_threshold")
        economic_excess = cp.Variable(m, nonneg=True, name="ms_economic_cvar_excess")
        constraints += [economic_excess >= economics - economic_threshold]
        economic_cvar = economic_threshold + probabilities @ economic_excess / (
            1.0 - prepared.economic_cvar_alpha
        )
        economic_objective += prepared.economic_cvar_weight * economic_cvar

    service_problem = cp.Problem(cp.Minimize(service_metric), constraints)
    economic_problem = cp.Problem(
        cp.Minimize(economic_objective), constraints + [service_metric <= service_cap]
    )
    if not service_problem.is_dpp() or not economic_problem.is_dpp():
        raise RuntimeError("multistage CVXPY model is not DPP-compliant")
    build_ms = (time.perf_counter() - started) * 1000.0
    return CompiledMultistage(
        key=key,
        prepared_shape=prepared,
        load=load,
        pv_generation=pv_generation,
        pv_surplus=pv_surplus,
        base_import=base_import,
        import_bound=import_bound,
        export_bound=export_bound,
        big_m=big_m,
        import_coeff=import_coeff,
        export_coeff=export_coeff,
        strict_coeff=strict_coeff,
        initial_energy=initial_energy,
        lower_recovery=lower_recovery,
        upper_recovery=upper_recovery,
        target_energy=target_energy,
        cycle_coeff=cycle_coeff,
        terminal_price=terminal_price,
        pv_bonus=pv_bonus,
        service_cap=service_cap,
        scenario_vars=scenario_vars,
        service_metric=service_metric,
        service_problem=service_problem,
        economic_problem=economic_problem,
        build_ms=build_ms,
    )


def _run_problem(problem: cp.Problem, settings: dict[str, Any], solver_name: str) -> None:
    solver = cp.HIGHS if solver_name == "HIGHS" else cp.CLARABEL
    problem.solve(solver=solver, warm_start=True, enforce_dpp=True, **_solver_options(settings, solver))


def _response(
    prepared: PreparedMultistage,
    compiled: CompiledMultistage,
    best_service: float,
    solver_name: str,
    started: float,
    prepare_ms: float,
    solver_ms: float,
    cache_hit: bool,
    decomposition: str,
    fallback_reason: str,
) -> dict[str, Any]:
    scenarios = prepared.scenario_set.scenarios
    base_index = next((i for i, scenario in enumerate(scenarios) if scenario.id == "base"), 0)
    base = scenarios[base_index]
    base_vars = compiled.scenario_vars[base_index]
    total_capacity = sum(float(spec["capacity_wh"]) for spec in prepared.storages)
    initial_total = sum(float(spec["initial_energy_wh"]) for spec in prepared.storages)
    charge_values = [np.asarray(storage.charge.value) for storage in base_vars.storages]
    discharge_values = [np.asarray(storage.discharge.value) for storage in base_vars.storages]
    energy_values = [np.asarray(storage.energy.value) for storage in base_vars.storages]
    grid_import_values = np.asarray(base_vars.grid_import.value)
    grid_export_values = np.asarray(base_vars.grid_export.value)
    curtail_values = np.asarray(base_vars.curtail.value)
    actions: list[dict[str, Any]] = []
    raw_total_cost = 0.0
    for t, slot in enumerate(prepared.slots):
        storage_power: dict[str, float] = {}
        storage_energy: dict[str, float] = {}
        battery_w = 0.0
        stored_wh = 0.0
        for i, storage in enumerate(base_vars.storages):
            power = float(charge_values[i][t] - discharge_values[i][t])
            energy = float(energy_values[i][t + 1])
            storage_id = str(prepared.storages[i]["id"])
            storage_power[storage_id] = power
            storage_energy[storage_id] = energy
            battery_w += power
            stored_wh += energy
        grid_w = float(grid_import_values[t] - grid_export_values[t])
        grid_kwh = grid_w * prepared.dt_h[t] / 1000.0
        raw_cost = prepared.price[t] * max(grid_kwh, 0.0) - prepared.export_price[t] * max(-grid_kwh, 0.0)
        raw_total_cost += raw_cost
        curtailed_w = max(0.0, float(curtail_values[t]))
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

    extra = getattr(compiled.economic_problem.solver_stats, "extra_stats", None)
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
        "request_id": str(prepared.payload["request_id"]),
        "ok": True,
        "solver": {
            "engine": "cvxpy",
            "backend": solver_name.lower(),
            "status": str(compiled.economic_problem.status),
            "formulation": "multistage-milp" if prepared.discrete else "multistage-lp",
            "objective_ore": float(compiled.economic_problem.value),
            "service_slack": best_service,
            "solve_ms": solve_ms,
            "prepare_ms": prepare_ms,
            "build_ms": 0.0 if cache_hit else compiled.build_ms,
            "solver_ms": solver_ms,
            "cache_hit": cache_hit,
            "dpp": True,
            "mip_gap": mip_gap,
            "scenario_count": len(scenarios),
            "scenario_original_count": prepared.scenario_set.original_count,
            "scenario_reduction_error": prepared.scenario_set.reduction_error,
            "scenario_policy": "multistage",
            "policy_version": POLICY_VERSION,
            "policy_config": policy_config(prepared),
            "non_anticipative_slots": prepared.first_stage_slots,
            "tree_nodes": prepared.tree.node_count,
            "move_blocks": len(prepared.blocks),
            "decomposition": decomposition,
            "risk_model": "service-cvar-then-expected-cost",
            "service_cvar_weight": prepared.service_cvar_weight,
            "service_cvar_alpha": prepared.service_cvar_alpha,
            "economic_cvar_weight": prepared.economic_cvar_weight,
            "economic_cvar_alpha": prepared.economic_cvar_alpha,
            "fallback": bool(fallback_reason),
            "fallback_reason": fallback_reason,
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


def policy_config(prepared: PreparedMultistage) -> str:
    settings = prepared.settings
    values = {
        "backend": str(settings.get("multistage_backend", "auto")),
        "formulation": prepared.formulation,
        "scenario_limit": _positive_int(
            settings.get("scenario_limit", 12), "settings.scenario_limit"
        ),
        "branch_interval_slots": _positive_int(
            settings.get("branch_interval_slots", 4),
            "settings.branch_interval_slots",
        ),
        "branch_horizon_slots": _positive_int(
            settings.get("branch_horizon_slots", min(prepared.n, 48)),
            "settings.branch_horizon_slots",
        ),
        "max_branching": _positive_int(
            settings.get("max_branching", 2), "settings.max_branching"
        ),
        "near_horizon_slots": _positive_int(
            settings.get("near_horizon_slots", min(prepared.n, 16)),
            "settings.near_horizon_slots",
        ),
        "mid_horizon_slots": _positive_int(
            settings.get("mid_horizon_slots", min(prepared.n, 96)),
            "settings.mid_horizon_slots",
        ),
        "mid_block_slots": _positive_int(
            settings.get("mid_block_slots", 2), "settings.mid_block_slots"
        ),
        "far_block_slots": _positive_int(
            settings.get("far_block_slots", 4), "settings.far_block_slots"
        ),
        "decomposition_threshold": _positive_int(
            settings.get("decomposition_threshold", 20),
            "settings.decomposition_threshold",
        ),
        "decomposition_method": str(settings.get("decomposition_method", "auto")),
        "ph_max_iterations": _positive_int(
            settings.get("ph_max_iterations", 8), "settings.ph_max_iterations"
        ),
        "ph_rho": finite_number(settings.get("ph_rho", 50), "settings.ph_rho"),
        "ph_tolerance_w": finite_number(
            settings.get("ph_tolerance_w", 5), "settings.ph_tolerance_w"
        ),
    }
    return json.dumps(values, sort_keys=True, separators=(",", ":"))
