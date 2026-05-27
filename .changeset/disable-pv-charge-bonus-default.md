---
"forty-two-watts": minor
---

Disable the passive-arbitrage PV-charge bonus by default (was 30 öre/kWh).

The bonus credited each kWh of battery charge fed from live PV surplus,
intended to break ties when the DP saw "store PV now" and "export PV
now, reimport later" as economically equivalent. In practice the import
tariff + VAT asymmetry already makes storage strictly preferred under
typical retail pricing, so the bonus was redundant.

The redundancy is harmless on flat-price days, but on days with future
negative-price hours the bonus pulled morning battery charging forward
to the point where no SoC headroom remained when the negative-price
window arrived — forcing PV export against negative prices instead of
absorbing the (paid-to-consume) energy into the battery.

Behavior change: operators who relied on the bonus can re-enable it
explicitly via `planner.pv_charge_bonus_ore_kwh` in `config.yaml`.
The previous fallback that silently reinstated 30 öre/kWh when the
value was set to 0 has also been removed — 0 now means 0.
