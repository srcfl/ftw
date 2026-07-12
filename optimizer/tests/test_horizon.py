from __future__ import annotations

import math
import time

from ftw_optimizer.worker import handle


def test_48_hour_scenario_horizon_solves_within_host_budget() -> None:
    slots = []
    base_load = []
    base_pv = []
    for i in range(192):
        hour = (i % 96) / 4.0
        price = 80 + 180 * math.exp(-0.5 * ((hour - 18) / 2) ** 2)
        pv = -7000 * math.exp(-0.5 * ((hour - 12.5) / 3) ** 2) if 5 < hour < 21 else 0
        load = 500 + 1800 * math.exp(-0.5 * ((hour - 19) / 2) ** 2)
        base_load.append(load)
        base_pv.append(pv)
        slots.append(
            {
                "start_ms": 1 + i * 15 * 60 * 1000,
                "len_min": 15,
                "price_ore": price,
                "spot_ore": price * 0.7,
                "confidence": 1 if i < 96 else 0.6,
                "pv_w": pv,
                "load_w": load,
                "max_import_w": 11000,
                "max_export_w": 11000,
            }
        )
    request = {
        "schema_version": 1,
        "request_id": "horizon",
        "settings": {
            "mode": "passive_arbitrage",
            "solver": "HIGHS",
            "formulation": "auto",
            "time_limit_s": 8,
            "mip_rel_gap": 0.005,
            "cvar_weight": 0.15,
            "cvar_alpha": 0.9,
        },
        "slots": slots,
        "storages": [
            {
                "id": "home",
                "capacity_wh": 15000,
                "initial_energy_wh": 7500,
                "min_energy_wh": 1500,
                "max_energy_wh": 14250,
                "max_charge_w": 5000,
                "max_discharge_w": 5000,
                "charge_efficiency": 0.95,
                "discharge_efficiency": 0.95,
                "terminal_price_ore_kwh": 150,
                "cycle_cost_ore_kwh": 10,
            }
        ],
        "flex_loads": [],
        "thermal_loads": [],
        "scenarios": [
            {"id": "base", "probability": 0.6, "load_w": base_load, "pv_w": base_pv},
            {"id": "low-pv", "probability": 0.25, "load_w": base_load, "pv_w": [min(0, p + 500) for p in base_pv]},
            {"id": "high-pv", "probability": 0.15, "load_w": base_load, "pv_w": [p - 500 if p < 0 else 0 for p in base_pv]},
        ],
    }
    started = time.perf_counter()
    response = handle(request)
    elapsed = time.perf_counter() - started
    assert response["ok"], response
    assert len(response["plan"]["actions"]) == 192
    assert response["solver"]["scenario_count"] == 3
    # CI guard, deliberately looser than the 5 s production default because
    # shared runners vary. Production records solve_ms for Pi-specific tuning.
    assert elapsed < 15, f"48 h solve took {elapsed:.2f}s"
