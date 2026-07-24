# Dashboard Simplification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the approved Terminal Native “Clean five” dashboard locally: Values-first Power now with the existing circle flow as an alternative, a compact price view, and one shared plain-language plan summary.

**Architecture:** Keep all existing backend and polling contracts. `app.js` continues to own live telemetry; `<ftw-price-chart>` gains a compact attribute backed by a pure price-summary helper; `plan.js` remains the only plan poller and renders both Overview and Plan from one pure normalized view model. Existing DOM IDs, safety wording, five destinations, and historical hash aliases stay compatible.

**Tech Stack:** Go-served static HTML, vanilla JavaScript ES modules, Web Components, CSS custom properties, Node’s built-in test runner, Playwright CLI for local browser verification.

## Global Constraints

- Preserve the site sign convention and do not touch drivers, planner dispatch, backend routes, or SQLite.
- Preserve `Overview / Energy / Plan / History / More` and `#live` / `#diagnose/<timestamp>` aliases.
- Reuse `ftw-hero-mode`: stored `hero` maps to Flow, stored `numbers` maps to Values, and no stored value defaults to Values.
- Keep one plan fetch loop in `web/plan.js`; Overview must not fetch `/api/mpc/plan`.
- Keep the existing live-value element IDs so `web/app.js` stays the only telemetry rendering path.
- Use amber only for interaction/current selection; retain semantic green/red/cyan state colors.
- Preserve all unrelated dirty worktree files. Stage only the files named in each task.
- Do not push or open a pull request during this local-preview pass.

## File Responsibility Map

| File | Responsibility after this change |
|---|---|
| `web/power-now.js` | Pure preference compatibility plus DOM/keyboard controller for Values and Flow |
| `web/app.js` | Existing live status poll; fills both detailed and compact Overview readings |
| `web/components/price-summary.js` | Pure current-price and next-low normalization |
| `web/components/ftw-price-chart.js` | Full and compact price presentations using the same preference/data semantics |
| `web/plan-brief.js` | Pure plan/status-to-view-model normalization |
| `web/plan.js` | Sole plan poller and renderer for Overview plus detailed Plan |
| `web/diagnose.js` | Destination routing and ordering of existing stateful sections |
| `web/index.html` | Semantic dashboard structure and stable rendering targets |
| `web/app.css` | Terminal Native hierarchy, responsive layout, and focus/reduced-motion behavior |
| `web/components/ftw-energy-flow.js` | Existing flow visualization, with an embedded-heading suppression hook |
| `web/components/ftw-savings-card.js` | Existing historical card plus a one-day compact Overview rendering |

---

### Task 1: Lock the Power now preference contract

**Files:**

- Create: `web/power-now.js`
- Create: `web/power-now.test.mjs`
- Modify: `web/index.html`

**Interface:**

```js
export function normalizePowerNowMode(storedValue)
// null/unknown/"numbers"/"values" -> "values"
// "hero"/"flow"                  -> "flow"

export function storedPowerNowMode(mode)
// "flow" -> "hero"; "values" -> "numbers"

export function initPowerNow(root = document, storage = localStorage)
// Applies the mode, persists changes when possible, and returns cleanup().
```

- [ ] Write `web/power-now.test.mjs` first with table-driven cases for the legacy values, the new Values default, unknown/corrupt storage, and stored output compatibility.

```js
for (const [stored, expected] of [
  [null, "values"],
  ["numbers", "values"],
  ["values", "values"],
  ["hero", "flow"],
  ["flow", "flow"],
  ["corrupt", "values"],
]) {
  assert.equal(normalizePowerNowMode(stored), expected);
}
```

- [ ] Add a small fake-root/fake-storage test proving selection updates `aria-selected`, the matching panel `hidden` state, and the legacy `mode-numbers` / `mode-hero` body class even when storage throws.
- [ ] Run `node --test web/power-now.test.mjs` and confirm it fails because the module does not exist.
- [ ] Implement the pure mappings and the controller. ArrowLeft/ArrowRight/Home/End must move focus and selection; click must select; storage failures must be caught.
- [ ] Remove the old inline Hero/numbers IIFE from `web/index.html` and load `/power-now.js` as a module after the markup.
- [ ] Run `node --test web/power-now.test.mjs web/javascript-syntax.test.mjs` and confirm both pass.
- [ ] Commit only these files:

