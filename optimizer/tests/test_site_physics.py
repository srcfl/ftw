from __future__ import annotations

import json
from pathlib import Path


FIXTURE = (
    Path(__file__).resolve().parents[2]
    / "go"
    / "internal"
    / "loadpoint"
    / "testdata"
    / "site_physics.json"
)


def grid_w(load_w: float, pv_w: float, battery_w: float, ev_w: float) -> float:
    return load_w + pv_w + battery_w + ev_w


def leftover_w(load_w: float, pv_w: float) -> float:
    return max(0.0, -(load_w + pv_w))


def house_residual_w(load_w: float, pv_w: float) -> float:
    return max(0.0, load_w + pv_w)


def battery_discharge_feeds_ev(
    battery_w: float, ev_w: float, load_w: float, pv_w: float
) -> bool:
    if ev_w <= 0 or battery_w >= 0:
        return False
    return -battery_w > house_residual_w(load_w, pv_w) + 50


def battery_energy_delta_wh(
    power_w: float, dt_h: float, charge_eff: float, discharge_eff: float
) -> float:
    if power_w >= 0:
        return power_w * dt_h * charge_eff
    return power_w * dt_h / discharge_eff


def test_site_physics_table_matches_go_kernel() -> None:
    fixture = json.loads(FIXTURE.read_text())
    for row in fixture["flows"]:
        assert grid_w(row["load_w"], row["pv_w"], row["battery_w"], row["ev_w"]) == row[
            "grid_w"
        ], row["name"]
        assert leftover_w(row["load_w"], row["pv_w"]) == row["leftover_w"], row["name"]
        assert house_residual_w(row["load_w"], row["pv_w"]) == row["house_residual_w"], row[
            "name"
        ]
        assert (
            battery_discharge_feeds_ev(
                row["battery_w"], row["ev_w"], row["load_w"], row["pv_w"]
            )
            is row["feeds_ev"]
        ), row["name"]
    for row in fixture["energy_steps"]:
        got = battery_energy_delta_wh(
            row["power_w"], row["dt_h"], row["charge_eff"], row["discharge_eff"]
        )
        assert abs(got - row["delta_wh"]) < 1e-9, row["name"]
