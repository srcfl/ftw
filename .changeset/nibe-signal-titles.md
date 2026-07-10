---
"forty-two-watts": minor
---

The heat-pump "all signals" detail view can now explain every signal.
`host.emit_metric` gained an optional 5th `title` argument — the device's own
human-readable point label — which threads through the telemetry snapshot and
`/api/drivers/{name}` (as `title`) so the UI can show a plain-language line under
each of the ~960 raw signals. The NIBE local driver passes each point's NIBE
title (e.g. "Frånluft (BT20)"); other drivers are unaffected (the arg defaults
to empty).
