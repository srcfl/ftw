from __future__ import annotations

from dataclasses import dataclass
from typing import Any

import numpy as np

from .protocol import ProtocolError, finite_number, positive_number, require_dict, require_list


@dataclass(frozen=True)
class Scenario:
    id: str
    probability: float
    load: np.ndarray
    pv: np.ndarray

    @property
    def net(self) -> np.ndarray:
        return self.load + self.pv


@dataclass(frozen=True)
class ScenarioSet:
    scenarios: tuple[Scenario, ...]
    original_count: int
    reduction_error: float


@dataclass(frozen=True)
class ScenarioTree:
    node_at: np.ndarray
    branch_slots: tuple[int, ...]
    node_count: int


def _vector(value: Any, n: int, field: str) -> np.ndarray:
    items = require_list(value, field)
    if len(items) != n:
        raise ProtocolError(f"{field} must have {n} entries")
    return np.asarray([finite_number(item, f"{field}[{i}]") for i, item in enumerate(items)])


def parse_scenarios(
    payload: dict[str, Any],
    n: int,
    base_load: np.ndarray,
    base_pv: np.ndarray,
) -> list[Scenario]:
    raw_scenarios = require_list(payload.get("scenarios", []), "scenarios")
    scenarios: list[Scenario] = []
    if raw_scenarios:
        for i, raw in enumerate(raw_scenarios):
            spec = require_dict(raw, f"scenarios[{i}]")
            scenario = Scenario(
                id=str(spec.get("id", f"scenario-{i}")),
                probability=positive_number(
                    spec.get("probability", 0), f"scenarios[{i}].probability"
                ),
                load=_vector(spec.get("load_w"), n, f"scenarios[{i}].load_w"),
                pv=_vector(spec.get("pv_w"), n, f"scenarios[{i}].pv_w"),
            )
            scenarios.append(scenario)
    else:
        scenarios.append(Scenario("base", 1.0, base_load.copy(), base_pv.copy()))

    ids = [scenario.id for scenario in scenarios]
    if len(set(ids)) != len(ids):
        raise ProtocolError("scenario ids must be unique")
    probability_sum = sum(scenario.probability for scenario in scenarios)
    normalized: list[Scenario] = []
    for scenario in scenarios:
        if np.any(scenario.load < -1e-9) or np.any(scenario.pv > 1e-9):
            raise ProtocolError(f"scenario {scenario.id} violates site sign convention")
        normalized.append(
            Scenario(
                scenario.id,
                scenario.probability / probability_sum,
                scenario.load,
                scenario.pv,
            )
        )
    return normalized


def reduce_scenarios(
    scenarios: list[Scenario], limit: int, dt_h: np.ndarray
) -> ScenarioSet:
    """Reduce trajectories with deterministic forward Kantorovich selection.

    The distance combines pointwise net power and cumulative net energy. The
    base path is always retained so the returned diagnostic plan has a stable
    reference trajectory. Probability mass from discarded paths is assigned
    to the nearest retained medoid.
    """

    if limit < 1:
        raise ProtocolError("settings.scenario_limit must be positive")
    original_count = len(scenarios)
    if original_count <= limit:
        return ScenarioSet(tuple(scenarios), original_count, 0.0)

    features = _trajectory_features(scenarios, dt_h)
    distances = _pairwise_distances(features)
    probabilities = np.asarray([scenario.probability for scenario in scenarios])
    base_index = next((i for i, scenario in enumerate(scenarios) if scenario.id == "base"), None)
    first = base_index if base_index is not None else int(np.argmax(probabilities))
    selected = [first]
    nearest = distances[:, first].copy()
    while len(selected) < limit:
        best_index = -1
        best_objective = float("inf")
        for candidate in range(original_count):
            if candidate in selected:
                continue
            objective = float(probabilities @ np.minimum(nearest, distances[:, candidate]))
            if objective < best_objective - 1e-12 or (
                abs(objective - best_objective) <= 1e-12
                and (best_index < 0 or scenarios[candidate].id < scenarios[best_index].id)
            ):
                best_index = candidate
                best_objective = objective
        selected.append(best_index)
        nearest = np.minimum(nearest, distances[:, best_index])

    selected.sort(key=lambda index: (scenarios[index].id != "base", scenarios[index].id))
    selected_distances = distances[:, selected]
    assignment = np.argmin(selected_distances, axis=1)
    reduced_probabilities = np.zeros(len(selected))
    for original_index, reduced_index in enumerate(assignment):
        reduced_probabilities[reduced_index] += probabilities[original_index]

    reduced = tuple(
        Scenario(
            scenarios[original_index].id,
            float(reduced_probabilities[reduced_index]),
            scenarios[original_index].load,
            scenarios[original_index].pv,
        )
        for reduced_index, original_index in enumerate(selected)
    )
    error = float(probabilities @ np.min(selected_distances, axis=1))
    return ScenarioSet(reduced, original_count, error)


