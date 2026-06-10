---
"forty-two-watts": minor
---

Add a Tesla-style manual charge control to the EV charging modal: an amp
slider (range = the loadpoint's min/max charge current) plus Start / Stop.
Start pins a **persistent** manual hold at the chosen amperage that now
**overrides `surplus_only`** — when the operator explicitly asks to charge,
we honour it and import from the grid if PV is short. Stop releases the hold
and drops straight back to automatic charging (PV-surplus-only when that
toggle is on).

Behaviour change: previously `surplus_only` clamped any manual hold down to
the available PV surplus (a manual "Start" with no sun did nothing). A manual
hold now takes priority over surplus; the per-phase fuse clamp remains the one
guard a manual hold can never override. Persistent holds carry no time expiry
and are auto-released when the vehicle unplugs. `POST .../manual_hold` now
accepts `hold_s: 0` (or omitted) to mean a persistent hold; `hold_s > 0` is
still the bounded diagnostic hold. `GET /api/loadpoints` gains `phases`,
`voltage_v`, `manual_active`, and `manual_charge_w` so the UI can render the
amp slider and reflect the current override.

The now-redundant Start / Pause / Resume footer buttons in the EV modal are
removed — the amp slider's Start/Stop supersedes them (Start is strictly more
capable: it pins a chosen amperage instead of always MaxChargeW). The
`POST /api/ev/command` endpoint is retained for Home Assistant / scripts.
