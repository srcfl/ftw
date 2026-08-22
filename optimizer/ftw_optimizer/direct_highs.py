from __future__ import annotations

import math
import time
from dataclasses import dataclass
from typing import Any, TYPE_CHECKING

import highspy
import numpy as np

from . import SCHEMA_VERSION
from .deadline import SolveCancelled, SolveDeadline, SolveDeadlineExceeded
from .model import (
    _arbitrage_spread_ore_kwh,
    _solver_options,
    _storage_starts_above_maximum,
)
from .preference import (
    COST_BOUND_TOLERANCE_ORE,
    INTEGRALITY_TOLERANCE,
    MIN_PREFERENCE_TIME_S,
    STAGE_DISABLED,
    STAGE_FLAT,
    STAGE_KEPT,
    STAGE_NO_TIME,
    STAGE_SINGLE_SLOT,
    cost_bound_slack_ore,
    flatten_horizon_has_choice,
    flatten_peaks_enabled,
    named_shortfalls,
    peaks_from_grid,
    preference_time_limit_s,
)
from .protocol import finite_number

if TYPE_CHECKING:
    from .multistage import PreparedMultistage


class DirectHighsError(RuntimeError):
    pass


SIMULTANEOUS_STORAGE_CYCLE_ERROR = (
    "HiGHS returned simultaneous storage charge and discharge"
)
SHARED_BASELINE_REPLAY_ERROR = (
    "direct shared mode violates the post-curtailment baseline"
)


class SharedBaselineReplayError(DirectHighsError):
    def __init__(self, build_ms: float, solver_ms: float) -> None:
        super().__init__(SHARED_BASELINE_REPLAY_ERROR)
        self.build_ms = build_ms
        self.solver_ms = solver_ms


@dataclass
class DirectScenarioVars:
    charge: list[list[int]]
    discharge: list[list[int]]
    energy: list[list[int]]
    curtail: list[int]
    grid_import: list[int]
    grid_export: list[int]


@dataclass
class DirectSharedStorageVars:
    charge: list[list[int]]
    discharge: list[list[int]]
    energy: list[list[int]]
    total_charge: list[list[int]]
    total_discharge: list[list[int]]
    service: dict[int, float]
    economic: dict[int, float]
    shortfalls: dict[str, int]


