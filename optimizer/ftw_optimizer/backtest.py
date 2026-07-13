from __future__ import annotations

import argparse
import csv
import json
import math
import sys
import time
import urllib.parse
import urllib.request
import uuid
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any, Iterable

from .worker import handle


DATASET_SCHEMA_VERSION = 1


class SnapshotSkip(ValueError):
    pass


def _get_json(url: str, timeout_s: float) -> dict[str, Any]:
    request = urllib.request.Request(
        url,
        headers={"Accept": "application/json", "User-Agent": "forty-two-watts-optimizer-backtest/1"},
        method="GET",
    )
    with urllib.request.urlopen(request, timeout=timeout_s) as response:
        payload = json.load(response)
    if not isinstance(payload, dict):
        raise RuntimeError(f"{url} returned a non-object JSON payload")
    return payload


def _evenly_spaced(items: list[dict[str, Any]], count: int) -> list[dict[str, Any]]:
    if count <= 0 or not items:
        return []
    if count >= len(items):
        return list(items)
    if count == 1:
        return [items[len(items) // 2]]
    indexes = {round(i * (len(items) - 1) / (count - 1)) for i in range(count)}
    return [items[i] for i in sorted(indexes)]


def select_summaries(
    summaries: Iterable[dict[str, Any]], sample_count: int, per_reason: int = 3
) -> list[dict[str, Any]]:
    ordered = sorted(summaries, key=lambda row: int(row["ts_ms"]))
    if sample_count <= 0 or sample_count >= len(ordered):
        return ordered

    by_reason: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for row in ordered:
        by_reason[str(row.get("reason", "unknown"))].append(row)

    selected: dict[int, dict[str, Any]] = {}
    for group in by_reason.values():
        for row in _evenly_spaced(group, min(per_reason, len(group))):
            selected[int(row["ts_ms"])] = row

    remaining = sample_count - len(selected)
    if remaining > 0:
        candidates = [row for row in ordered if int(row["ts_ms"]) not in selected]
        for row in _evenly_spaced(candidates, remaining):
            selected[int(row["ts_ms"])] = row

    if len(selected) < sample_count:
        for row in ordered:
            selected.setdefault(int(row["ts_ms"]), row)
            if len(selected) >= sample_count:
                break
    return sorted(selected.values(), key=lambda row: int(row["ts_ms"]))[:sample_count]


def export_dataset(
    api_base: str,
    output: Path,
    days: int,
    samples: int,
    timeout_s: float,
) -> dict[str, Any]:
    base = api_base.rstrip("/")
    until_ms = int(time.time() * 1000)
    since_ms = until_ms - days * 24 * 60 * 60 * 1000
    cursor = until_ms
    summaries_by_ts: dict[int, dict[str, Any]] = {}

    while cursor >= since_ms:
        query = urllib.parse.urlencode(
            {"since": since_ms, "until": cursor, "limit": 5000}
        )
        payload = _get_json(f"{base}/api/mpc/diagnose/history?{query}", timeout_s)
        page = payload.get("snapshots", [])
        if not isinstance(page, list):
            raise RuntimeError("diagnostic history response has no snapshots array")
        for row in page:
            if isinstance(row, dict) and int(row.get("ts_ms", 0)) > 0:
                summaries_by_ts[int(row["ts_ms"])] = row
        if len(page) < 5000:
            break
        oldest = min(int(row["ts_ms"]) for row in page if isinstance(row, dict))
        if oldest <= since_ms or oldest >= cursor:
            break
        cursor = oldest - 1

    selected = select_summaries(summaries_by_ts.values(), samples)
    metadata = {
        "type": "metadata",
        "schema_version": DATASET_SCHEMA_VERSION,
        "exported_at_ms": int(time.time() * 1000),
        "source": base,
        "since_ms": since_ms,
        "until_ms": until_ms,
        "index_count": len(summaries_by_ts),
        "sample_count": len(selected),
    }

    output.parent.mkdir(parents=True, exist_ok=True)
    temporary = output.with_suffix(output.suffix + ".tmp")
    with temporary.open("w", encoding="utf-8") as target:
        target.write(json.dumps(metadata, separators=(",", ":")) + "\n")
        for position, summary in enumerate(selected, start=1):
            query = urllib.parse.urlencode({"ts": int(summary["ts_ms"])})
            payload = _get_json(f"{base}/api/mpc/diagnose/at?{query}", timeout_s)
            snapshot = payload.get("snapshot")
            if not isinstance(snapshot, dict) or not isinstance(snapshot.get("diagnostic"), dict):
                continue
            record = {
                "type": "snapshot",
                "summary": summary,
                "diagnostic": snapshot["diagnostic"],
            }
            target.write(json.dumps(record, separators=(",", ":"), allow_nan=False) + "\n")
            print(f"exported {position}/{len(selected)}", file=sys.stderr)
    temporary.replace(output)
    return metadata


def load_dataset(path: Path) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    metadata: dict[str, Any] | None = None
    snapshots: list[dict[str, Any]] = []
    with path.open("r", encoding="utf-8") as source:
        for line_number, line in enumerate(source, start=1):
            if not line.strip():
                continue
            record = json.loads(line)
            if not isinstance(record, dict):
                raise ValueError(f"dataset line {line_number} is not an object")
            if record.get("type") == "metadata":
                metadata = record
            elif record.get("type") == "snapshot":
                snapshots.append(record)
    if metadata is None or metadata.get("schema_version") != DATASET_SCHEMA_VERSION:
        raise ValueError("unsupported or missing backtest dataset metadata")
    return metadata, snapshots


def request_from_diagnostic(
    diagnostic: dict[str, Any],
    *,
    solver: str,
    formulation: str,
    time_limit_s: float,
    max_import_w: float,
    max_export_w: float,
    min_arbitrage_spread_ore_kwh: float,
) -> dict[str, Any]:
    if diagnostic.get("loadpoint_id"):
        raise SnapshotSkip("historical loadpoint contract is not persisted")
    params = diagnostic.get("params")
    slots = diagnostic.get("slots")
    if not isinstance(params, dict) or not isinstance(slots, list) or not slots:
        raise SnapshotSkip("diagnostic has no reconstructable params/slots")

    capacity_wh = float(params.get("capacity_wh", 0))
    if not math.isfinite(capacity_wh) or capacity_wh <= 0:
        raise SnapshotSkip("diagnostic has no battery capacity")
    initial_soc_pct = float(params.get("initial_soc_pct", 0))
    min_soc_pct = float(params.get("soc_min_pct", 0))
    max_soc_pct = float(params.get("soc_max_pct", 100))

    request_slots = []
    for slot in slots:
        if not isinstance(slot, dict):
            raise SnapshotSkip("diagnostic contains an invalid slot")
        request_slots.append(
            {
                "start_ms": int(slot["slot_start_ms"]),
                "len_min": int(slot["len_min"]),
                "price_ore": float(slot["price_ore"]),
                "spot_ore": float(slot.get("spot_ore", slot["price_ore"])),
                "confidence": float(slot.get("confidence", 1)),
                "pv_w": float(slot.get("pv_w", 0)),
                "load_w": float(slot.get("load_w", 0)),
                "max_import_w": max_import_w,
                "max_export_w": max_export_w,
            }
        )

    return {
        "schema_version": 1,
        "request_id": f"backtest-{uuid.uuid4()}",
        "settings": {
            "mode": str(params.get("mode", "self_consumption")),
            "solver": solver,
            "formulation": formulation,
            "time_limit_s": time_limit_s,
            "mip_rel_gap": 0.005,
            "export_bonus_ore_kwh": float(params.get("export_bonus_ore_kwh", 0)),
            "export_fee_ore_kwh": float(params.get("export_fee_ore_kwh", 0)),
            "export_floor_ore_kwh": params.get("export_floor_ore_kwh"),
            "min_arbitrage_spread_ore_kwh": min_arbitrage_spread_ore_kwh,
            "pv_charge_bonus_ore_kwh": float(params.get("pv_charge_bonus_ore_kwh", 0)),
            "cvar_weight": 0,
            "cvar_alpha": 0.9,
        },
        "slots": request_slots,
        "storages": [
            {
                "id": "historical-fleet",
                "capacity_wh": capacity_wh,
                "initial_energy_wh": capacity_wh * initial_soc_pct / 100,
                "min_energy_wh": capacity_wh * min_soc_pct / 100,
                "max_energy_wh": capacity_wh * max_soc_pct / 100,
                "max_charge_w": float(params.get("max_charge_w", 0)),
                "max_discharge_w": float(params.get("max_discharge_w", 0)),
                "charge_efficiency": float(params.get("charge_efficiency", 0.95)),
                "discharge_efficiency": float(params.get("discharge_efficiency", 0.95)),
                "terminal_price_ore_kwh": float(params.get("terminal_soc_price_ore_kwh", 0)),
                "cycle_cost_ore_kwh": 0,
            }
        ],
        "flex_loads": [],
        "thermal_loads": [],
    }


def _percentile(values: list[float], fraction: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    position = fraction * (len(ordered) - 1)
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return ordered[lower]
    return ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower)


def load_realized_csv(path: Path | None) -> dict[int, dict[str, float]]:
    if path is None:
        return {}
    rows: dict[int, dict[str, float]] = {}
    with path.open("r", encoding="utf-8", newline="") as source:
        for raw in csv.DictReader(source):
            try:
                start_ms = int(raw["bucket_start_ms"])
                rows[start_ms] = {
                    key: float(raw[key]) if raw.get(key, "") != "" else math.nan
                    for key in (
                        "bucket_end_ms",
                        "pv_w",
                        "ev_w",
                        "v2x_w",
                        "house_load_w",
                        "total_ore_kwh",
                        "spot_ore_kwh",
                    )
                }
            except (KeyError, TypeError, ValueError):
                continue
    return rows


def _realized_cost(
    grid_w: float,
    dt_h: float,
    import_ore_kwh: float,
    export_ore_kwh: float,
) -> float:
    grid_kwh = grid_w * dt_h / 1000.0
    return import_ore_kwh * max(grid_kwh, 0.0) - export_ore_kwh * max(-grid_kwh, 0.0)


def dp_evaluation_reference(
    diagnostic: dict[str, Any],
) -> tuple[float, dict[str, Any]]:
    shadow = diagnostic.get("dp_evaluation_shadow")
    if isinstance(shadow, dict) and isinstance(shadow.get("first_action"), dict):
        return float(shadow.get("total_cost_ore", 0)), shadow["first_action"]

    solver = diagnostic.get("solver")
    engine = str(solver.get("engine", "")) if isinstance(solver, dict) else ""
    slots = diagnostic.get("slots", [])
    if engine in {"", "go-dp"} and not diagnostic.get("optimizer_input") and slots:
        return float(diagnostic.get("total_cost_ore", 0)), slots[0]
    raise SnapshotSkip("missing same-input DP evaluation shadow")


def realized_first_slot(
    diagnostic: dict[str, Any],
    response: dict[str, Any],
    realized: dict[int, dict[str, float]],
    max_import_w: float,
    max_export_w: float,
    old_action: dict[str, Any] | None = None,
) -> dict[str, Any] | None:
    slots = diagnostic.get("slots", [])
    actions = response.get("plan", {}).get("actions", [])
    if not slots or not actions:
        return None
    old_slot = slots[0]
    new = actions[0]
    if old_action is None:
        try:
            _, old_action = dp_evaluation_reference(diagnostic)
        except SnapshotSkip:
            return None
    start_ms = int(old_slot.get("slot_start_ms", 0))
    actual = realized.get(start_ms)
    if actual is None:
        return None
    required = (
        actual["bucket_end_ms"],
        actual["pv_w"],
        actual["ev_w"],
        actual["v2x_w"],
        actual["house_load_w"],
        actual["total_ore_kwh"],
        actual["spot_ore_kwh"],
    )
    if not all(math.isfinite(value) for value in required):
        return None

    params = diagnostic.get("params", {})
    export_ore = actual["spot_ore_kwh"]
    export_ore += float(params.get("export_bonus_ore_kwh", 0))
    export_ore -= float(params.get("export_fee_ore_kwh", 0))
    floor = params.get("export_floor_ore_kwh")
    if floor is not None:
        export_ore = max(export_ore, float(floor))
    base_w = actual["house_load_w"] + actual["ev_w"] + actual["v2x_w"] + actual["pv_w"]
    old_grid_w = base_w + float(old_action.get("battery_w", 0))
    new_grid_w = base_w + float(new.get("battery_w", 0))
    dt_h = (actual["bucket_end_ms"] - start_ms) / 3_600_000.0
    mode = str(params.get("mode", "self_consumption"))
    min_grid_w = min(0.0, base_w)
    if mode == "self_consumption":
        mode_violation = not (
            min(base_w, 0.0) - 50 <= new_grid_w <= max(base_w, 0.0) + 50
        )
    elif mode in {"cheap_charge", "passive_arbitrage"}:
        mode_violation = new_grid_w < min_grid_w - 50
    else:
        mode_violation = False
    limit_violation = (
        (max_import_w > 0 and new_grid_w > max_import_w + 2)
        or (max_export_w > 0 and new_grid_w < -max_export_w - 2)
    )
    old_cost = _realized_cost(old_grid_w, dt_h, actual["total_ore_kwh"], export_ore)
    new_cost = _realized_cost(new_grid_w, dt_h, actual["total_ore_kwh"], export_ore)
    return {
        "bucket_start_ms": start_ms,
        "actual_base_w": base_w,
        "forecast_base_w": float(old_slot.get("load_w", 0)) + float(old_slot.get("pv_w", 0)),
        "old_battery_w": float(old_action.get("battery_w", 0)),
        "new_battery_w": float(new.get("battery_w", 0)),
        "old_grid_w": old_grid_w,
        "new_grid_w": new_grid_w,
        "old_cost_ore": old_cost,
        "new_cost_ore": new_cost,
        "delta_ore": new_cost - old_cost,
        "mode_violation": mode_violation,
        "limit_violation": limit_violation,
    }


def run_backtest(
    dataset: Path,
    output: Path,
    *,
    solver: str,
    formulation: str,
    time_limit_s: float,
    max_import_w: float,
    max_export_w: float,
    min_arbitrage_spread_ore_kwh: float,
    limit: int,
    realized_csv: Path | None,
) -> dict[str, Any]:
    metadata, snapshots = load_dataset(dataset)
    realized = load_realized_csv(realized_csv)
    if limit > 0:
        snapshots = snapshots[:limit]
    results: list[dict[str, Any]] = []
    failures: Counter[str] = Counter()
    skips: Counter[str] = Counter()
    solve_times: list[float] = []
    deltas: list[float] = []
    old_costs: list[float] = []
    new_costs: list[float] = []
    out_of_bounds_starts = 0
    realized_candidates: list[dict[str, Any]] = []

    for position, record in enumerate(snapshots, start=1):
        diagnostic = record.get("diagnostic", {})
        summary = record.get("summary", {})
        params = diagnostic.get("params", {}) if isinstance(diagnostic, dict) else {}
        initial = float(params.get("initial_soc_pct", 0)) if isinstance(params, dict) else 0
        minimum = float(params.get("soc_min_pct", 0)) if isinstance(params, dict) else 0
        maximum = float(params.get("soc_max_pct", 100)) if isinstance(params, dict) else 100
        if initial < minimum - 1e-9 or initial > maximum + 1e-9:
            out_of_bounds_starts += 1
        try:
            request = request_from_diagnostic(
                diagnostic,
                solver=solver,
                formulation=formulation,
                time_limit_s=time_limit_s,
                max_import_w=max_import_w,
                max_export_w=max_export_w,
                min_arbitrage_spread_ore_kwh=min_arbitrage_spread_ore_kwh,
            )
            old_cost, old_action = dp_evaluation_reference(diagnostic)
        except SnapshotSkip as exc:
            skips[str(exc)] += 1
            continue

        response = handle(request)
        row = {
            "ts_ms": int(summary.get("ts_ms", diagnostic.get("computed_at_ms", 0))),
            "reason": str(summary.get("reason", diagnostic.get("last_reason", "unknown"))),
            "initial_soc_pct": initial,
        }
        if not response.get("ok"):
            error = response.get("error", {})
            message = f"{error.get('code', 'unknown')}: {error.get('message', 'unknown')}"
            failures[message] += 1
            row.update({"ok": False, "error": message})
            results.append(row)
            print(f"replayed {position}/{len(snapshots)} failed: {message}", file=sys.stderr)
            continue

        new_cost = float(response["plan"]["total_cost_ore"])
        solve_ms = float(response["solver"]["solve_ms"])
        delta = new_cost - old_cost
        old_costs.append(old_cost)
        new_costs.append(new_cost)
        deltas.append(delta)
        solve_times.append(solve_ms)
        row.update(
            {
                "ok": True,
                "old_dp_cost_ore": old_cost,
                "new_optimizer_cost_ore": new_cost,
                "delta_ore": delta,
                "solve_ms": solve_ms,
                "status": response["solver"]["status"],
                "formulation": response["solver"]["formulation"],
                "service_slack": response["solver"]["service_slack"],
            }
        )
        actual = realized_first_slot(
            diagnostic, response, realized, max_import_w, max_export_w, old_action
        )
        if actual is not None:
            row["realized_first_slot"] = actual
            realized_candidates.append({"ts_ms": row["ts_ms"], **actual})
        results.append(row)
        print(f"replayed {position}/{len(snapshots)}", file=sys.stderr)

    realized_by_bucket: dict[int, dict[str, Any]] = {}
    for candidate in realized_candidates:
        bucket = int(candidate["bucket_start_ms"])
        previous = realized_by_bucket.get(bucket)
        offset = abs(int(candidate["ts_ms"]) - bucket)
        previous_offset = abs(int(previous["ts_ms"]) - bucket) if previous else math.inf
        if previous is None or offset < previous_offset:
            realized_by_bucket[bucket] = candidate
    realized_unique = list(realized_by_bucket.values())
    realized_deltas = [float(row["delta_ore"]) for row in realized_unique]

    report = {
        "schema_version": 1,
        "generated_at_ms": int(time.time() * 1000),
        "dataset": metadata,
        "configuration": {
            "solver": solver,
            "formulation": formulation,
            "time_limit_s": time_limit_s,
            "max_import_w": max_import_w,
            "max_export_w": max_export_w,
            "min_arbitrage_spread_ore_kwh": min_arbitrage_spread_ore_kwh,
            "historical_scenarios": False,
        },
        "summary": {
            "snapshots": len(snapshots),
            "solved": len(solve_times),
            "failed": sum(failures.values()),
            "skipped": sum(skips.values()),
            "out_of_bounds_starts": out_of_bounds_starts,
            "solve_ms": {
                "p50": _percentile(solve_times, 0.50),
                "p95": _percentile(solve_times, 0.95),
                "p99": _percentile(solve_times, 0.99),
                "max": max(solve_times) if solve_times else None,
            },
            "planned_cost_ore": {
                "old_dp_sum": sum(old_costs),
                "new_optimizer_sum": sum(new_costs),
                "delta_sum": sum(deltas),
                "delta_p50": _percentile(deltas, 0.50),
                "delta_p95": _percentile(deltas, 0.95),
            },
            "realized_first_slot": {
                "unique_slots": len(realized_unique),
                "old_dp_cost_ore": sum(float(row["old_cost_ore"]) for row in realized_unique),
                "new_optimizer_cost_ore": sum(float(row["new_cost_ore"]) for row in realized_unique),
                "delta_ore": sum(realized_deltas),
                "delta_p50": _percentile(realized_deltas, 0.50),
                "delta_p95": _percentile(realized_deltas, 0.95),
                "mode_violations": sum(bool(row["mode_violation"]) for row in realized_unique),
                "limit_violations": sum(bool(row["limit_violation"]) for row in realized_unique),
            },
            "failures": dict(failures.most_common()),
            "skips": dict(skips.most_common()),
        },
        "limitations": [
            "Historical diagnostics preserve forecast snapshots, not realized outcomes.",
            "The legacy diagnostic schema does not preserve full loadpoint contracts; those snapshots are skipped.",
            "Historical PV/load scenario distributions are unavailable, so replay uses the persisted base/downside slots without CVaR.",
            "Realized first-slot results are one-step counterfactuals; live dispatch feedback would adjust planned battery power.",
        ],
        "results": results,
    }
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(report, indent=2, allow_nan=False) + "\n", encoding="utf-8")
    return report


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Export and replay historical MPC diagnostics")
    subparsers = parser.add_subparsers(dest="command", required=True)

    export = subparsers.add_parser("export", help="export a read-only diagnostic sample")
    export.add_argument("--api-base", required=True)
    export.add_argument("--output", type=Path, required=True)
    export.add_argument("--days", type=int, default=30)
    export.add_argument("--samples", type=int, default=200)
    export.add_argument("--timeout-s", type=float, default=30)

    run = subparsers.add_parser("run", help="solve a previously exported dataset offline")
    run.add_argument("--input", type=Path, required=True)
    run.add_argument("--output", type=Path, required=True)
    run.add_argument("--solver", choices=["HIGHS", "CLARABEL"], default="HIGHS")
    run.add_argument("--formulation", choices=["auto", "milp", "relaxed"], default="auto")
    run.add_argument("--time-limit-s", type=float, default=5)
    run.add_argument("--max-import-w", type=float, default=0)
    run.add_argument("--max-export-w", type=float, default=0)
    run.add_argument("--min-arbitrage-spread-ore-kwh", type=float, default=0)
    run.add_argument("--limit", type=int, default=0)
    run.add_argument("--realized-csv", type=Path)
    return parser


def main() -> None:
    args = _parser().parse_args()
    if args.command == "export":
        if args.days <= 0 or args.samples <= 0:
            raise SystemExit("--days and --samples must be positive")
        summary = export_dataset(args.api_base, args.output, args.days, args.samples, args.timeout_s)
    else:
        summary = run_backtest(
            args.input,
            args.output,
            solver=args.solver,
            formulation=args.formulation,
            time_limit_s=args.time_limit_s,
            max_import_w=max(0, args.max_import_w),
            max_export_w=max(0, args.max_export_w),
            min_arbitrage_spread_ore_kwh=max(0, args.min_arbitrage_spread_ore_kwh),
            limit=max(0, args.limit),
            realized_csv=args.realized_csv,
        )["summary"]
    json.dump(summary, sys.stdout, indent=2, allow_nan=False)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
