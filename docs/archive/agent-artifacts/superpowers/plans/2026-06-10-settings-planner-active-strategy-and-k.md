# Settings → Planner: active-strategy display + k clarity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the dead `planner.mode` dropdown in Settings → Planner with a live read-only "Active strategy" row, and fix the "PV forecast safety (k)" field so operators stop misreading it (correct help text + live σ/hedge readout).

**Architecture:** Web-only change to one settings tab. Two pure functions (`strategyLabel`, `hedgeLine`) live inside the tab's IIFE and are exposed via `S.tabs.planner._pure` for Node tests (the settings tabs are classic non-module scripts — tests stub `globalThis.window` and dynamic-import the file). Live data arrives in the tab's `after(ctx)` hook: `/api/status` for the mode, `/api/modes` for labels when PR #468 has landed (graceful fallback when it hasn't), `/api/pvmodel` for σ.

**Tech Stack:** Vanilla JS (IIFE settings tab), `node --test` for unit tests. No Go changes.

**Spec:** `docs/superpowers/specs/2026-06-10-settings-planner-active-strategy-and-k-design.md`
**Issue:** #479 · **Branch:** `479-settings-planner-ui` (worktree at `.claude/worktrees/479-settings-planner-ui`)

---

### Task 1: Pure helpers `strategyLabel` + `hedgeLine`, with tests

**Files:**
- Modify: `web/settings/tabs/planner.js` (add helpers + `_pure` export inside the IIFE)
- Create: `web/settings/tabs/planner.test.mjs`

- [ ] **Step 1: Write the failing test**

Create `web/settings/tabs/planner.test.mjs`:

```mjs
// node --test web/settings/tabs/planner.test.mjs
//
// Pure-function tests for the Settings → Planner tab. The tab is a
// classic (non-module) script that attaches to window.FTWSettings, so
// stub a window object and dynamic-import the file; the helpers are
// reachable via the _pure escape hatch.

import { describe, it } from "node:test";
import assert from "node:assert/strict";

globalThis.window = {};
await import("./planner.js");
const tab = globalThis.window.FTWSettings.tabs.planner;
const { strategyLabel, hedgeLine } = tab._pure;

describe("strategyLabel", () => {
  it("maps every planner mode via the local fallback", () => {
    assert.equal(strategyLabel("planner_passive_arbitrage", null), "Passive arbitrage");
    assert.equal(strategyLabel("planner_arbitrage", null), "Active arbitrage");
    assert.equal(strategyLabel("planner_self", null), "Self-consumption (planner, legacy)");
    assert.equal(strategyLabel("planner_cheap", null), "Cheap charge (planner, legacy)");
  });

  it("suffixes non-planner modes as manual", () => {
    assert.equal(
      strategyLabel("self_consumption", null),
      "Self consumption (manual — planner not dispatching)"
    );
    assert.equal(strategyLabel("idle", null), "Idle (manual — planner not dispatching)");
  });

  it("prefers the /api/modes catalog label when present", () => {
    const catalog = [{ key: "planner_passive_arbitrage", label: "Passive arbitrage (catalog)" }];
    assert.equal(strategyLabel("planner_passive_arbitrage", catalog), "Passive arbitrage (catalog)");
  });

  it("still suffixes manual modes when the catalog provides the label", () => {
    const catalog = [{ key: "idle", label: "Idle" }];
    assert.equal(strategyLabel("idle", catalog), "Idle (manual — planner not dispatching)");
  });

  it("returns a dash for missing mode", () => {
    assert.equal(strategyLabel(null, null), "—");
    assert.equal(strategyLabel("", null), "—");
  });
});

describe("hedgeLine", () => {
  it("formats a normal σ with the hedge product", () => {
    assert.equal(hedgeLine("1", 432.16), "σ right now ≈ 432 W → hedge = k·σ ≈ 432 W");
    assert.equal(hedgeLine("2", 432.16), "σ right now ≈ 432 W → hedge = k·σ ≈ 864 W");
  });

  it("treats empty or junk k as 0", () => {
    assert.equal(hedgeLine("", 432.16), "σ right now ≈ 432 W → hedge = k·σ ≈ 0 W");
    assert.equal(hedgeLine("abc", 432.16), "σ right now ≈ 432 W → hedge = k·σ ≈ 0 W");
  });

  it("reports no hedge when σ is ~0", () => {
    assert.equal(hedgeLine("1", 0), "σ right now ≈ 0 W — no hedge");
    assert.equal(hedgeLine("1", 0.4), "σ right now ≈ 0 W — no hedge");
  });

  it("returns null when σ is missing or invalid (line stays hidden)", () => {
    assert.equal(hedgeLine("1", null), null);
    assert.equal(hedgeLine("1", undefined), null);
    assert.equal(hedgeLine("1", NaN), null);
    assert.equal(hedgeLine("1", -5), null);
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/479-settings-planner-ui
node --test web/settings/tabs/planner.test.mjs
```

