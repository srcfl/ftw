---
"forty-two-watts": minor
---

state: resilient two-tier storage with auto-heal

Disposable, re-fetchable data (spot prices, weather forecasts) now lives in a
separate `cache.db`, isolated from the precious `state.db` (trained models,
energy history, device identity). At boot each database runs `PRAGMA
quick_check`: a corrupt `cache.db` is quarantined and rebuilt empty
(re-fetched within the hour); a corrupt `state.db` is restored from a daily
recovery snapshot, or quarantined and started fresh if none exists. Every
recovery is surfaced on `GET /api/health` under `storage`, so DB corruption is
never a silent, blank-dashboard failure again. Existing installs migrate
automatically — `prices`/`forecasts` move from `state.db` to `cache.db` on
first boot.
