---
"forty-two-watts": patch
---

Dashboard light-mode + heat-pump card fixes.

The live power/energy **charts now render correctly in light mode**. The canvas chrome (axis labels, gridlines, zero/now/hover lines, both tooltips' background+border+text, and the neutral "Load" line + legend swatch) was hard-coded for the dark theme, so it went invisible or wrong on a light background. The charts now resolve the CSS theme tokens (`--fg`, `--fg-dim`, `--fg-muted`, `--line`, `--ink-raised`, `--accent-e`) into concrete canvas colors per draw — cached and re-read when `data-theme` changes. Saturated data-series hues are unchanged.

The **heat-pump card now re-discovers newly-added drivers** without a manual reload. `heating.js` cached discovery once and never re-checked — and an empty result is truthy, so a site that discovered before its pump reported `hp_power_w` stayed blank. It now re-scans on first load and every 5 minutes, while steady-state polling still only touches already-known heat-pump drivers.

The heat-pump **"all signals" detail view now shows a Register column** — each signal's source Modbus register id (read from the metric snapshot the driver reports). Signals with no Modbus mapping show "—".