Expected: FAIL — `TypeError: Cannot destructure property 'strategyLabel' of 'tab._pure' as it is undefined` (the tab object exists but has no `_pure`).

- [ ] **Step 3: Add the helpers to `planner.js`**

In `web/settings/tabs/planner.js`, after the line `S.tabs = S.tabs || {};` and before `S.tabs.planner = {`, insert:

```js
  // strategyLabel maps a control-mode string to the operator-facing
  // label. Prefers the /api/modes catalog (PR #468) when provided so we
  // don't become yet another hard-coded copy of the mode list; falls
  // back to a local table, then to prettifying the raw mode string.
  // Non-planner modes get a "(manual …)" suffix: the planner computes a
  // plan but the dispatcher isn't following it.
  var STRATEGY_LABELS = {
    planner_passive_arbitrage: "Passive arbitrage",
    planner_arbitrage: "Active arbitrage",
    planner_self: "Self-consumption (planner, legacy)",
    planner_cheap: "Cheap charge (planner, legacy)",
  };

  function strategyLabel(mode, catalog) {
    if (!mode) return "—";
    var label = null;
    if (catalog && catalog.length) {
      for (var i = 0; i < catalog.length; i++) {
        if (catalog[i] && catalog[i].key === mode && catalog[i].label) {
          label = catalog[i].label;
          break;
        }
      }
    }
    if (!label) label = STRATEGY_LABELS[mode];
    if (!label) {
      label = mode.replace(/_/g, " ");
      label = label.charAt(0).toUpperCase() + label.slice(1);
    }
    if (mode.indexOf("planner_") !== 0) label += " (manual — planner not dispatching)";
    return label;
  }

  // hedgeLine renders the live "what does k actually do" readout under
  // the k input: σ (the live PV-forecast error std from /api/pvmodel)
  // and the resulting hedge k·σ in watts. Returns null when σ is
  // missing/invalid — the caller keeps the line hidden.
  function hedgeLine(k, sigmaW) {
    if (sigmaW == null || typeof sigmaW !== "number" || isNaN(sigmaW) || sigmaW < 0) return null;
    var sigma = Math.round(sigmaW);
    if (sigma < 1) return "σ right now ≈ 0 W — no hedge";
    var kn = parseFloat(k);
    if (isNaN(kn) || kn < 0) kn = 0;
    return "σ right now ≈ " + sigma + " W → hedge = k·σ ≈ " + Math.round(kn * sigma) + " W";
  }
```

Then, after the closing `};` of `S.tabs.planner = { … }` (still inside the IIFE), add:

```js
  // Escape hatch for node --test (planner.test.mjs); not a public API.
  S.tabs.planner._pure = { strategyLabel: strategyLabel, hedgeLine: hedgeLine };
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
node --test web/settings/tabs/planner.test.mjs
```

Expected: PASS (all `strategyLabel` + `hedgeLine` tests green).

- [ ] **Step 5: Commit**

