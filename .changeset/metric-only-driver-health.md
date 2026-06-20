---
"forty-two-watts": patch
---

Fix read-only telemetry drivers showing "offline" with "last success:
never" despite live data. A driver that only calls `host.emit_metric`
(e.g. the MyUplink heat-pump driver) never recorded a health success, so
the watchdog flipped it offline even while it polled and emitted metrics
fine. `emit_metric` now records a health success like the structured
`host.emit` does. The dispatch stale-meter guard is unaffected — it keys
on per-reading (DerMeter) freshness, not driver-level last-success.
