---
"forty-two-watts": minor
---

Rich heat-pump telemetry. The MyUplink driver now captures **every** device
point (not just four), emitting each as a metric with its unit. A new
click-through **detail view** on the Heat pump dashboard card groups all
signals by unit class (temperatures / power / frequency / percent / electrical
/ counters & degree-minutes / state), each with its current value and a 24h
sparkline. `host.emit_metric` gains an optional `unit` argument (carried into
the live snapshot for UI grouping); metric emission now also registers a driver
health success, so a read-only telemetry driver no longer shows as offline.