```bash
git add web/power-now.js web/power-now.test.mjs web/index.html
git commit -m "feat(ui): add power now view controller"
```

### Task 2: Build the Values-first Overview shell

**Files:**

- Create: `web/dashboard-simplification.test.mjs`
- Modify: `web/index.html`
- Modify: `web/app.css`
- Modify: `web/app.js`
- Modify: `web/components/ftw-energy-flow.js`
- Modify: `web/components/ftw-savings-card.js`
- Modify: `web/mobile-information-architecture.test.mjs`

**DOM contract:**

```text
#view-overview
  .overview-heading
  #power-now
    #power-now-tab-values -> #power-now-values
    #power-now-tab-flow   -> #power-now-flow
      #energy-flow
  .overview-outlook
    #overview-price       -> <ftw-price-chart compact>
    #overview-plan-summary
  #overview-today
  .fuse-row
```

- [ ] Add failing structural assertions that Overview owns `#power-now`, both accessible tabs/panels, one compact price component, one plan-summary target, one today strip, and the existing `#energy-flow`.
- [ ] Add failing compatibility assertions that the five desktop/mobile destinations still exist and that the live IDs `grid-w`, `pv-w`, `load-w`, `bat-w`, `card-ev-w`, and `energy-flow` occur exactly once.
- [ ] Run `node --test web/dashboard-simplification.test.mjs web/mobile-information-architecture.test.mjs` and confirm the new assertions fail.
- [ ] Replace the loose hero plus five equal cards with one bounded Power now card. Preserve existing live IDs and click targets; add `#bat-soc` alongside the battery reading.
- [ ] Put the existing `<ftw-energy-flow>` in the Flow tab panel and add an `embedded` attribute. In `ftw-energy-flow.js`, hide only its internal `.title` for `:host([embedded])`; keep the component, setReadings path, planet events, and aggregation control unchanged.
- [ ] Add the compact Overview heading, price/plan side-by-side container, today strip, and fuse below the primary answers.
- [ ] Update `app.js` so `formatW(null/undefined/non-finite)` renders `—`, `#bat-soc` renders a percentage only when available, and the Overview import/export/PV values mirror the existing daily totals without another fetch.
- [ ] Add `<ftw-savings-card compact>` to the today strip. Extend the component’s observed attributes and one-day query behavior so compact mode renders `Saved today` with a single value and no week/month control or chart.
- [ ] Add Terminal Native styles: one outer Power now surface, column values on desktop, compact rows on mobile, 44 px segmented targets, clear focus rings, and no duplicate card borders.
- [ ] Add a reduced-motion rule that disables segmented-control and flow-adjacent decorative transitions without suppressing state changes.
- [ ] Run the two structural tests plus `web/javascript-syntax.test.mjs`; confirm they pass.
- [ ] Commit only the task files:

```bash
git add web/dashboard-simplification.test.mjs web/index.html web/app.css web/app.js web/components/ftw-energy-flow.js web/components/ftw-savings-card.js web/mobile-information-architecture.test.mjs
git commit -m "feat(ui): simplify overview power hierarchy"
```

### Task 3: Add the compact price mode

**Files:**

- Create: `web/components/price-summary.js`
- Create: `web/components/price-summary.test.mjs`
- Modify: `web/components/ftw-price-chart.js`
- Modify: `web/index.html`

**Interface:**

```js
export function buildPriceSummary(items, {
  now,
  vatOn,
  vatPercent,
})
// Returns:
// {
//   current: { tsMs, lenMin, ore } | null,
//   nextLow: { tsMs, lenMin, ore } | null,
//   today: [{ tsMs, lenMin, ore }],
//   minOre,
//   maxOre,
// }
```

**Component state contract:**

```text
loading      -> "Loading prices…"
unconfigured -> "Price unavailable" + Price settings path
ready        -> current price + next low + compact today profile
stale        -> retain last ready data and append "Last update failed"
error        -> unavailable message without an empty SVG
```