```bash
git add web/settings/tabs/planner.js web/settings/tabs/planner.test.mjs
git commit -m "feat(settings): pure strategyLabel + hedgeLine helpers for the planner tab (#479)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Replace the Mode dropdown with the read-only Active-strategy row

**Files:**
- Modify: `web/settings/tabs/planner.js` (render + new `after` hook)
- Test: `web/settings/tabs/planner.test.mjs` (render regression test)

- [ ] **Step 1: Write the failing render test**

Append to `web/settings/tabs/planner.test.mjs`:

```mjs
describe("render", () => {
  function stubCtx() {
    return {
      config: { planner: {} },
      field: (label, path) => "[field:" + path + "]",
      selectField: (label, path) => "[select:" + path + "]",
      help: () => "[?]",
    };
  }

  it("no longer renders the planner.mode dropdown", () => {
    const html = tab.render(stubCtx());
    assert.ok(!html.includes("planner.mode"), "planner.mode must not be bound in the form");
  });

  it("renders the active-strategy placeholder and hedge line containers", () => {
    const html = tab.render(stubCtx());
    assert.ok(html.includes('id="planner-active-strategy"'));
    assert.ok(html.includes('id="planner-hedge-line"'));
    assert.ok(html.includes("Set from the Plan card on the dashboard"));
  });
});
```

- [ ] **Step 2: Run the test to verify the new cases fail**

```bash
node --test web/settings/tabs/planner.test.mjs
```

Expected: FAIL — `planner.mode must not be bound in the form` (the dropdown is still rendered) and the `planner-active-strategy` assertion fails. (The `planner-hedge-line` assertion is satisfied in Task 3 — see note in Step 3.)

- [ ] **Step 3: Replace the dropdown in `render`**

In `web/settings/tabs/planner.js` `render`, delete this block:

```js
        selectField("Mode", "planner.mode", ["passive_arbitrage", "arbitrage", "self_consumption", "cheap_charge"], "passive_arbitrage",
          "passive_arbitrage = charge from cheapest source (PV or cheap grid), never export from battery. arbitrage = full timing arbitrage including battery export. self_consumption / cheap_charge = legacy (use passive_arbitrage instead).") +
```

and put this in its place:

```js
        '<label>Active strategy ' +
        help("The strategy the planner is running right now. It is chosen with the Strategy buttons on the dashboard Plan card and persists across restarts. The config file's planner.mode is only the first-boot default and is not editable here.") +
        '</label>' +
        '<div id="planner-active-strategy" style="font-family:var(--mono);margin:2px 0 0">—</div>' +
        '<p style="color:var(--text-dim);font-size:0.8rem;margin:4px 0 12px">Set from the Plan card on the dashboard — not editable here.</p>' +
```

Note `render` already receives `help` via `var field = ctx.field, selectField = ctx.selectField, help = ctx.help, config = ctx.config;`. `selectField` is now unused — remove it from that `var` line. For this step to also satisfy the third test assertion, Task 3 Step 2 adds `planner-hedge-line`; if you are executing tasks strictly in order, expect exactly one remaining failure (`planner-hedge-line`) after this step, which Task 3 clears.

- [ ] **Step 4: Add the `after` hook**

In `web/settings/tabs/planner.js`, the tab object currently only has `render`. Add an `after` key after it (Task 3 extends this same hook):

```js
    after: function (ctx) {
      var ownerFetch = ctx.ownerFetch || window.fetch.bind(window);

      // ---- Active strategy (read-only, from the runtime, not the YAML) ----
      var stratEl = document.getElementById("planner-active-strategy");
      if (stratEl) {
        // /api/modes is the server-side mode catalog from PR #468; older
        // hosts 404 it — treat any failure as "no catalog" and fall back
        // to the local label table.
        var catalogP = ownerFetch("/api/modes")
          .then(function (r) { return r.ok ? r.json() : null; })
          .then(function (d) { return d && d.modes ? d.modes : null; })
          .catch(function () { return null; });
        var modeP = ownerFetch("/api/status")
          .then(function (r) { return r.json(); })
          .then(function (d) { return d && d.mode; })
          .catch(function () { return null; });
        Promise.all([modeP, catalogP]).then(function (res) {
          stratEl.textContent = strategyLabel(res[0], res[1]);
        });
      }
    },
