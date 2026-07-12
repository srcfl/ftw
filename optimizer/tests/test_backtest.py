from __future__ import annotations

import pytest

from ftw_optimizer.backtest import (
    SnapshotSkip,
    realized_first_slot,
    request_from_diagnostic,
    select_summaries,
)


def diagnostic() -> dict:
    return {
        "computed_at_ms": 1000,
        "total_cost_ore": 12.5,
        "params": {
            "mode": "passive_arbitrage",
            "initial_soc_pct": 50,
            "soc_min_pct": 10,
            "soc_max_pct": 95,
            "capacity_wh": 10000,
            "max_charge_w": 5000,
            "max_discharge_w": 5000,
            "charge_efficiency": 0.95,
            "discharge_efficiency": 0.95,
            "terminal_soc_price_ore_kwh": 100,
        },
        "slots": [
            {
                "slot_start_ms": 1000,
                "len_min": 15,
                "price_ore": 100,
                "spot_ore": 50,
                "confidence": 1,
                "pv_w": -500,
                "load_w": 1000,
            }
        ],
    }


def test_select_summaries_preserves_rare_reasons() -> None:
    rows = [{"ts_ms": i, "reason": "scheduled"} for i in range(1, 101)]
    rows.append({"ts_ms": 101, "reason": "missing_plan_retry"})
    selected = select_summaries(rows, 10)
    assert len(selected) == 10
    assert any(row["reason"] == "missing_plan_retry" for row in selected)


def test_request_from_diagnostic_reconstructs_storage_and_limits() -> None:
    request = request_from_diagnostic(
        diagnostic(),
        solver="HIGHS",
        formulation="auto",
        time_limit_s=5,
        max_import_w=11040,
        max_export_w=11040,
        min_arbitrage_spread_ore_kwh=30,
    )
    assert request["storages"][0]["initial_energy_wh"] == 5000
    assert request["slots"][0]["max_import_w"] == 11040
    assert request["settings"]["min_arbitrage_spread_ore_kwh"] == 30


def test_request_from_diagnostic_skips_legacy_loadpoint_without_contract() -> None:
    value = diagnostic()
    value["loadpoint_id"] = "easee"
    with pytest.raises(SnapshotSkip, match="loadpoint contract"):
        request_from_diagnostic(
            value,
            solver="HIGHS",
            formulation="auto",
            time_limit_s=5,
            max_import_w=0,
            max_export_w=0,
            min_arbitrage_spread_ore_kwh=0,
        )


def test_realized_first_slot_reprices_both_actions_against_actual_base() -> None:
    value = diagnostic()
    value["slots"][0]["battery_w"] = -200
    response = {"plan": {"actions": [{"battery_w": -400}]}}
    realized = {
        1000: {
            "bucket_end_ms": 901000,
            "pv_w": -500,
            "ev_w": 0,
            "v2x_w": 0,
            "house_load_w": 1000,
            "total_ore_kwh": 100,
            "spot_ore_kwh": 50,
        }
    }
    result = realized_first_slot(value, response, realized, 11040, 11040)
    assert result is not None
    assert result["old_grid_w"] == 300
    assert result["new_grid_w"] == 100
    assert result["delta_ore"] == pytest.approx(-5)
    assert not result["mode_violation"]