- [ ] Write failing pure tests with a fixed `now` for current-slot selection, VAT-on price math, future-low selection, midnight filtering, no current slot, and an empty item list.
- [ ] Run `node --test web/components/price-summary.test.mjs` and confirm module-not-found failure.
- [ ] Implement the pure helper without DOM or wall-clock reads outside the injected `now`.
- [ ] Add `compact` to `FtwPriceChart.observedAttributes`, track loading/ready/unconfigured/stale/error separately from `_data`, and preserve last good data on a later fetch failure.
- [ ] In compact mode render an accessible text summary, current `öre/kWh`, next-low time/value, a small non-interactive today profile, and an `href="#energy"` full-view link. Omit VAT/horizon controls and hover-only detail.
- [ ] Keep the detailed Energy instance unchanged in capability and make both instances read the same `ftw.priceChart.vatOn` preference.
- [ ] Extend the structural test to assert one compact Overview instance and one non-compact detailed instance.
- [ ] Run `node --test web/components/price-summary.test.mjs web/dashboard-simplification.test.mjs web/javascript-syntax.test.mjs`.
- [ ] Commit only the price files and markup:

```bash
git add web/components/price-summary.js web/components/price-summary.test.mjs web/components/ftw-price-chart.js web/index.html web/dashboard-simplification.test.mjs
git commit -m "feat(ui): add compact dashboard price view"
```

### Task 4: Normalize and share the plan briefing

**Files:**

- Create: `web/plan-brief.js`
- Create: `web/plan-brief.test.mjs`
- Modify: `web/plan.js`
- Modify: `web/index.html`
- Modify: `web/app.css`
- Modify: `web/mobile-information-architecture.test.mjs`
- Modify: `web/plan-fallback-warning.test.mjs`

**Interface:**

```js
export function derivePlanBrief({
  enabled,
  plan,
  status,
  now,
})
// Returns one view model:
// {
//   state: { key, label, tone },
//   next: { action, time },
//   reason,
//   constraint,
//   forecast: { label, detail },
//   soc: null | { label, detail },
//   planner: { label, detail },
// }
```

- [ ] Write failing table-driven tests for planner off, preparing, active, ready, stale, solver fallback, active safety clamp, and no-battery output. Use fixed slot timestamps and an injected `now`.
- [ ] Run `node --test web/plan-brief.test.mjs` and confirm it fails because the helper is absent.
- [ ] Move `readableReason`, action labeling, slot choice, safety clamp wording, forecast confidence wording, expected SoC wording, and solver naming into `derivePlanBrief`.
- [ ] Convert `plan.js` to an ES module import of the helper. Keep `fetchAll()` and `PLAN_REFRESH_MS` as the sole `/api/mpc/plan` polling path.
- [ ] Replace the five equal Plan briefing cards with primary state/action, secondary reason/safety, and a compact metadata row. Keep the existing element IDs where practical for compatibility.
- [ ] Add Overview render targets and a renderer that maps the same view model to the compact state badge, next action/time, reason, safety adjustment, optional SoC, and `href="#plan"` link.
- [ ] Ensure no-battery output hides the SoC row rather than rendering a misleading unavailable battery state.
- [ ] Update fallback/source tests to import or assert the helper output rather than depending on duplicated strings in `plan.js`.
- [ ] Run `node --test web/plan-brief.test.mjs web/mobile-information-architecture.test.mjs web/plan-fallback-warning.test.mjs web/javascript-syntax.test.mjs`.
- [ ] Commit only the plan files:

```bash
git add web/plan-brief.js web/plan-brief.test.mjs web/plan.js web/index.html web/app.css web/mobile-information-architecture.test.mjs web/plan-fallback-warning.test.mjs
git commit -m "feat(ui): share plain language plan summary"
```

### Task 5: Clarify Energy, History, and responsive hierarchy

**Files:**

- Modify: `web/diagnose.js`
- Modify: `web/index.html`
- Modify: `web/app.css`
- Modify: `web/dashboard-simplification.test.mjs`
- Modify: `web/mobile-information-architecture.test.mjs`