```

- [ ] **Step 5: Run the tests + syntax check**

```bash
node --test web/settings/tabs/planner.test.mjs
node --check web/settings/tabs/planner.js
```

Expected: the two render tests from Step 1 — the `planner.mode` one passes, the container one still fails on `planner-hedge-line` only (cleared in Task 3). `node --check` clean. If you prefer all-green commits, run only the first render test now via `node --test --test-name-pattern "planner.mode" web/settings/tabs/planner.test.mjs`.

- [ ] **Step 6: Commit**

```bash
git add web/settings/tabs/planner.js web/settings/tabs/planner.test.mjs
git commit -m "feat(settings): show live active strategy instead of the dead planner.mode dropdown (#479)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Correct k help text + live σ/hedge readout

**Files:**
- Modify: `web/settings/tabs/planner.js` (k field help, hedge-line element, `after` extension)

- [ ] **Step 1: Replace the k field help text**

In `render`, replace the existing PV-forecast-safety block:

```js
        '<div class="field-row"><div>' +
        field("PV forecast safety (k)", "planner.pv_forecast_safety_k", "number", 1.0,
          "How conservative the planner is about solar that might not arrive. It plans against forecast − k×σ, where σ is the live PV-forecast error. Higher k keeps more battery reserve on uncertain/cloudy days; 1.0 is the default; 0 = use the full battery (no hedge). On clear days and in winter the hedge is ~0 automatically.") +
        '</div></div>' +
```

with:

```js
        '<div class="field-row"><div>' +
        field("PV forecast safety (k)", "planner.pv_forecast_safety_k", "number", 1.0,
          "How much the planner trusts the solar forecast. It plans against forecast − k×σ, where σ is the live PV-forecast error. Higher k = trust the forecast less: the battery holds more reserve and charges earlier, drifting toward self-consumption behaviour. 0 = trust the forecast fully (no hedge). On clear, stable days σ shrinks toward zero and k has little effect — the hedge sizes itself to the real risk.") +
        '<div id="planner-hedge-line" style="display:none;color:var(--text-dim);font-size:0.8rem;margin-top:4px"></div>' +
        '</div></div>' +
```

- [ ] **Step 2: Extend the `after` hook with the live hedge line**

Append inside the `after` function from Task 2, after the active-strategy block:

```js
      // ---- Live σ/hedge readout under the k field ----
      var hedgeEl = document.getElementById("planner-hedge-line");
      var kInput = document.querySelector('input[data-path="planner.pv_forecast_safety_k"]');
      if (hedgeEl && kInput) {
        ownerFetch("/api/pvmodel")
          .then(function (r) { return r.json(); })
          .then(function (d) {
            if (!d || d.enabled === false) return; // pvmodel off → line stays hidden
            var sigma = d.pv_residual_std_w;
            function update() {
              var text = hedgeLine(kInput.value, sigma);
              if (text == null) return;
              hedgeEl.textContent = text;
              hedgeEl.style.display = "";
            }
            update();
            kInput.addEventListener("input", update);
          })
          .catch(function () {}); // unreachable → line stays hidden
      }
```

- [ ] **Step 3: Run the full test file + syntax check**

```bash
node --test web/settings/tabs/planner.test.mjs
node --check web/settings/tabs/planner.js
```

Expected: ALL tests pass now (including the `planner-hedge-line` container assertion from Task 2). `node --check` clean.

- [ ] **Step 4: Commit**

```bash
git add web/settings/tabs/planner.js
git commit -m "fix(settings): correct PV forecast safety (k) mental model + live sigma/hedge readout (#479)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: Cache-bust, changeset, verification, PR

**Files:**
- Modify: `web/index.html:778` (script version query)
- Create: `.changeset/settings-planner-active-strategy.md`

- [ ] **Step 1: Bump the script cache-bust tag**

In `web/index.html` line 778, change:

```html
  <script src="/settings/tabs/planner.js?v=pvsafetyk"></script>
```

to:

```html
  <script src="/settings/tabs/planner.js?v=strategy479"></script>
```

- [ ] **Step 2: Write the changeset**

Create `.changeset/settings-planner-active-strategy.md`:

```md
---
"forty-two-watts": patch
---

