"""Cost-neutral preference stage: flatten grid peaks without spending money.

The economic solve is allowed to pick any of several equally priced
schedules. A second solve then minimises the horizon's grid import and
export peaks, constrained to keep the money the first stage found.
Hard site limits stay hard. A failed or late preference stage keeps the
economic schedule.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Callable

import cvxpy as cp
import numpy as np

from .deadline import SolveCancelled, SolveDeadline, SolveDeadlineExceeded
from .protocol import ProtocolError, require_dict


PREFERENCE_TIME_SHARE = 0.2
MIN_PREFERENCE_TIME_S = 0.05
# Slack is numerical only. 0.1 öre is real money: at 50 öre/kWh it buys
# 2 W of peak reduction, which is enough to nibble a unique export or
# sneak a spread-blocked discharge. Relative term covers reconstructed
# cost on large objectives without becoming a watt budget.
COST_BOUND_SLACK_ORE = 1e-8
COST_BOUND_RELATIVE = 1e-12
COST_BOUND_TOLERANCE_ORE = 1e-6
INTEGRALITY_TOLERANCE = 1e-5
SHORTFALL_WH_REPORT = 1.0
THERMAL_SLACK_REPORT_C = 0.01

STAGE_DISABLED = "disabled"
STAGE_FLAT = "flattened"
STAGE_KEPT = "kept_economic"
STAGE_NO_TIME = "no_time"
STAGE_SINGLE_SLOT = "single_slot"


@dataclass(frozen=True)
class FlattenResult:
    stage: str
    import_peak_w: float
    export_peak_w: float


def flatten_peaks_enabled(settings: dict[str, Any]) -> bool:
    raw = require_dict(settings, "settings").get("flatten_peaks", True)
    if isinstance(raw, bool):
        return raw
    raise ProtocolError("settings.flatten_peaks must be a boolean")


def preference_time_limit_s(
    settings: dict[str, Any],
    deadline: SolveDeadline,
) -> float:
    configured = float(settings.get("time_limit_s", 2.0))
    remaining = deadline.remaining_s("preference flatten")
    return min(max(configured, 0.0) * PREFERENCE_TIME_SHARE, remaining)


def cost_bound_slack_ore(cost_value: float) -> float:
    return max(COST_BOUND_SLACK_ORE, abs(cost_value) * COST_BOUND_RELATIVE)


def flatten_horizon_has_choice(n_slots: int) -> bool:
    """Peak flattening is a tie-break across time. One slot has no tie."""
    return n_slots > 1


def peaks_from_grid(grid_import: np.ndarray, grid_export: np.ndarray) -> tuple[float, float]:
    import_peak = float(np.max(np.maximum(grid_import, 0.0))) if grid_import.size else 0.0
    export_peak = float(np.max(np.maximum(grid_export, 0.0))) if grid_export.size else 0.0
    return import_peak, export_peak


def snapshot_variable_values(problem: cp.Problem) -> dict[int, np.ndarray | None]:
    snapshot: dict[int, np.ndarray | None] = {}
    for variable in problem.variables():
        if variable.value is None:
            snapshot[id(variable)] = None
        else:
            snapshot[id(variable)] = np.array(variable.value, copy=True)
    return snapshot


def restore_variable_values(
    problem: cp.Problem,
    snapshot: dict[int, np.ndarray | None],
) -> None:
    for variable in problem.variables():
        value = snapshot.get(id(variable))
        variable.value = None if value is None else value


def boolean_solution_is_integral(problem: cp.Problem) -> bool:
    for variable in problem.variables():
        attrs = getattr(variable, "attributes", {}) or {}
        if not (attrs.get("boolean") or attrs.get("integer")):
            continue
        if variable.value is None:
            return False
        values = np.asarray(variable.value, dtype=float)
        if np.any(~np.isfinite(values)):
            return False
        if np.any(np.abs(values - np.round(values)) > INTEGRALITY_TOLERANCE):
            return False
    return True


def named_shortfalls(values: dict[str, float]) -> dict[str, float]:
    return {
        str(asset_id): float(value)
        for asset_id, value in values.items()
        if float(value) >= SHORTFALL_WH_REPORT
    }


def build_service_report(
    *,
    flex_loads: list[Any],
    thermal_loads: list[Any],
    storages: list[Any],
) -> dict[str, Any]:
    report: dict[str, Any] = {}
    flex_shortfall: dict[str, float] = {}
    for flex in flex_loads:
        shortfall = getattr(flex, "shortfall", None)
        if shortfall is None or shortfall.value is None:
            continue
        value = float(shortfall.value)
        if value >= SHORTFALL_WH_REPORT:
            flex_shortfall[str(flex.spec["id"])] = value
    if flex_shortfall:
        report["flex_shortfall_wh"] = flex_shortfall

    storage_values: dict[str, float] = {}
    for storage in storages:
        spec = storage.spec if hasattr(storage, "spec") else storage
        shortfall = spec.get("_shortfall") if isinstance(spec, dict) else None
        if shortfall is None or getattr(shortfall, "value", None) is None:
            continue
        storage_values[str(spec["id"])] = float(shortfall.value)
    storage_shortfall = named_shortfalls(storage_values)
    if storage_shortfall:
        report["storage_shortfall_wh"] = storage_shortfall

    thermal_lower: dict[str, float] = {}
    thermal_upper: dict[str, float] = {}
    for thermal in thermal_loads:
        lower = getattr(thermal, "lower_slack", None)
        upper = getattr(thermal, "upper_slack", None)
        asset_id = str(thermal.spec["id"])
        if lower is not None and lower.value is not None:
            value = float(np.max(np.asarray(lower.value, dtype=float)))
            if value >= THERMAL_SLACK_REPORT_C:
                thermal_lower[asset_id] = value
        if upper is not None and upper.value is not None:
            value = float(np.max(np.asarray(upper.value, dtype=float)))
            if value >= THERMAL_SLACK_REPORT_C:
                thermal_upper[asset_id] = value
    if thermal_lower:
        report["thermal_lower_slack_c"] = thermal_lower
    if thermal_upper:
        report["thermal_upper_slack_c"] = thermal_upper
    return report


def apply_cvxpy_flatten(
    *,
    cost_problem: cp.Problem,
    cost_objective: cp.Expression,
    constraints: list[cp.Constraint],
    grid_import: cp.Expression,
    grid_export: cp.Expression,
    settings: dict[str, Any],
    deadline: SolveDeadline,
    discrete: bool,
    solve_problem: Callable[[cp.Problem], None],
) -> FlattenResult:
    """Minimise base-scenario grid peaks without spending economic cost."""

    def current_peaks() -> tuple[float, float]:
        if grid_import.value is None or grid_export.value is None:
            return 0.0, 0.0
        return peaks_from_grid(
            np.asarray(grid_import.value, dtype=float),
            np.asarray(grid_export.value, dtype=float),
        )

    if not flatten_peaks_enabled(settings):
        import_peak, export_peak = current_peaks()
        return FlattenResult(STAGE_DISABLED, import_peak, export_peak)

    snapshot = snapshot_variable_values(cost_problem)
    before = current_peaks()
    n_slots = int(grid_import.shape[0]) if grid_import.shape else 1
    if not flatten_horizon_has_choice(n_slots):
        return FlattenResult(STAGE_SINGLE_SLOT, *before)
    cost_value = float(cost_objective.value)
    try:
        time_limit = preference_time_limit_s(settings, deadline)
    except SolveCancelled:
        raise
    except SolveDeadlineExceeded:
        return FlattenResult(STAGE_NO_TIME, *before)
    if time_limit < MIN_PREFERENCE_TIME_S:
        return FlattenResult(STAGE_NO_TIME, *before)

    import_peak = cp.Variable(nonneg=True, name="preference_import_peak_w")
    export_peak = cp.Variable(nonneg=True, name="preference_export_peak_w")
    slack = cost_bound_slack_ore(cost_value)
    preference = cp.Problem(
        cp.Minimize(import_peak + export_peak),
        constraints
        + [
            import_peak >= grid_import,
            export_peak >= grid_export,
            cost_objective <= cost_value + slack,
        ],
    )
    previous_limit = settings.get("time_limit_s")
    settings["time_limit_s"] = time_limit
    try:
        try:
            solve_problem(preference)
        except SolveCancelled:
            restore_variable_values(cost_problem, snapshot)
            raise
        except (SolveDeadlineExceeded, cp.error.SolverError):
            restore_variable_values(cost_problem, snapshot)
            return FlattenResult(STAGE_KEPT, *before)
        except Exception:
            restore_variable_values(cost_problem, snapshot)
            return FlattenResult(STAGE_KEPT, *before)
    finally:
        if previous_limit is None:
            settings.pop("time_limit_s", None)
        else:
            settings["time_limit_s"] = previous_limit

    if preference.status not in {cp.OPTIMAL, cp.OPTIMAL_INACCURATE}:
        restore_variable_values(cost_problem, snapshot)
        return FlattenResult(STAGE_KEPT, *before)
    if discrete and not boolean_solution_is_integral(preference):
        restore_variable_values(cost_problem, snapshot)
        return FlattenResult(STAGE_KEPT, *before)
    if cost_objective.value is None or float(cost_objective.value) > (
        cost_value + slack + COST_BOUND_TOLERANCE_ORE
    ):
        restore_variable_values(cost_problem, snapshot)
        return FlattenResult(STAGE_KEPT, *before)
    after = current_peaks()
    return FlattenResult(STAGE_FLAT, *after)