class SparseModel:
    def __init__(self) -> None:
        self.lower: list[float] = []
        self.upper: list[float] = []
        self.rows: list[tuple[dict[int, float], float, float]] = []
        self.integer: list[int] = []

    def variable(
        self,
        lower: float = 0.0,
        upper: float = highspy.kHighsInf,
        *,
        integer: bool = False,
    ) -> int:
        index = len(self.lower)
        self.lower.append(lower)
        self.upper.append(upper)
        if integer:
            self.integer.append(index)
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

    def build(
        self,
        costs: np.ndarray,
        settings: dict[str, Any],
        *,
        time_limit_s: float,
    ) -> highspy.Highs:
        highs = highspy.Highs()
        highs.setOptionValue("output_flag", False)
        options = _solver_options(settings, "HIGHS")
        highs.setOptionValue(
            "time_limit", min(float(options["time_limit"]), time_limit_s)
        )
        highs.setOptionValue("mip_rel_gap", float(options["mip_rel_gap"]))
        lower = np.asarray(self.lower, dtype=np.float64)
        upper = np.asarray(self.upper, dtype=np.float64)
        _require_ok(highs.addVars(len(lower), lower, upper), "add variables")
        indices = np.arange(len(lower), dtype=np.int32)
        _require_ok(highs.changeColsCost(len(lower), indices, costs), "set objective")
        if self.integer:
            integer_indices = np.asarray(self.integer, dtype=np.int32)
            integer_types = np.full(
                len(integer_indices),
                highspy.HighsVarType.kInteger,
                dtype=np.uint8,
            )
            _require_ok(
                highs.changeColsIntegrality(
                    len(integer_indices), integer_indices, integer_types
                ),
                "set integer variables",
            )

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
    *,
    shared: bool = False,
    exact_shared_baseline: bool = False,
    deadline: SolveDeadline | float | None = None,
    prior_build_ms: float = 0.0,
    prior_solver_ms: float = 0.0,
) -> dict[str, Any]:
    if _storage_starts_above_maximum(prepared.storages):
        raise DirectHighsError(
            "direct HiGHS path requires storage starts at or below the operating maximum"
        )
    if prepared.discrete or prepared.unsafe_cycle or prepared.unsafe_meter_split:
        raise DirectHighsError("direct HiGHS path requires a cycle-safe continuous tariff")
    if deadline is None:
        deadline = SolveDeadline(
            started
            + float(_solver_options(prepared.settings, "HIGHS")["time_limit"])
        )
    _remaining_time_s(deadline)
    build_started = time.perf_counter()
    model = SparseModel()
    m = len(prepared.scenario_set.scenarios)
    n = prepared.n
    probabilities = np.asarray(
        [scenario.probability for scenario in prepared.scenario_set.scenarios]
    )
    service_terms: list[dict[int, float]] = []
    economic_terms: list[dict[int, float]] = []
    risk_terms: list[dict[int, float]] = []
    scenario_vars: list[DirectScenarioVars] = []

    block_start_at = np.zeros(n, dtype=np.int64)
    for block_start, block_end in prepared.blocks:
        block_start_at[block_start:block_end] = block_start

    storage_actions: dict[tuple[int, int, int], tuple[int, int]] = {}
    shared_storage: DirectSharedStorageVars | None = None
    curtail_upper: dict[tuple[int, int], float] = {}
    for si, scenario in enumerate(prepared.scenario_set.scenarios):
        pv_generation = np.maximum(0.0, -scenario.pv)
        for t in range(n):
            key = (int(prepared.tree.node_at[si, t]), t)
            curtail_upper[key] = min(curtail_upper.get(key, math.inf), float(pv_generation[t]))
    curtail_actions = {
        key: model.variable(0.0, upper) for key, upper in sorted(curtail_upper.items())
    }

    spread = _arbitrage_spread_ore_kwh(prepared.settings, prepared.mode)
    for si, scenario in enumerate(prepared.scenario_set.scenarios):
        pv_generation = np.maximum(0.0, -scenario.pv)
        pv_surplus = np.maximum(0.0, pv_generation - scenario.load)
        base_import = np.maximum(0.0, scenario.load - pv_generation)
        charges = shared_storage.charge if shared_storage is not None else []
        discharges = shared_storage.discharge if shared_storage is not None else []
        energies = shared_storage.energy if shared_storage is not None else []
        total_charge = (
            shared_storage.total_charge
            if shared_storage is not None
            else [[] for _ in range(n)]
        )
        total_discharge = (
            shared_storage.total_discharge
            if shared_storage is not None
            else [[] for _ in range(n)]
        )
        service = dict(shared_storage.service) if shared_storage is not None else {}
        economic = dict(shared_storage.economic) if shared_storage is not None else {}
        shortfalls = (
            dict(shared_storage.shortfalls) if shared_storage is not None else {}
        )
        # Shared charge and discharge make storage state deterministic across
        # scenarios. Reuse its variables and rows; only meter flow varies.
        storages_to_build = () if shared_storage is not None else prepared.storages
        for storage_index, spec in enumerate(storages_to_build):
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
                target_slot = int(spec.get("target_slot", n - 1))
                shortfall = model.variable()
                target = float(spec["target_energy_wh"])
                model.row(
                    {energy[target_slot + 1]: 1.0, shortfall: 1.0},
                    target,
                )
                _add(service, shortfall, 1.0 / capacity)
                shortfalls[str(spec.get("id", f"storage-{storage_index}"))] = shortfall

            cycle_coefficient = spread + max(0.0, float(spec.get("cycle_cost_ore_kwh", 0)))
            throughput_coefficient = (
                max(0.0, float(spec.get("throughput_cost_ore_kwh", 0)))
                if shared
                else 0.0
            )
            for t in range(n):
                _add(
                    economic,
                    discharge[t],
                    cycle_coefficient * float(prepared.dt_h[t]) / 1000.0,
                )
                if throughput_coefficient > 0:
                    coefficient = (
                        throughput_coefficient
                        * float(prepared.dt_h[t])
                        / 1000.0
                    )
                    _add(economic, charge[t], coefficient)
                    _add(economic, discharge[t], coefficient)
            _add(
                economic,
                energy[-1],
                -float(spec.get("terminal_price_ore_kwh", 0)) / 1000.0,
            )
            charges.append(charge)
            discharges.append(discharge)
            energies.append(energy)
        if shared and shared_storage is None:
            shared_storage = DirectSharedStorageVars(
                charges,
                discharges,
                energies,
                total_charge,
                total_discharge,
                dict(service),
                dict(economic),
                dict(shortfalls),
            )
        scenario_grid_cost: dict[int, float] = {}

        curtail = [
            curtail_actions[(int(prepared.tree.node_at[si, t]), t)] for t in range(n)
        ]
        grid_import: list[int] = []
        grid_export: list[int] = []
        for t in range(n):
            net = float(scenario.load[t] - pv_generation[t])
            shared_baseline_mode = shared and prepared.mode in {
                "self_consumption",
                "cheap_charge",
                "passive_arbitrage",
            }
            baseline_stays_import = False
            baseline_crosses = False
            baseline_import_index: int | None = None
            baseline_export_index: int | None = None
            if shared_baseline_mode:
                curtail_ceiling = float(
                    curtail_upper[
                        (int(prepared.tree.node_at[si, t]), t)
                    ]
                )
                baseline_crosses = net < 0 < net + curtail_ceiling
                if baseline_crosses and exact_shared_baseline:
                    baseline_import_max = net + curtail_ceiling
                    baseline_export_max = -net
                    baseline_import_index = model.variable(
                        0.0, baseline_import_max
                    )
                    baseline_export_index = model.variable(
                        0.0, baseline_export_max
                    )
                    baseline_import_mode = model.variable(
                        0.0, 1.0, integer=True
                    )
                    model.row(
                        {
                            baseline_import_index: 1.0,
                            baseline_export_index: -1.0,
                            curtail[t]: -1.0,
                        },
                        net,
                        net,
                    )
                    model.row(
                        {
                            baseline_import_index: 1.0,
                            baseline_import_mode: -baseline_import_max,
                        },
                        upper=0.0,
                    )
                    model.row(
                        {
                            baseline_export_index: 1.0,
                            baseline_import_mode: baseline_export_max,
                        },
                        upper=baseline_export_max,
                    )
                baseline_stays_import = net >= 0

            import_upper = float(prepared.import_bound[t])
            export_upper = float(prepared.export_bound[t])
            if prepared.mode == "self_consumption":
                if shared_baseline_mode:
                    if not baseline_crosses and not baseline_stays_import:
                        import_upper = min(import_upper, 50.0)
                else:
                    import_upper = min(
                        import_upper, float(base_import[t]) + 50.0
                    )
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
            model.row(balance, net, net)
            if shared_baseline_mode and prepared.mode == "self_consumption":
                if baseline_crosses and exact_shared_baseline:
                    assert baseline_import_index is not None
                    assert baseline_export_index is not None
                    model.row(
                        {import_index: 1.0, baseline_import_index: -1.0},
                        upper=50.0,
                    )
                    model.row(
                        {export_index: 1.0, baseline_export_index: -1.0},
                        upper=50.0,
                    )
                elif baseline_crosses:
                    model.row(
                        {import_index: 1.0, curtail[t]: -1.0},
                        upper=50.0,
                    )
                    model.row(
                        {export_index: 1.0},
                        upper=-net + 50.0,
                    )
                elif baseline_stays_import:
                    model.row(
                        {import_index: 1.0, curtail[t]: -1.0},
                        upper=net + 50.0,
                    )
                    model.row({export_index: 1.0}, upper=50.0)
                else:
                    model.row(
                        {export_index: 1.0, curtail[t]: 1.0},
                        upper=-net + 50.0,
                    )
            elif shared_baseline_mode:
                if baseline_crosses and exact_shared_baseline:
                    assert baseline_export_index is not None
                    model.row(
                        {export_index: 1.0, baseline_export_index: -1.0},
                        upper=1e-6,
                    )
                elif baseline_crosses:
                    model.row(
                        {export_index: 1.0},
                        upper=-net + 1e-6,
                    )
                elif baseline_stays_import:
                    model.row({export_index: 1.0}, upper=1e-6)
                else:
                    model.row(
                        {export_index: 1.0, curtail[t]: 1.0},
                        upper=-net + 1e-6,
                    )
            elif prepared.mode == "self_consumption":
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
            if shared:
                _add(
                    scenario_grid_cost,
                    import_index,
                    float(
                        prepared.effective_import[t]
                        * prepared.dt_h[t]
                        / 1000.0
                    ),
                )
            _add(
                economic,
                export_index,
                -float(prepared.effective_export[t] * prepared.dt_h[t] / 1000.0),
            )
            if shared:
                _add(
                    scenario_grid_cost,
                    export_index,
                    -float(
                        prepared.effective_export[t]
                        * prepared.dt_h[t]
                        / 1000.0
                    ),
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
        risk_terms.append(scenario_grid_cost if shared else economic)
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
        for si, risk in enumerate(risk_terms):
            row = {excess[si]: 1.0, threshold: 1.0}
            for index, value in risk.items():
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

    highs = model.build(
        service_costs,
        prepared.settings,
        time_limit_s=_remaining_time_s(deadline),
    )
    build_ms = (time.perf_counter() - build_started) * 1000.0
    _require_ok(
        highs.setOptionValue("time_limit", _remaining_time_s(deadline)),
        "set service time limit",
    )
    solver_started = time.perf_counter()
    _run_optimal(highs, "service", deadline)
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
    _require_ok(
        highs.setOptionValue("time_limit", _remaining_time_s(deadline)),
        "set economic time limit",
    )
    _run_optimal(highs, "economic", deadline)
    solver_ms = (time.perf_counter() - solver_started) * 1000.0
    mip_gap = float(highs.getInfo().mip_gap) if model.integer else None
    solution = np.asarray(highs.getSolution().col_value, dtype=np.float64)
    if len(solution) != len(model.lower) or not np.all(np.isfinite(solution)):
        raise DirectHighsError("HiGHS returned a non-finite solution")
    economic_objective = float(highs.getObjectiveValue())
    flatten_started = time.perf_counter()
    flatten_stage, solution, import_peak_w, export_peak_w = _apply_direct_flatten(
        highs,
        model,
        prepared,
        scenario_vars,
        economic_costs,
        economic_objective,
        solution,
        deadline,
        shared=shared,
    )
    solver_ms += (time.perf_counter() - flatten_started) * 1000.0
    if shared:
        try:
            _validate_shared_baseline_solution(
                prepared, scenario_vars, solution
            )
        except DirectHighsError as exc:
            if (
                not exact_shared_baseline
                and str(exc) == SHARED_BASELINE_REPLAY_ERROR
            ):
                raise SharedBaselineReplayError(
                    build_ms, solver_ms
                ) from exc
            raise

    return _response(
        prepared,
        scenario_vars,
        solution,
        economic_objective,
        best_service,
        started,
        prepare_ms,
        prior_build_ms + build_ms,
        prior_solver_ms + solver_ms,
        decomposition,
        len(model.lower),
        len(model.rows),
        len(model.integer),
        mip_gap,
        shared,
        flatten_stage,
        import_peak_w,
        export_peak_w,
        _direct_storage_service_report(shared_storage, solution),
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
    integer_variables: int,
    mip_gap: float | None,
    shared: bool,
    preference_stage: str = "",
    import_peak_w: float = 0.0,
    export_peak_w: float = 0.0,
    service_report: dict[str, Any] | None = None,
) -> dict[str, Any]:
    scenarios = prepared.scenario_set.scenarios
    base_index = next((i for i, scenario in enumerate(scenarios) if scenario.id == "base"), 0)
    base = scenarios[base_index]
    base_vars = scenario_vars[base_index]
    for scenario in scenario_vars:
        for charges, discharges in zip(scenario.charge, scenario.discharge):
            for charge_index, discharge_index in zip(charges, discharges):
                if min(solution[charge_index], solution[discharge_index]) > 1e-6:
                    raise DirectHighsError(SIMULTANEOUS_STORAGE_CYCLE_ERROR)
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
        pv_forecast = prepared.base_pv if shared else base.pv
        actions.append(
            {
                "slot_start_ms": int(slot.get("start_ms", 0)),
                "slot_len_min": int(slot["len_min"]),
                "battery_w": battery_w,
                "grid_w": grid_w,
                "soc_pct": stored_wh / total_capacity * 100.0,
                "cost_ore": raw_cost,
                "pv_limit_w": max(0.0, -pv_forecast[t] - curtailed_w)
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
    solver: dict[str, Any] = {
        "engine": "highspy",
        "backend": "highs",
        "status": "optimal",
        "formulation": (
            "milp"
            if shared and integer_variables
            else "convex"
            if shared
            else "multistage-lp"
        ),
        "objective_ore": objective,
        "service_slack": best_service,
        "solve_ms": solve_ms,
        "prepare_ms": prepare_ms,
        "build_ms": build_ms,
        "solver_ms": solver_ms,
        "cache_hit": False,
        "dpp": False,
        "mip_gap": mip_gap,
        "scenario_count": len(scenarios),
        "scenario_policy": "shared" if shared else "multistage",
        "policy_version": "shared-v1" if shared else "storage-multistage-v1",
        "non_anticipative_slots": prepared.first_stage_slots,
        "model_variables": variables,
        "model_constraints": constraints,
        "preference_stage": preference_stage,
        "import_peak_w": import_peak_w,
        "export_peak_w": export_peak_w,
    }
    if service_report:
        solver["service_report"] = service_report
    if shared:
        probabilities = np.asarray([scenario.probability for scenario in scenarios])
        energy_cost = 0.0
        for probability, scenario in zip(probabilities, scenario_vars):
            for t in range(prepared.n):
                energy_cost += float(probability) * (
                    prepared.effective_import[t]
                    * prepared.dt_h[t]
                    * solution[scenario.grid_import[t]]
                    / 1000.0
                    - prepared.effective_export[t]
                    * prepared.dt_h[t]
                    * solution[scenario.grid_export[t]]
                    / 1000.0
                )
        degradation = 0.0
        terminal_value = 0.0
        spread = _arbitrage_spread_ore_kwh(prepared.settings, prepared.mode)
        for index, spec in enumerate(prepared.storages):
            discharge_rate = spread + max(
                0.0, float(spec.get("cycle_cost_ore_kwh", 0))
            )
            throughput_rate = max(
                0.0, float(spec.get("throughput_cost_ore_kwh", 0))
            )
            for t in range(prepared.n):
                degradation += (
                    prepared.dt_h[t]
                    * (
                        discharge_rate * solution[base_vars.discharge[index][t]]
                        + throughput_rate
                        * (
                            solution[base_vars.charge[index][t]]
                            + solution[base_vars.discharge[index][t]]
                        )
                    )
                    / 1000.0
                )
            terminal_value -= (
                float(spec.get("terminal_price_ore_kwh", 0))
                * solution[base_vars.energy[index][-1]]
                / 1000.0
            )
        solver.update(
            {
                "cvar_weight": prepared.economic_cvar_weight,
                "cvar_alpha": prepared.economic_cvar_alpha,
                "objective_breakdown_ore": {
                    "energy": float(energy_cost),
                    "demand_charge_increment": 0.0,
                    "degradation": float(degradation),
                    "terminal_energy_value": float(terminal_value),
                },
            }
        )
    else:
        from .multistage import policy_config

        solver.update(
            {
                "scenario_original_count": prepared.scenario_set.original_count,
                "scenario_reduction_error": prepared.scenario_set.reduction_error,
                "policy_config": policy_config(prepared),
                "tree_nodes": prepared.tree.node_count,
                "move_blocks": len(prepared.blocks),
                "decomposition": f"direct-highs-{decomposition}",
                "risk_model": "service-cvar-then-expected-cost",
                "service_cvar_weight": prepared.service_cvar_weight,
                "service_cvar_alpha": prepared.service_cvar_alpha,
                "economic_cvar_weight": prepared.economic_cvar_weight,
                "economic_cvar_alpha": prepared.economic_cvar_alpha,
            }
        )
    return {
        "schema_version": SCHEMA_VERSION,
        "request_id": str(prepared.payload["request_id"]),
        "ok": True,
        "solver": solver,
        "plan": {
            "mode": prepared.mode,
            "horizon_slots": prepared.n,
            "capacity_wh": total_capacity,
            "initial_soc_pct": initial_total / total_capacity * 100.0,
            "total_cost_ore": raw_total_cost,
            "actions": actions,
        },
    }


def _direct_storage_service_report(
    shared_storage: DirectSharedStorageVars | None,
    solution: np.ndarray,
) -> dict[str, Any]:
    if shared_storage is None:
        return {}
    values: dict[str, float] = {}
    for asset_id, index in shared_storage.shortfalls.items():
        if 0 <= index < len(solution):
            values[str(asset_id)] = float(solution[index])
    named = named_shortfalls(values)
    if not named:
        return {}
    return {"storage_shortfall_wh": named}


def _direct_peaks(
    scenario_vars: list[DirectScenarioVars],
    solution: np.ndarray,
    prepared: "PreparedMultistage",
) -> tuple[float, float]:
    scenarios = prepared.scenario_set.scenarios
    base_index = next(
        (i for i, scenario in enumerate(scenarios) if scenario.id == "base"),
        0,
    )
    base_vars = scenario_vars[base_index]
    grid_import = np.asarray(
        [solution[base_vars.grid_import[t]] for t in range(prepared.n)],
        dtype=float,
    )
    grid_export = np.asarray(
        [solution[base_vars.grid_export[t]] for t in range(prepared.n)],
        dtype=float,
    )
    return peaks_from_grid(grid_import, grid_export)


def _add_highs_row(
    highs: highspy.Highs,
    coefficients: dict[int, float],
    lower: float,
    upper: float,
) -> None:
    columns = np.asarray(sorted(coefficients), dtype=np.int32)
    values = np.asarray([coefficients[int(index)] for index in columns], dtype=np.float64)
    starts = np.asarray([0, len(columns)], dtype=np.int32)
    _require_ok(
        highs.addRows(
            1,
            np.asarray([lower], dtype=np.float64),
            np.asarray([upper], dtype=np.float64),
            len(columns),
            starts,
            columns,
            values,
        ),
        "add preference row",
    )


def _direct_solution_is_integral(model: SparseModel, solution: np.ndarray) -> bool:
    for index in model.integer:
        if index >= len(solution):
            return False
        value = float(solution[index])
        if not math.isfinite(value):
            return False
        if abs(value - round(value)) > INTEGRALITY_TOLERANCE:
            return False
    return True


def _apply_direct_flatten(
    highs: highspy.Highs,
    model: SparseModel,
    prepared: "PreparedMultistage",
    scenario_vars: list[DirectScenarioVars],
    economic_costs: np.ndarray,
    economic_objective: float,
    solution: np.ndarray,
    deadline: SolveDeadline | float,
    *,
    shared: bool,
) -> tuple[str, np.ndarray, float, float]:
    peaks = _direct_peaks(scenario_vars, solution, prepared)
    if not shared or not flatten_peaks_enabled(prepared.settings):
        return STAGE_DISABLED, solution, peaks[0], peaks[1]
    if not flatten_horizon_has_choice(prepared.n):
        return STAGE_SINGLE_SLOT, solution, peaks[0], peaks[1]
    if not isinstance(deadline, SolveDeadline):
        return STAGE_DISABLED, solution, peaks[0], peaks[1]
    try:
        time_limit = preference_time_limit_s(prepared.settings, deadline)
    except SolveCancelled:
        raise
    except SolveDeadlineExceeded:
        return STAGE_NO_TIME, solution, peaks[0], peaks[1]
    if time_limit < MIN_PREFERENCE_TIME_S:
        return STAGE_NO_TIME, solution, peaks[0], peaks[1]

    incumbent = np.array(solution, copy=True)
    n_orig = len(model.lower)
    slack = cost_bound_slack_ore(economic_objective)
    scenarios = prepared.scenario_set.scenarios
    base_index = next(
        (i for i, scenario in enumerate(scenarios) if scenario.id == "base"),
        0,
    )
    base_vars = scenario_vars[base_index]
    try:
        _require_ok(
            highs.addVars(
                2,
                np.asarray([0.0, 0.0], dtype=np.float64),
                np.asarray([highspy.kHighsInf, highspy.kHighsInf], dtype=np.float64),
            ),
            "add preference peak variables",
        )
        import_peak = n_orig
        export_peak = n_orig + 1
        for t in range(prepared.n):
            _add_highs_row(
                highs,
                {base_vars.grid_import[t]: 1.0, import_peak: -1.0},
                -highspy.kHighsInf,
                0.0,
            )
            _add_highs_row(
                highs,
                {base_vars.grid_export[t]: 1.0, export_peak: -1.0},
                -highspy.kHighsInf,
                0.0,
            )
        cost_coeffs = {
            index: float(value)
            for index, value in enumerate(economic_costs[:n_orig])
            if abs(value) > 1e-14
        }
        _add_highs_row(
            highs,
            cost_coeffs,
            -highspy.kHighsInf,
            economic_objective + slack,
        )
        preference_costs = np.zeros(n_orig + 2, dtype=np.float64)
        preference_costs[import_peak] = 1.0
        preference_costs[export_peak] = 1.0
        column_indices = np.arange(n_orig + 2, dtype=np.int32)
        _require_ok(
            highs.changeColsCost(n_orig + 2, column_indices, preference_costs),
            "set preference objective",
        )
        _require_ok(
            highs.setOptionValue("time_limit", time_limit),
            "set preference time limit",
        )
        _run_optimal(highs, "preference", deadline)
        candidate = np.asarray(highs.getSolution().col_value, dtype=np.float64)
        if (
            len(candidate) < n_orig
            or not np.all(np.isfinite(candidate[:n_orig]))
            or (model.integer and not _direct_solution_is_integral(model, candidate))
        ):
            return STAGE_KEPT, incumbent, peaks[0], peaks[1]
        kept = min(n_orig, len(economic_costs), len(candidate))
        spent = float(np.dot(economic_costs[:kept], candidate[:kept]))
        if spent > economic_objective + slack + COST_BOUND_TOLERANCE_ORE:
            return STAGE_KEPT, incumbent, peaks[0], peaks[1]
        new_peaks = _direct_peaks(scenario_vars, candidate, prepared)
        return STAGE_FLAT, candidate, new_peaks[0], new_peaks[1]
    except SolveCancelled:
        raise
    except (SolveDeadlineExceeded, DirectHighsError):
        return STAGE_KEPT, incumbent, peaks[0], peaks[1]


def _add(coefficients: dict[int, float], index: int, value: float) -> None:
    coefficients[index] = coefficients.get(index, 0.0) + value


def _remaining_time_s(deadline: SolveDeadline | float) -> float:
    if isinstance(deadline, SolveDeadline):
        return deadline.remaining_s("direct HiGHS solve")
    remaining = deadline - time.perf_counter()
    if remaining <= 0.0:
        raise SolveDeadlineExceeded("direct HiGHS solve deadline exceeded")
    return remaining


def _validate_shared_baseline_solution(
    prepared: "PreparedMultistage",
    scenario_vars: list[DirectScenarioVars],
    solution: np.ndarray,
) -> None:
    if prepared.mode not in {
        "self_consumption",
        "cheap_charge",
        "passive_arbitrage",
    }:
        return
    tolerance = 1e-4
    for scenario, variables in zip(
        prepared.scenario_set.scenarios, scenario_vars
    ):
        pv_generation = np.maximum(0.0, -scenario.pv)
        for t in range(prepared.n):
            curtailed = float(solution[variables.curtail[t]])
            baseline = float(
                scenario.load[t] - pv_generation[t] + curtailed
            )
            baseline_import = max(baseline, 0.0)
            baseline_export = max(-baseline, 0.0)
            grid_import = float(solution[variables.grid_import[t]])
            grid_export = float(solution[variables.grid_export[t]])
            if prepared.mode == "self_consumption":
                valid = (
                    grid_import <= baseline_import + 50.0 + tolerance
                    and grid_export <= baseline_export + 50.0 + tolerance
                )
            else:
                valid = grid_export <= baseline_export + 1e-6 + tolerance
            if not valid:
                raise DirectHighsError(SHARED_BASELINE_REPLAY_ERROR)


def _accumulate(target: np.ndarray, terms: dict[int, float], weight: float) -> None:
    for index, value in terms.items():
        target[index] += weight * value


def _require_ok(status: highspy.HighsStatus, operation: str) -> None:
    if status != highspy.HighsStatus.kOk:
        raise DirectHighsError(f"HiGHS failed to {operation}: {status}")


def _run_optimal(
    highs: highspy.Highs,
    phase: str,
    deadline: SolveDeadline | float,
) -> None:
    _remaining_time_s(deadline)
    if isinstance(deadline, SolveDeadline):
        if not highs.HandleUserInterrupt:
            highs.HandleUserInterrupt = True
        deadline.attach_highs(highs)
        try:
            try:
                deadline.check(f"direct HiGHS {phase} solve")
                solver_thread = highs.startSolve()
                # startSolve resets HiGHS' stop flag. Repeat a cancellation that
                # arrived after attachment but before the solver thread started.
                if deadline.is_cancelled():
                    highs.cancelSolve()
                run_status = highs.joinSolve(solver_thread)
            except Exception:
                # Cancellation or expiry is the request result even when HiGHS
                # reports its own concurrent start/join error first.
                deadline.check(f"direct HiGHS {phase} solve")
                raise
        finally:
            deadline.detach_highs(highs)
        deadline.check(f"direct HiGHS {phase} solve")
    else:
        run_status = highs.run()
    status = highs.getModelStatus()
    if status in {
        highspy.HighsModelStatus.kInterrupt,
        highspy.HighsModelStatus.kHighsInterrupt,
    }:
        raise SolveCancelled(f"direct HiGHS {phase} solve was cancelled")
    if status == highspy.HighsModelStatus.kTimeLimit:
        raise SolveDeadlineExceeded(
            f"direct HiGHS {phase} solve deadline exceeded"
        )
    if run_status is None:
        raise DirectHighsError(f"HiGHS {phase} solve returned no status")
    _require_ok(run_status, f"run {phase} solve")
    if status != highspy.HighsModelStatus.kOptimal:
        raise DirectHighsError(f"HiGHS {phase} solve failed with status {status}")
    _remaining_time_s(deadline)