Settings → Planner: the Mode dropdown is gone — it edited a config field that the runtime strategy (set from the dashboard Plan card) overrides, so it showed stale values and confused operators. In its place a read-only "Active strategy" row shows what the planner is actually running. The "PV forecast safety (k)" help text now explains the real mechanism (plans against forecast − k·σ; higher k holds more reserve and charges earlier, it never forces charging), with a live σ/hedge readout under the field.
```

- [ ] **Step 3: Full verification**

```bash
node --test web/settings/tabs/planner.test.mjs
node --check web/settings/tabs/planner.js
node --check web/settings.js
git status --short   # only the intended files
```

Expected: tests pass, syntax clean, working tree contains exactly the planned files.

- [ ] **Step 4: Manual visual pass (dev server)**

```bash
cd /Users/fredde/repositories/forty-two-watts/.claude/worktrees/479-settings-planner-ui
make dev
```

Open `http://localhost:8080`, Settings → Planner, and check:
1. "Active strategy" shows a label (with sims it's whatever mode the dev instance runs), not `—`.
2. No Mode dropdown.
3. The hedge line appears under the k field once `/api/pvmodel` has data (on a fresh dev instance σ may be absent → line hidden, which is correct; verify the element exists in the DOM inspector).
4. Type a new k value → the hedge number updates live (when the line is visible).
5. Save → confirm `planner.mode` is untouched in the dev config YAML and no restart-required reason mentions the planner.

Stop the dev server afterwards. If no display is available, note in the PR that the visual pass ran via tests only.

- [ ] **Step 5: Commit + push + PR**

```bash
git add web/index.html .changeset/settings-planner-active-strategy.md
git commit -m "chore(settings): cache-bust planner tab + changeset (#479)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
git push -u origin 479-settings-planner-ui
gh pr create --title "fix(settings): live active-strategy display + PV-safety (k) clarity" --body "$(cat <<'EOF'
Closes #479.

## Why

A field operator ran `planner_passive_arbitrage` (picked on the dashboard Plan card) while Settings → Planner claimed `self_consumption`, and concluded the system had failed to charge the batteries — it was executing passive arbitrage correctly. Two UI problems caused this:

1. The **Mode dropdown** edits config-YAML `planner.mode`, which requires a restart AND is overridden by the runtime mode persisted in the state DB. A dead knob that shows stale values.
2. **"PV forecast safety (k)"** reads as "charge safely". The real mechanism is a downside hedge (plan against forecast − k·σ).

## What

- The Mode dropdown is replaced by a read-only **Active strategy** row, filled from `GET /api/status` in the tab's `after` hook. Labels prefer the `GET /api/modes` catalog (#468) when the host has it, with a local fallback — no fifth hard-coded mode list. Non-planner modes render as "… (manual — planner not dispatching)". The YAML field stays as the first-boot default, just no longer exposed.
- The k field gets a corrected help text (higher k = trust the forecast less → more reserve, charges earlier; never forces charging) and a live readout under the field: `σ right now ≈ 432 W → hedge = k·σ ≈ 432 W`, from `GET /api/pvmodel`, recomputed as you type.
- Pure helpers (`strategyLabel`, `hedgeLine`) covered by `web/settings/tabs/planner.test.mjs` (`node --test`).

Spec: `docs/superpowers/specs/2026-06-10-settings-planner-active-strategy-and-k-design.md`

## Verification

- `node --test web/settings/tabs/planner.test.mjs` — pass
- `node --check` on the touched JS — clean
- Manual pass against the dev server (see plan Task 4)

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Expected: PR created against `master`, changeset-check green (changeset present), `go test + vet` green (no Go changes).

---

## Self-Review

- **Spec coverage:** dropdown removal + active-strategy row (Task 2), catalog-preferring labels with fallback + manual suffix (Tasks 1–2), k help text + live σ line with all edge cases (Tasks 1, 3), styling via tokens/inline pattern matching the existing tab (Tasks 2–3), tests with window-stub dynamic import (Task 1), YAML field untouched on save (verified in Task 4 Step 4.5), changeset patch + English PR referencing #479 (Task 4). No gaps.
- **Placeholder scan:** none — every step has concrete code/commands.
- **Type consistency:** `strategyLabel(mode, catalog)` and `hedgeLine(k, sigmaW)` used identically in tests (Task 1), render/after (Tasks 2–3); element ids `planner-active-strategy` / `planner-hedge-line` consistent across tasks.