def build_scenario_tree(
    scenarios: tuple[Scenario, ...],
    n: int,
    first_stage_slots: int,
    branch_interval_slots: int,
    branch_horizon_slots: int,
    max_branching: int,
) -> ScenarioTree:
    if not 1 <= first_stage_slots <= n:
        raise ProtocolError("settings.non_anticipative_slots must be in [1, len(slots)]")
    if branch_interval_slots < 1:
        raise ProtocolError("settings.branch_interval_slots must be positive")
    if branch_horizon_slots < first_stage_slots:
        raise ProtocolError("settings.branch_horizon_slots must cover the first stage")
    if max_branching < 2:
        raise ProtocolError("settings.max_branching must be at least 2")

    m = len(scenarios)
    branch_slots = tuple(
        range(first_stage_slots, min(n, branch_horizon_slots), branch_interval_slots)
    )
    branch_set = set(branch_slots)
    node_at = np.zeros((m, n), dtype=np.int64)
    groups: list[tuple[int, list[int]]] = [(0, list(range(m)))]
    next_node = 1
    for t in range(n):
        if t in branch_set:
            refined: list[tuple[int, list[int]]] = []
            for parent_node, group in groups:
                children = _split_group(
                    group, scenarios, observed_slots=t, max_branching=max_branching
                )
                if len(children) == 1:
                    refined.append((parent_node, children[0]))
                    continue
                for child in children:
                    refined.append((next_node, child))
                    next_node += 1
            groups = refined
        for node, group in groups:
            for scenario_index in group:
                node_at[scenario_index, t] = node

    return ScenarioTree(
        node_at=node_at,
        branch_slots=branch_slots,
        node_count=next_node,
    )


def decision_blocks(
    n: int,
    near_horizon_slots: int,
    mid_horizon_slots: int,
    mid_block_slots: int,
    far_block_slots: int,
    branch_slots: tuple[int, ...],
) -> tuple[tuple[int, int], ...]:
    if near_horizon_slots < 1:
        raise ProtocolError("settings.near_horizon_slots must be positive")
    if mid_horizon_slots < near_horizon_slots:
        raise ProtocolError("settings.mid_horizon_slots must cover the near horizon")
    if mid_block_slots < 1 or far_block_slots < 1:
        raise ProtocolError("move-block sizes must be positive")

    hard_boundaries = {0, n, *[slot for slot in branch_slots if 0 < slot < n]}
    blocks: list[tuple[int, int]] = []
    start = 0
    while start < n:
        if start < near_horizon_slots:
            width = 1
        elif start < mid_horizon_slots:
            width = mid_block_slots
        else:
            width = far_block_slots
        end = min(n, start + width)
        crossing = [boundary for boundary in hard_boundaries if start < boundary < end]
        if crossing:
            end = min(crossing)
        blocks.append((start, end))
        start = end
    return tuple(blocks)


def _trajectory_features(scenarios: list[Scenario], dt_h: np.ndarray) -> np.ndarray:
    net = np.stack([scenario.net for scenario in scenarios])
    pv_generation = np.stack([-scenario.pv for scenario in scenarios])
    power_scale = max(1.0, float(np.quantile(np.abs(net), 0.9)))
    pv_scale = max(1.0, float(np.quantile(np.abs(pv_generation), 0.9)))
    cumulative = np.cumsum(net * dt_h, axis=1)
    energy_scale = max(1.0, float(np.quantile(np.abs(cumulative), 0.9)))
    return np.concatenate(
        (net / power_scale, pv_generation / pv_scale, cumulative / energy_scale),
        axis=1,
    )


def _pairwise_distances(features: np.ndarray) -> np.ndarray:
    delta = features[:, None, :] - features[None, :, :]
    return np.sqrt(np.mean(delta * delta, axis=2))


def _split_group(
    group: list[int],
    scenarios: tuple[Scenario, ...],
    observed_slots: int,
    max_branching: int,
) -> list[list[int]]:
    if len(group) <= 1 or observed_slots <= 0:
        return [group]
    net = np.stack([scenarios[index].net[:observed_slots] for index in group])
    pv_generation = np.stack(
        [-scenarios[index].pv[:observed_slots] for index in group]
    )
    net_scale = max(1.0, float(np.quantile(np.abs(net), 0.9)))
    pv_scale = max(1.0, float(np.quantile(np.abs(pv_generation), 0.9)))
    history = np.concatenate((net / net_scale, pv_generation / pv_scale), axis=1)
    distances = _pairwise_distances(history)
    if float(np.max(distances)) <= 1e-9:
        return [group]

    probabilities = np.asarray([scenarios[index].probability for index in group])
    center_count = min(max_branching, len(group))
    centers = [int(np.argmax(probabilities))]
    nearest = distances[:, centers[0]].copy()
    while len(centers) < center_count:
        candidate = int(np.argmax(nearest * np.maximum(probabilities, 1e-12)))
        if candidate in centers or nearest[candidate] <= 1e-9:
            break
        centers.append(candidate)
        nearest = np.minimum(nearest, distances[:, candidate])

    assignment = np.argmin(distances[:, centers], axis=1)
    clusters: list[list[int]] = []
    for cluster_index in range(len(centers)):
        members = [group[i] for i in range(len(group)) if assignment[i] == cluster_index]
        if members:
            clusters.append(sorted(members))
    clusters.sort(key=lambda members: members[0])
    return clusters
