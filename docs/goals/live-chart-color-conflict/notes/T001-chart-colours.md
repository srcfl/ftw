# T001 Scout receipt — Live chart colour mapping

## Summary

Live chart series → colour map lives in `web/next-app.js`. All colours
are hard-coded hex, NOT theme tokens. Collision confirmed:

- `Pixii` (battery driver) hashes to `BATTERY_PALETTE[7] = #a855f7`
- `Laddning bil EV` hashes to `EV_PALETTE[6] = #a855f7`

`web/components/theme.css` defines `--violet`, `--cyan`, `--cyan-dim`
and friends — but the chart does not reference them. There are no
`--chart-*` tokens. The full charter wording "all colour values come
from theme.css tokens" therefore makes a strict reading of the goal
into a larger refactor than the user's reported fix asks for.

## Evidence (file:line)

- `web/next-app.js:65-68` — `BATTERY_PALETTE` (8 hex)
- `web/next-app.js:69-75` — `batteryColor()` djb2 hash → palette idx
- `web/next-app.js:77-81` — `batteryLabel()`
- `web/next-app.js:117-123` — `EV_PALETTE` (8 hex)
- `web/next-app.js:124-130` — `evColor()`
- `web/next-app.js:131-134` — `evLabel()` ("name EV")
- `web/next-app.js:110` — legend swatch uses `batteryColor(name)`
- `web/next-app.js:155` — legend swatch uses `evColor(name)`
- `web/next-app.js:834-848` — series builder
- `web/next-app.js:835-837` — static series Grid #ef4444, PV #22c55e,
  Load #e2e8f0
- `web/index.html:343-347` — static legend HTML inline hex
- `web/legacy.html:201-205` — same legend in legacy
- `web/components/theme.css:49-59` — dark-theme tokens
- `web/components/theme.css:119-128` — light-theme overrides
- `web/components/theme.css:104` — `html[data-theme="light"]` selector
- `Makefile:100-107` — `make dev`

## Token inventory (theme.css)

| Token | Dark | Light |
|---|---|---|
| `--amber` | `oklch(0.82 0.18 75)` | `oklch(0.68 0.17 75)` |
| `--amber-d` | `oklch(0.65 0.14 75)` | `oklch(0.55 0.14 75)` |
| `--red-e` | `oklch(0.72 0.18 20)` | `oklch(0.55 0.20 20)` |
| `--green-e` | `oklch(0.78 0.16 150)` | `oklch(0.58 0.18 150)` |
| `--cyan` | `oklch(0.82 0.14 210)` | `oklch(0.58 0.14 210)` |
| `--cyan-dim` | `oklch(0.65 0.10 210)` | `oklch(0.48 0.10 210)` |
| `--violet` | `oklch(0.80 0.14 300)` | `oklch(0.55 0.14 300)` |
| `--white-s` | `oklch(0.95 0.01 250)` | `oklch(0.30 0.015 250)` |
| `--accent-e` | `oklch(0.82 0.16 65)` | `oklch(0.68 0.16 65)` |

## Candidate tokens for recolour

- **Primary: `--cyan`** — hue 210, distinct from Grid (red), PV (green),
  Load (white-ish), amber accent, and `--violet` (Pixii).
- Alt: `--cyan-dim` if `--cyan` competes with the accent visually.
- Rejected: `--violet` (already Pixii), `--amber*`/`--accent-e`
  (amber-only rule + Load fc proximity), `--red-e` (Grid), `--green-e`
  (PV), `--white-s` (Load).

## Light-theme pattern

`theme.css:104` uses `html[data-theme="light"]` to override token
*values*. The CLAUDE.md note "no `data-theme` branching" refers to
component code, not to `theme.css` itself. Token-based recolour will
flip cleanly across themes.

## Verification

- Dev server: `make dev` (Makefile:100-107), then open
  `http://localhost:8080`.
- Visual check: legend swatches for `Pixii` and `Laddning bil EV` must
  be distinct in both dark and light themes (theme toggle in the UI).
- No web lint or web tests exist (no `package.json` under `web/`).

## Target files for Worker

- `web/next-app.js` — `BATTERY_PALETTE`, `EV_PALETTE`, series builder,
  legend swatch background.
- `web/index.html` — static legend hex inline.
- `web/legacy.html` — same legend; Judge decides whether in scope.
- `web/components/theme.css` — optional `--chart-*` tokens if Judge
  wants a clean abstraction.

## Ambiguity for Judge

- **A1 — Scope**: minimal collision fix (swap one slot to a token
  lookup) vs. full tokenisation pass (replace every hex in the chart).
  Recommendation: minimal fix; full tokenisation = separate tranche.
- **A2 — Which keeps purple**: heuristic says Pixii → `--violet`,
  Laddning bil EV → `--cyan`. Code is neutral.
- **A3 — Hash determinism**: swapping a palette slot only helps the
  current driver names; new drivers could collide again. Restructure is
  out of slice.
- **A4 — `legacy.html`**: probably out of slice (next-app is what the
  field tester sees). Judge decides.

## Notes

- Driver name `laddning bil` comes from `config.yaml`, not Lua
  make/SN.
- Hex collision reproduced via node hash:
  `pixii` → slot 7 (#a855f7), `laddning bil` → slot 6 (#a855f7).
