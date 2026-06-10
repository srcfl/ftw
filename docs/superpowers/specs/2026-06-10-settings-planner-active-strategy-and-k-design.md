# Settings → Planner: active-strategy display + PV-safety (k) clarity

**Date:** 2026-06-10
**Issue:** [#479](https://github.com/frahlg/forty-two-watts/issues/479)
**Scope:** web-only — `web/settings/tabs/planner.js` (+ a small test file). No Go changes.

## Background

A field operator running v0.120.7 reported that Settings → Planner showed
`self_consumption` while the dashboard Plan card ran passive arbitrage, and
concluded the system had failed to charge the batteries. Investigation showed
the system was executing passive arbitrage correctly; the confusion came from
two UI problems:

1. **The Mode dropdown is a dead knob.** It edits config-YAML `planner.mode`,
   which (a) requires a restart (`planner.*` is constructed once at startup),
   (b) is overridden anyway by the runtime mode persisted in the state DB —
   `main.go` pushes the persisted control mode into the MPC service
   (*"not whatever cfg.planner.mode says"*), and (c) offers two legacy
   strategies (`self_consumption`, `cheap_charge`) the dashboard no longer
   exposes.
2. **"PV forecast safety (k)" invites the wrong mental model** — "safety
   factor ⇒ charges safely". The real mechanism is a downside hedge: the DP
   plans against `forecast − k·σ`, where σ is the live PV-forecast error std
   (`pv_residual_std_w` from `/api/pvmodel`). Higher k ⇒ trust the forecast
   less ⇒ hold more reserve / charge earlier. It never forces charging.

## Fix 1 — replace the Mode dropdown with a read-only Active strategy row

Remove `selectField("Mode", "planner.mode", …)` from the tab. In its place,
render a read-only row:

```
ACTIVE STRATEGY
Passive arbitrage
Set from the Plan card on the dashboard — not editable here.
```

- The value is filled in the tab's `after(ctx)` hook from `GET /api/status`
  → `mode`. Until the fetch resolves (or on fetch failure) the value shows
  `—`.
- Mode → label mapping mirrors the dashboard Strategy picker:
  - `planner_passive_arbitrage` → `Passive arbitrage`
  - `planner_arbitrage` → `Active arbitrage`
  - `planner_self` / `planner_cheap` → `Self-consumption (planner, legacy)` /
    `Cheap charge (planner, legacy)`
  - any non-planner mode (e.g. `self_consumption`, `idle`, `charge`) →
    `<Mode> (manual — planner not dispatching)`, where `<Mode>` is the mode
    string with underscores replaced by spaces and the first letter
    capitalised.
- The YAML field `planner.mode` keeps existing as the first-boot/headless
  default. It is simply no longer exposed in the UI. Operators who saved a
  config through the modal keep whatever value is in their YAML — saving the
  form must not clear or rewrite `planner.mode` (the settings shell only
  writes paths bound to rendered fields, so removing the field is enough).

## Fix 2 — correct the k help text and show the live hedge

Keep the label `PV forecast safety (k)`. Replace the help text with (final
wording, English like the rest of the UI):

> How much the planner trusts the solar forecast. It plans against
> forecast − k×σ, where σ is the live PV-forecast error. Higher k = trust
> the forecast less: the battery holds more reserve and charges earlier,
> drifting toward self-consumption behaviour. 0 = trust the forecast fully
> (no hedge). On clear, stable days σ shrinks toward zero and k has little
> effect — the hedge sizes itself to the real risk.

Under the input, render a live line (hidden until data arrives):

```
σ right now ≈ 432 W → hedge = k·σ ≈ 432 W
```

- σ comes from `GET /api/pvmodel` → `pv_residual_std_w`, fetched once in the
  same `after(ctx)` hook.
- The hedge value recomputes live on the k input's `input` event (current
  field value × σ), so the operator sees the effect of a new k before saving.
- Edge cases:
  - `/api/pvmodel` unreachable, `enabled: false`, or missing field → the
    line stays hidden.
  - σ ≈ 0 (< 1 W) → show `σ right now ≈ 0 W — no hedge` and skip the arrow
    part.
  - Non-numeric/empty k in the field → treat as 0 for the live line.
- Values are rounded to whole watts.

## Styling

Follow `DESIGN.md` and the existing settings-modal conventions: reuse the
`field-row` markup, tokens only (`var(--text-dim)` for the subtext and live
line, `var(--mono)` where the modal already uses mono for values), no
hard-coded colours, no new component-level hues.

## Testing

Extract the two pure functions so they are testable without DOM:

- `strategyLabel(mode)` — mode string → display label (table above).
- `hedgeLine(k, sigmaW)` — numbers → rendered text (or `null` when hidden).

The settings tabs are classic (non-module) scripts attached to
`window.FTWSettings`, so the ESM-export pattern used by
`ftw-pair-card-render.js` does not apply directly. Instead: attach the pure
functions to the tab object (`S.tabs.planner._pure = { strategyLabel,
hedgeLine }`), and in the test stub `globalThis.window = {}` before a
dynamic `await import("./planner.js")`, then reach the functions through
`globalThis.window.FTWSettings.tabs.planner._pure`.

Cover with `web/settings/tabs/planner.test.mjs`: label mapping for every
planner mode + a manual mode, hedge formatting for normal σ, σ=0, and
empty k. Note CI only runs `go test` — these run developer-side via
`node --test web/settings/tabs/planner.test.mjs` (same as the existing
`web/components/*.test.mjs` files).

Manual verification on the dev server (`make dev`): open Settings → Planner,
confirm the active strategy renders, edit k and watch the hedge line update,
save and confirm `planner.mode` in the YAML is untouched.

## Delivery

- Branch `479-settings-planner-ui` from `origin/master`, PR in English
  referencing #479.
- Changeset: **patch** — "Settings → Planner: replace the dead mode dropdown
  with a live active-strategy display; clarify PV forecast safety (k) with
  corrected help text and a live σ/hedge readout."
