---
"forty-two-watts": patch
---

Fix dashboard stalls on late-day loads by aggregating the `/api/status`
energy-today totals in SQLite instead of loading every history sample
since midnight into Go on every 2-second status poll.