**Destination order contract:**

```text
Energy:  heading -> Live -> today totals -> full price -> heating/24h detail
History: heading -> history numbers -> savings -> raw energy report -> decisions
Plan:    heading -> condensed briefing/controls -> detailed plan chart
```

- [ ] Add failing source/DOM assertions for the intended selector order in `organizeDestinations()` and for the History outcome anchor preceding raw reporting.
- [ ] Run the two dashboard information-architecture tests and confirm the new order assertions fail.
- [ ] Change `diagnose.js` to append the existing stateful Energy sections in the new order rather than cloning them.
- [ ] Insert History numbers and savings before `.energy-history-header`; retain the raw history DOM and `.diagnose-header` after them so deep-link selection still works.
- [ ] Add low-emphasis wrappers/tokens for raw reporting and plan-decision diagnostics without hiding controls or reducing text contrast.
- [ ] Finish the 12-column desktop Overview grid: Power now full width, price and plan 6 columns each, today and fuse full width. At `max-width: 720px`, stack in reading order and reserve bottom space for sticky navigation.
- [ ] Check 320–390 px CSS for no horizontal overflow, 44 px interactive targets, readable plan metadata, and a Flow panel that may grow vertically without clipping.
- [ ] Run `node --test web/dashboard-simplification.test.mjs web/mobile-information-architecture.test.mjs web/javascript-syntax.test.mjs`.
- [ ] Commit only the hierarchy files:

```bash
git add web/diagnose.js web/index.html web/app.css web/dashboard-simplification.test.mjs web/mobile-information-architecture.test.mjs
git commit -m "feat(ui): clarify dashboard destination hierarchy"
```

### Task 6: Changeset, full verification, and local browser handoff

**Files:**

- Create: `.changeset/simplify-dashboard-outlook.md`
- Modify only if verification finds a scoped defect: files from Tasks 1–5

- [ ] Add a patch changeset:

```md
---
"ftw": patch
---

Simplify the dashboard with a Values-or-Flow Power now view, a compact price outlook, and a shared plain-language plan summary.
```

- [ ] Run the complete web suite:

```bash
npm test
```

- [ ] Run repository verification:

```bash
make verify
```

- [ ] Start or restart the local development app using the repository’s existing local config/state and open `http://127.0.0.1:8080/#overview`.
- [ ] In a fresh browser context verify Values is selected; switch to Flow, reload, and verify the circles and selection persist.
- [ ] At desktop width verify current price, next low, plan-off/local state, today strip, fuse, and each of the five destinations.
- [ ] At 390 × 844 verify the first-screen reading order, segmented keyboard/touch targets, no horizontal scroll, and bottom-nav clearance.
- [ ] Verify dark and light themes, Flow planet click targets, `#live`, `#diagnose`, and browser console errors.
- [ ] Capture local desktop/mobile screenshots outside tracked source paths for the user preview.
- [ ] Inspect the final diff and confirm no backend, driver, optimizer, version, changelog, or unrelated dirty files are included:

```bash
git diff --check
git status --short
git diff --stat HEAD
```

- [ ] Commit the changeset and any verification-only scoped fixes:

```bash
git add .changeset/simplify-dashboard-outlook.md
git commit -m "chore: add dashboard simplification changeset"
```

- [ ] Leave the branch local and report the preview URL, test evidence, and any intentional local-only artifacts. Do not push or create a PR.

## Plan Self-Review

- Every design requirement maps to a task: Values/Flow (1–2), compact price (3), shared plan brief (4), Clean five hierarchy (5), and local desktop/mobile verification (6).
- Data ownership is singular: status in `app.js`, prices in the component, plan in `plan.js`.
- All new calculation logic is pure and testable with injected time.
- Accessibility is explicit for the segmented control, state text, summaries, focus, touch size, reduced motion, and chart alternatives.
- Compatibility is explicit for DOM IDs, storage values, routes, custom-element identity, and plan polling.
- Release hygiene is explicit: changeset required, unrelated files excluded, no push/PR.
