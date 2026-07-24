# Terminal Native Theme Unification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Unify every FTW web surface around the approved neutral Terminal Native palette, make Flow the default Power now view, reset mobile destination scroll position, and ship the verified branch to the user's Raspberry Pi for real-data review.

**Architecture:** `web/components/theme.css` remains the single palette authority. A compatibility bridge maps the old light-DOM tokens onto canonical roles, while targeted CSS and shadow-DOM edits remove colors that bypass the tokens. A tiny shared runtime resolves CSS colors for canvas APIs, while the two behavior changes stay isolated in `power-now.js` and a new testable mobile-navigation module.

**Tech Stack:** HTML, CSS custom properties, browser-native JavaScript/Web Components, Canvas 2D, Node's built-in test runner, Go cross-compilation, Docker Compose on Raspberry Pi.

## Global Constraints

- Preserve the existing information architecture, API calls, polling paths, setup workflow, planner behavior, driver behavior, power signs, and safety behavior.
- Dark roles are Canvas `#0d0d0d`, Recessed `#101010`, Panel `#161616`, Raised `#1e1e1e`, Border `#2a2a2a`, Primary `#e8e8e8`, Secondary `#a0a0a0`, and Muted `#858585`.
- Light roles are Canvas `#f4f4f2`, Recessed `#ecece8`, Panel `#fafaf8`, Raised `#ffffff`, Border `#cecec7`, Primary `#191919`, Secondary `#4f4f4b`, and Muted `#686862`.
- Amber is interaction/current-time chrome; green, red, cyan, and violet remain semantic data/status colors.
- Amber-filled controls use `--on-accent: #0a0a0a`.
- Hard-coded series colors may remain only when they encode data or a physical 3D scene; surfaces, borders, labels, tooltips, empty states, menus, and controls use tokens.
- Keep the logo as the sole FTW header brand mark.
- Flow is first and default; explicit stored Values preferences remain compatible through legacy `numbers`/`hero` values.
- Only a user click that changes a bottom-mobile destination resets scroll to top.
- `/setup` keeps its current layout and behavior.
- Add a patch changeset; do not edit `package.json` version or `CHANGELOG.md`.
- Preserve all unrelated dirty worktree files and do not push or open a pull request.
- Deployment must use a user-provided `user@192.168.1.139` target, the detected existing Compose project, the existing `/app/data` bind, atomic temp-name plus `mv` binary replacement, and service-specific `docker compose up -d`; never use `down`.

---

## File Map

- Create `web/theme-unification.test.mjs`: static contract tests for palette aliases, chrome cleanup, header branding, setup inheritance, component fallbacks, and canvas theme integration.
- Create `web/theme-runtime.js`: ES module that exports a testable resolver and exposes the browser instance as `window.ftwThemeColors`.
- Create `web/theme-runtime.test.mjs`: unit tests for theme color resolution without a browser DOM.
- Create `web/mobile-navigation.js`: testable controller for user-initiated mobile destination scroll resets.
- Create `web/mobile-navigation.test.mjs`: unit tests for changed destination, active destination, and non-mobile clicks.
- Modify `web/components/theme.css`: canonical neutral dark/light palette, elevated surface, compatibility aliases, and on-accent token.
- Modify `web/app.css`: flat page/header/mobile chrome and neutral active/history states.
- Modify `web/style.css`: token-based history, diagnostics, settings, help, modal, and setup-shared chrome.
- Modify `web/index.html`: logo-only header, Flow-first Power now markup, new runtime/module scripts, and theme-change event.
- Modify `web/power-now.js`: Flow-safe preference normalization and fallback.
- Modify `web/power-now.test.mjs`: Flow-default and preference compatibility tests.
- Modify `web/dashboard-simplification.test.mjs`: Flow-first markup and default accessibility contract.
- Modify `web/energy-history.js`, `web/diagnose.js`, and `web/plan.js`: theme-aware Canvas chrome and redraw on theme changes.
- Audit every component registered by `web/components/index.js` plus the lazily loaded `web/components/ftw-pv-arrays-3d.js`; modify `web/components/ftw-energy-flow.js`, `web/components/ftw-pv-arrays-3d.js`, `web/components/ftw-savings-card.js`, `web/components/ftw-notif-status.js`, `web/components/ftw-notif-history.js`, and `web/components/ftw-notif-test-button.js` where the audit finds bypass colors.
- Modify `web/update-badge.js`, `web/settings/tabs/devices.js`, `web/settings/tabs/weather.js`, and `web/settings/tabs/system.js`: neutral fallback surfaces and canonical semantic tokens.
- Create `.changeset/unify-terminal-native-theme.md`: patch release note for the user-visible web changes.

---

### Task 1: Canonical Palette and Compatibility Bridge

**Files:**
- Create: `web/theme-unification.test.mjs`
- Modify: `web/components/theme.css`

**Interfaces:**
- Consumes: the existing `--ink*`, `--line*`, `--fg*`, and semantic custom-property names.
- Produces: `--ink-elevated`, `--on-accent`, and exact legacy aliases consumed by all later tasks.

- [ ] **Step 1: Write the failing palette contract**

Create `web/theme-unification.test.mjs` with:

```js
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const webRoot = dirname(fileURLToPath(import.meta.url));
const read = (path) => readFileSync(join(webRoot, path), "utf8");
const theme = read("components/theme.css");
const appCss = read("app.css");
const styleCss = read("style.css");
const html = read("index.html");
const setup = read("setup.html");

describe("terminal-native palette", () => {
  it("defines the approved neutral dark and light roles", () => {
    for (const value of [
      "#0d0d0d", "#101010", "#161616", "#1e1e1e", "#2a2a2a",
      "#e8e8e8", "#a0a0a0", "#858585",
      "#f4f4f2", "#ecece8", "#fafaf8", "#ffffff", "#cecec7",
      "#191919", "#4f4f4b", "#686862",
    ]) assert.match(theme, new RegExp(value, "i"), value);
    assert.match(theme, /--on-accent:\s*#0a0a0a/i);
  });

  it("bridges every legacy role to the canonical palette", () => {
    const aliases = {
      bg: "ink",
      surface: "ink-raised",
      surface2: "ink-sunken",
      border: "line",
      text: "fg",
      "text-dim": "fg-dim",
      green: "green-e",
      red: "red-e",
      yellow: "amber",
      blue: "cyan",
      accent: "accent-e",
    };
    for (const [legacy, canonical] of Object.entries(aliases)) {
      assert.match(theme, new RegExp(`--${legacy}:\\s*var\\(--${canonical}\\)`));
    }
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
node --test web/theme-unification.test.mjs
```

Expected: FAIL because the approved hex roles, `--on-accent`, and compatibility aliases are absent.

- [ ] **Step 3: Implement the neutral role system**

In the dark `:root` block of `web/components/theme.css`, define the canonical surfaces before the aliases:

```css
--ink:          #0d0d0d;
--ink-sunken:   #101010;
--ink-raised:   #161616;
--ink-elevated: #1e1e1e;
--line:         #2a2a2a;
--line-soft:    #222222;
--fg:           #e8e8e8;
--fg-dim:       #a0a0a0;
--fg-muted:     #858585;
--fg-label:     #d8d8d8;
--on-accent:    #0a0a0a;

--bg:       var(--ink);
--surface:  var(--ink-raised);
--surface2: var(--ink-sunken);
--border:   var(--line);
--text:     var(--fg);
--text-dim: var(--fg-dim);
--green:    var(--green-e);
--red:      var(--red-e);
--yellow:   var(--amber);
--blue:     var(--cyan);
--accent:   var(--accent-e);
```

In `html[data-theme="light"]`, override the same canonical surface roles:

```css
--ink:          #f4f4f2;
--ink-sunken:   #ecece8;
--ink-raised:   #fafaf8;
--ink-elevated: #ffffff;
--line:         #cecec7;
--line-soft:    #ddddD7;
--fg:           #191919;
--fg-dim:       #4f4f4b;
--fg-muted:     #686862;
--fg-label:     #4f4f4b;
```

Replace blue-tinted hero surface tokens with neutral equivalents while leaving semantic flow colors intact:

```css
--hero-bg-top:       #1a1a1a;
--hero-bg-bot:       #111111;
--hero-line-base:    #2a2a2a;
--hero-box-fill:     rgb(22 22 22 / 95%);
--hero-box-border:   #303030;
--hero-house-fill:   #1c1c1c;
--hero-label-text:   #d8d8d8;
--hero-sub-text:     #b8b8b8;
--hero-soc-track:    #2a2a2a;
```

Use these exact light hero values:

```css
--hero-bg-top:       #fafaf8;
--hero-bg-bot:       #ecece8;
--hero-line-base:    #cecec7;
--hero-box-fill:     rgb(255 255 255 / 96%);
--hero-box-border:   #cecec7;
--hero-house-fill:   #fafaf8;
--hero-label-text:   #4f4f4b;
--hero-sub-text:     #686862;
--hero-soc-track:    #ddddD7;
```

- [ ] **Step 4: Run the focused test**

Run:

```bash
node --test web/theme-unification.test.mjs
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/components/theme.css web/theme-unification.test.mjs
git commit -m "style(ui): unify terminal theme tokens"
```

---

### Task 2: Flat Dashboard, History, Settings, and Setup Chrome

**Files:**
- Modify: `web/app.css`
- Modify: `web/style.css`
- Modify: `web/index.html`
- Modify: `web/theme-unification.test.mjs`

**Interfaces:**
- Consumes: `--ink`, `--ink-raised`, `--ink-elevated`, `--ink-sunken`, `--line`, `--fg*`, `--accent-e`, and `--on-accent`.
- Produces: logo-only branding and neutral light-DOM chrome inherited by the dashboard and `/setup`.

- [ ] **Step 1: Add failing chrome and branding tests**

Append to `web/theme-unification.test.mjs`:

```js
describe("terminal-native light DOM chrome", () => {
  it("uses one accessible FTW brand mark", () => {
    assert.equal((html.match(/class="header-logo"/g) || []).length, 1);
    assert.match(html, /<img[^>]+alt="FTW"[^>]+class="header-logo"/);
    assert.doesNotMatch(html, /<h1>\s*FTW\s*<\/h1>/);
  });

  it("uses flat token surfaces for page, header, and mobile destinations", () => {
    assert.doesNotMatch(appCss, /body\.ftw-app::before[\s\S]*?radial-gradient/);
    assert.doesNotMatch(appCss, /body\.ftw-app > header::before[\s\S]*?backdrop-filter/);
    assert.match(appCss, /body\.ftw-app > header\s*\{[\s\S]*?background:\s*var\(--ink-elevated\)/);
    assert.match(appCss, /\.mobile-destinations\s*\{[\s\S]*?background:\s*var\(--ink-elevated\)/);
  });

  it("uses amber instead of blue for selected interface chrome", () => {
    assert.match(styleCss, /\.diag-row\.active\s*\{[\s\S]*?var\(--accent-e\)/);
    assert.match(styleCss, /\.modal-tabs button\.active\s*\{[\s\S]*?var\(--accent-e\)/);
    assert.doesNotMatch(styleCss, /\.diag-row\.active\s*\{[\s\S]*?#60a5fa/);
  });

  it("loads canonical tokens before shared setup styles", () => {
    assert.ok(
      setup.indexOf("/components/theme.css") < setup.indexOf("/style.css"),
      "setup must load theme.css before style.css",
    );
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
node --test web/theme-unification.test.mjs
```

Expected: FAIL on the duplicate FTW heading, gradients/blur, and blue selection rules.

- [ ] **Step 3: Flatten dashboard chrome and remove duplicate branding**

In `web/index.html`, leave only:

```html
<img src="/logo.svg" alt="FTW" class="header-logo">
```

In `web/app.css`:

```css
body.ftw-app {
  background: var(--ink);
}

body.ftw-app > header {
  background: var(--ink-elevated);
  border: 1px solid var(--line);
}

body.ftw-app .tab-btn:hover {
  color: var(--fg);
  background: var(--ink-sunken);
}

@media (max-width: 720px) {
  body.ftw-app .mobile-destinations {
    background: var(--ink-elevated);
    backdrop-filter: none;
    -webkit-backdrop-filter: none;
  }
}
```

Delete the complete `body.ftw-app::before`, `body.ftw-app > header::before`, light-theme gradient/pseudo-element overrides, `header h1` rules, and the light-theme transparent header border override. Keep the existing header sizing, grid, radii, margins, and responsive anchors.

- [ ] **Step 4: Normalize legacy active and recessed states**

Apply these exact role changes in `web/style.css`:

```css
.diag-row:hover,
.diag-table tbody tr:hover {
  background: color-mix(in srgb, var(--fg) 4%, transparent);
}
.diag-row.active {
  background: color-mix(in srgb, var(--accent-e) 14%, transparent);
  border-left: 4px solid var(--accent-e);
  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--accent-e) 25%, transparent);
}
.diag-chart-highlight,
.diag-table tbody tr.diag-row-hover {
  background: color-mix(in srgb, var(--accent-e) 12%, transparent);
}
.diag-chart-highlight {
  border-inline: 1px solid color-mix(in srgb, var(--accent-e) 65%, transparent);
}
.range-buttons button.active,
.view-buttons button.active {
  background: var(--accent-e);
  border-color: var(--accent-e);
  color: var(--on-accent);
}
.modal-tabs button.active {
  color: var(--fg);
  border-bottom-color: var(--accent-e);
}
.help {
  background: color-mix(in srgb, var(--cyan) 16%, transparent);
  color: var(--cyan);
}
.help:hover::after {
  background: var(--ink-elevated);
  border: 1px solid var(--line);
  color: var(--fg);
}
```

Keep export fuse colors blue/cyan because they encode power direction, and keep green/red/amber diagnostic reason colors because they encode reason/status.

- [ ] **Step 5: Run the focused tests**

Run:

```bash
node --test web/theme-unification.test.mjs web/dashboard-simplification.test.mjs
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add web/app.css web/style.css web/index.html web/theme-unification.test.mjs
git commit -m "style(ui): flatten dashboard chrome"
```

---

### Task 3: Shadow-DOM Component and Settings Fallback Audit

**Files:**
- Modify: `web/update-badge.js`
- Modify: `web/components/ftw-savings-card.js`
- Modify: `web/components/ftw-notif-status.js`
- Modify: `web/components/ftw-notif-history.js`
- Modify: `web/components/ftw-notif-test-button.js`
- Modify: `web/components/ftw-energy-flow.js`
- Modify: `web/components/ftw-pv-arrays-3d.js`
- Modify: `web/settings/tabs/devices.js`
- Modify: `web/settings/tabs/weather.js`
- Modify: `web/settings/tabs/system.js`
- Modify: `web/theme-unification.test.mjs`

**Interfaces:**
- Consumes: canonical theme tokens from Task 1.
- Produces: neutral fallback chrome in independently styled/shadow-DOM components.

- [ ] **Step 1: Add a failing audited-fallback test**

Append:

```js
describe("component fallback palette", () => {
  const registeredComponents = [
    "components/ftw-modal.js",
    "components/ftw-progress-bar.js",
    "components/ftw-badge.js",
    "components/ftw-card.js",
    "components/ftw-tabs.js",
    "components/ftw-legend.js",
    "components/ftw-energy-flow.js",
    "components/ftw-battery-control.js",
    "components/ftw-pv-control.js",
    "components/ftw-price-chart.js",
    "components/ftw-energy-cake.js",
    "components/ftw-bar-chart.js",
    "components/ftw-history-card.js",
    "components/ftw-savings-card.js",
    "components/ftw-update-check.js",
    "components/ftw-notif-status.js",
    "components/ftw-notif-test-button.js",
    "components/ftw-notif-history.js",
    "components/ftw-pv-arrays-3d.js",
  ];
  const audited = [
    ...registeredComponents,
    "update-badge.js",
    "settings/tabs/devices.js",
    "settings/tabs/weather.js",
    "settings/tabs/system.js",
  ];

  it("contains no legacy blue-slate chrome fallback", () => {
    const disallowed = /#(?:0f172a|1e293b|334155|94a3b8|3b82f6|375a8f|6cf)\b/ig;
    for (const path of audited) {
      assert.deepEqual(read(path).match(disallowed) || [], [], path);
    }
  });

  it("uses the shared on-accent token", () => {
    assert.doesNotMatch(read("update-badge.js"), /color:\s*#0a0a0a/);
    assert.match(read("update-badge.js"), /color:\s*var\(--on-accent,\s*#0a0a0a\)/);
    assert.match(read("components/ftw-notif-test-button.js"), /var\(--on-accent/);
  });

  it("keeps every registered component in the explicit theme audit", () => {
    const registry = read("components/index.js");
    for (const path of registeredComponents.filter((path) => !path.endsWith("ftw-pv-arrays-3d.js"))) {
      assert.match(registry, new RegExp(path.split("/").at(-1).replace(".", "\\.")), path);
    }
    assert.match(read("settings/tabs/weather.js"), /ftw-pv-arrays-3d\.js/);
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
node --test web/theme-unification.test.mjs
```

Expected: FAIL and name each component that still contains a blue/slate chrome fallback.

- [ ] **Step 3: Replace chrome fallbacks with neutral/token equivalents**

Use this exact replacement matrix only in the audited files:

```text
var(--ink-raised, #1e293b)  -> var(--ink-raised, #161616)
var(--ink-sunken, #0f172a)  -> var(--ink-sunken, #101010)
var(--line, #334155)        -> var(--line, #2a2a2a)
var(--fg, #e2e8f0)          -> var(--fg, #e8e8e8)
var(--fg-dim, #94a3b8)      -> var(--fg-dim, #a0a0a0)
var(--text-dim, #aaa)       -> var(--fg-dim, #a0a0a0)
var(--text-dim, #888)       -> var(--fg-muted, #858585)
color: #0a0a0a              -> color: var(--on-accent, #0a0a0a)
var(--accent, #375a8f)      -> var(--accent-e, #f5b942)
var(--accent, #6cf)         -> var(--accent-e, #f5b942)
background: #1e293b         -> background: var(--ink-sunken, #101010)
```

For `ftw-notif-test-button`, make the primary button:

```css
button {
  background: var(--accent-e, #f5b942);
  color: var(--on-accent, #0a0a0a);
  border: 1px solid var(--accent-e, #f5b942);
}
```

For `ftw-savings-card`, change `.spark-tip .muted` to:

```css
color: var(--fg-muted, #858585);
```

For `ftw-energy-flow`, change the focus-ring fallback to:

```css
filter: drop-shadow(0 0 4px var(--accent-e, #f5b942));
```

For `ftw-pv-arrays-3d`, add:

```js
function themeColor(name, fallback) {
  const value = getComputedStyle(document.documentElement)
    .getPropertyValue(name)
    .trim();
  return value || fallback;
}
```

and create each panel-name sprite with canonical label chrome:

```js
const sprite = makeLabelSprite(p.name, {
  color: themeColor("--on-accent", "#0a0a0a"),
  bgColor: themeColor("--accent-e", "#f5b942"),
  canvasSize: 72,
});
```

Keep the ground, panel material, compass direction, and north-stripe colors because they draw the physical/data scene.

For Settings device verification statuses, replace `#2a7` with `var(--green-e, #38b978)` and `#c44` with `var(--red-e, #dc5b64)`. These remain semantic, not surface chrome.

- [ ] **Step 4: Re-run the focused tests**

Run:

```bash
node --test web/theme-unification.test.mjs web/settings/tabs/system.test.mjs
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/update-badge.js web/components/ftw-energy-flow.js web/components/ftw-pv-arrays-3d.js web/components/ftw-savings-card.js web/components/ftw-notif-status.js web/components/ftw-notif-history.js web/components/ftw-notif-test-button.js web/settings/tabs/devices.js web/settings/tabs/weather.js web/settings/tabs/system.js web/theme-unification.test.mjs
git commit -m "style(ui): normalize component chrome"
```

---

### Task 4: Theme-Aware Canvas Chrome

**Files:**
- Create: `web/theme-runtime.js`
- Create: `web/theme-runtime.test.mjs`
- Modify: `web/index.html`
- Modify: `web/energy-history.js`
- Modify: `web/diagnose.js`
- Modify: `web/plan.js`
- Modify: `web/theme-unification.test.mjs`

**Interfaces:**
- Consumes: `window.getComputedStyle`, `document.documentElement`, and canonical CSS custom properties.
- Produces: `globalThis.ftwThemeColors.resolve(name, fallback)` and `globalThis.ftwThemeColors.palette()`.

- [ ] **Step 1: Write failing runtime tests**

Create `web/theme-runtime.test.mjs`:

```js
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { createThemeColors } from "./theme-runtime.js";

describe("theme canvas colors", () => {
  it("resolves canonical properties and falls back cleanly", () => {
    const values = new Map([["--fg", "rgb(232, 232, 232)"]]);
    const colors = createThemeColors((name) => values.get(name) || "");
    assert.equal(colors.resolve("--fg", "#fff"), "rgb(232, 232, 232)");
    assert.equal(colors.resolve("--missing", "#858585"), "#858585");
  });

  it("builds all neutral canvas chrome roles", () => {
    const colors = createThemeColors(() => "");
    assert.deepEqual(colors.palette(), {
      text: "#e8e8e8",
      dim: "#a0a0a0",
      muted: "#858585",
      line: "#2a2a2a",
      panel: "#161616",
      accent: "#f5b942",
    });
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
node --test web/theme-runtime.test.mjs
```

Expected: FAIL because `theme-runtime.js` does not exist.

- [ ] **Step 3: Add the shared resolver**

Create `web/theme-runtime.js`:

```js
export function createThemeColors(readProperty) {
  const resolve = (name, fallback) => readProperty(name).trim() || fallback;
  return {
    resolve,
    palette() {
      return {
        text: resolve("--fg", "#e8e8e8"),
        dim: resolve("--fg-dim", "#a0a0a0"),
        muted: resolve("--fg-muted", "#858585"),
        line: resolve("--line", "#2a2a2a"),
        panel: resolve("--ink-raised", "#161616"),
        accent: resolve("--accent-e", "#f5b942"),
      };
    },
  };
}

if (typeof window !== "undefined" && typeof document !== "undefined") {
  const style = () => getComputedStyle(document.documentElement);
  window.ftwThemeColors = createThemeColors(
    (name) => style().getPropertyValue(name),
  );
}
```

Load it before chart scripts:

```html
<script type="module" src="/theme-runtime.js?v=theme1"></script>
```

- [ ] **Step 4: Move Canvas chrome to the resolver**

In each draw function, resolve once per draw:

```js
const C = window.ftwThemeColors
  ? window.ftwThemeColors.palette()
  : {
      text: "#e8e8e8",
      dim: "#a0a0a0",
      muted: "#858585",
      line: "#2a2a2a",
      panel: "#161616",
      accent: "#f5b942",
    };
```

For `energy-history.js`, use:

```js
if (!points.length) {
  ctx.fillStyle = C.muted;
  ctx.font = "13px system-ui, sans-serif";
  ctx.fillText("No energy recorded in this range.", 16, 28);
  return;
}
const textColor = C.dim;
const borderColor = C.line;
```

For `diagnose.js`, use:

```js
ctx.save();
ctx.globalAlpha = 0.03;
ctx.fillStyle = C.text;
ctx.fillRect(pad.l, priceY0, plotW, priceH);
ctx.fillRect(pad.l, socY0, plotW, socH);
ctx.restore();

ctx.strokeStyle = C.line;
ctx.fillStyle = C.dim;
```

Use those last two assignments for the power zero line and Y-axis labels respectively. Keep price, grid, PV, battery, EV, and SoC series colors unchanged.

For `plan.js`, replace each white-alpha grid/axis assignment with:

```js
ctx.strokeStyle = C.line;
ctx.fillStyle = C.dim;
```

Use `C.muted` for secondary axis captions, `C.panel` for tooltip surfaces, and create the hover line with:

```js
hoverLine.style.cssText =
  "position:absolute;top:0;width:1px;height:100%;" +
  "background:var(--line);" +
  "border-left:1px dashed var(--fg-muted);" +
  "pointer-events:none;display:none;z-index:2";
```

Do not replace price, grid, PV, battery, EV, SoC, or flow series colors.

In the theme toggle's `apply(theme)` function in `web/index.html`, dispatch after setting the attribute:

```js
window.dispatchEvent(new CustomEvent("ftw-theme-change", {
  detail: { theme: theme },
}));
```

Add redraw listeners:

```js
window.addEventListener("ftw-theme-change", render);
```

for `plan.js`;

```js
window.addEventListener("ftw-theme-change", function () {
  if (lastData) drawChart(lastData);
});
```

for `energy-history.js`; and:

```js
window.addEventListener("ftw-theme-change", function () {
  if (state.detail) drawChart(state.detail);
});
```

for `diagnose.js`.

- [ ] **Step 5: Add source-contract coverage**

Append:

```js
describe("canvas theme integration", () => {
  it("loads the resolver and redraws themed canvases", () => {
    assert.match(html, /src="\/theme-runtime\.js\?v=theme1"/);
    for (const path of ["energy-history.js", "diagnose.js", "plan.js"]) {
      const source = read(path);
      assert.match(source, /ftwThemeColors/);
      assert.match(source, /ftw-theme-change/);
    }
  });
});
```

- [ ] **Step 6: Run the focused tests**

Run:

```bash
node --test web/theme-runtime.test.mjs web/theme-unification.test.mjs
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add web/theme-runtime.js web/theme-runtime.test.mjs web/index.html web/energy-history.js web/diagnose.js web/plan.js web/theme-unification.test.mjs
git commit -m "style(ui): theme canvas chrome"
```

---

### Task 5: Flow-First Power Now

**Files:**
- Modify: `web/power-now.js`
- Modify: `web/power-now.test.mjs`
- Modify: `web/index.html`
- Modify: `web/dashboard-simplification.test.mjs`

**Interfaces:**
- Consumes: storage key `ftw-hero-mode`.
- Produces: `normalizePowerNowMode(storedValue)` returning `"flow"` for missing/unknown/`hero`/`flow`, and `"values"` for `numbers`/`values`.

- [ ] **Step 1: Change tests to the approved compatibility table**

In `web/power-now.test.mjs`, use:

```js
for (const [stored, expected] of [
  [null, "flow"],
  ["numbers", "values"],
  ["values", "values"],
  ["hero", "flow"],
  ["flow", "flow"],
  ["corrupt", "flow"],
]) {
  assert.equal(normalizePowerNowMode(stored), expected);
}
```

Rename the storage-unavailable case to `"defaults to Flow and remains interactive when storage is unavailable"` and assert Flow is selected before an ArrowRight moves focus to Values.

In `web/dashboard-simplification.test.mjs`, replace the Values-first regexes with:

```js
assert.ok(
  overview.indexOf('id="power-now-tab-flow"') <
    overview.indexOf('id="power-now-tab-values"'),
);
assert.match(overview, /id="power-now-tab-flow"[^>]*aria-selected="true"/);
assert.match(overview, /id="power-now-flow"[^>]*role="tabpanel"(?![^>]*hidden)/);
assert.match(overview, /id="power-now-values"[^>]*role="tabpanel"[^>]*hidden/);
```

- [ ] **Step 2: Run tests to verify they fail**

Run:

```bash
node --test web/power-now.test.mjs web/dashboard-simplification.test.mjs
```

Expected: FAIL because the controller and markup still default to Values.

- [ ] **Step 3: Implement the Flow-safe mapping**

In `web/power-now.js`:

```js
export function normalizePowerNowMode(storedValue) {
  return storedValue === "numbers" || storedValue === "values"
    ? "values"
    : "flow";
}
```

Inside `apply`, change the invalid requested-mode fallback:

```js
const mode = MODES.includes(requestedMode) ? requestedMode : "flow";
```

Order modes consistently:

```js
const MODES = ["flow", "values"];
```

In `web/index.html`, order the Flow tab/panel first, give Flow `aria-selected="true"`, give Values `aria-selected="false" tabindex="-1"`, and put `hidden` on the Values panel instead of the Flow panel.

- [ ] **Step 4: Run tests**

Run:

```bash
node --test web/power-now.test.mjs web/dashboard-simplification.test.mjs
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/power-now.js web/power-now.test.mjs web/index.html web/dashboard-simplification.test.mjs
git commit -m "feat(ui): default power now to flow"
```

---

### Task 6: Mobile Destination Scroll Reset

**Files:**
- Create: `web/mobile-navigation.js`
- Create: `web/mobile-navigation.test.mjs`
- Modify: `web/index.html`

**Interfaces:**
- Consumes: `#mobile-destinations`, `button.app-nav-btn[data-view]`, and `document.body.dataset.view`.
- Produces: `initMobileDestinationScroll(root, viewport)` returning a cleanup function.

- [ ] **Step 1: Write failing behavior tests**

Create `web/mobile-navigation.test.mjs`:

```js
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { shouldResetMobileScroll } from "./mobile-navigation.js";

describe("mobile destination scroll reset", () => {
  it("resets only when a mobile click changes destination", () => {
    assert.equal(shouldResetMobileScroll("mobile-destinations", "overview", "energy"), true);
    assert.equal(shouldResetMobileScroll("mobile-destinations", "energy", "energy"), false);
    assert.equal(shouldResetMobileScroll("app-tabs", "overview", "energy"), false);
    assert.equal(shouldResetMobileScroll("mobile-destinations", "overview", ""), false);
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```bash
node --test web/mobile-navigation.test.mjs
```

Expected: FAIL because the module does not exist.

- [ ] **Step 3: Implement the isolated controller**

Create `web/mobile-navigation.js`:

```js
export function shouldResetMobileScroll(sourceId, currentView, nextView) {
  return sourceId === "mobile-destinations" &&
    Boolean(nextView) &&
    currentView !== nextView;
}

export function initMobileDestinationScroll(root = document, viewport = window) {
  const nav = root.getElementById("mobile-destinations");
  if (!nav) return () => {};
  let pendingView = "";

  const onClick = (event) => {
    const button = event.target.closest(".app-nav-btn[data-view]");
    if (!button || !nav.contains(button)) return;
    const currentView = root.body && root.body.dataset.view;
    const nextView = button.dataset.view;
    if (!shouldResetMobileScroll(nav.id, currentView, nextView)) return;
    pendingView = nextView;
  };

  const onHashChange = () => {
    if (!pendingView || !root.body || root.body.dataset.view !== pendingView) return;
    pendingView = "";
    viewport.requestAnimationFrame(() => {
      viewport.scrollTo({ top: 0, left: 0, behavior: "auto" });
    });
  };

  nav.addEventListener("click", onClick);
  viewport.addEventListener("hashchange", onHashChange);
  return () => {
    nav.removeEventListener("click", onClick);
    viewport.removeEventListener("hashchange", onHashChange);
  };
}

if (typeof document !== "undefined") {
  const start = () => initMobileDestinationScroll(document, window);
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start, { once: true });
  } else {
    start();
  }
}
```

Load it after `diagnose.js` so the existing router handles the hash/view in the same click turn:

```html
<script type="module" src="/mobile-navigation.js?v=scroll1"></script>
```

- [ ] **Step 4: Run focused tests**

Run:

```bash
node --test web/mobile-navigation.test.mjs web/dashboard-simplification.test.mjs
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/mobile-navigation.js web/mobile-navigation.test.mjs web/index.html
git commit -m "fix(ui): reset mobile destination scroll"
```

---

### Task 7: Changeset and Local Verification

**Files:**
- Create: `.changeset/unify-terminal-native-theme.md`
- If verification exposes an in-scope regression, modify only the exact file from Tasks 1–6 that causes that regression and add a focused regression assertion before the fix.

**Interfaces:**
- Consumes: the completed local implementation.
- Produces: a patch release note and evidence that tests, build, and browser interactions pass.

- [ ] **Step 1: Add the patch changeset**

Create:

```markdown
---
"ftw": patch
---

Unify dashboard, setup, history, settings, and component chrome around the neutral terminal-native theme, default Power now to Flow, and reset mobile destination scrolling.
```

- [ ] **Step 2: Run all web tests**

Run:

```bash
npm test
```

Expected: all `web/**/*.test.mjs` tests pass with zero failures.

- [ ] **Step 3: Run repository verification**

Run:

```bash
make verify
```

Expected: Go/Python tests, compose migration checks, vet, and builds all exit 0.

- [ ] **Step 4: Run local browser QA against the existing app**

At `http://127.0.0.1:8080`, verify desktop and 390 px mobile widths in dark and light:

```text
Overview: Flow fresh default; Values selection persists after reload
Energy: live chart and price chrome are neutral
Plan: chart labels/grid readable in both themes
History: Detailed records and Earlier decisions are neutral; active row amber
More: all action surfaces are neutral
Settings: every tab, active tab, input, and modal surface
Update Center and notification history: neutral fallback surfaces
Mobile drawer and five-destination bar: neutral, 44px+ targets
/setup: same theme roles, unchanged steps/layout
Mobile destination switch: new section starts at document top
Active mobile destination tap: current scroll position remains
Back/Forward and #diagnose/<timestamp>: no forced top reset
```

Record screenshots under `/private/tmp` only; do not add them to git.

- [ ] **Step 5: Build the ARM64 binary**

Run:

```bash
make build-arm64
file bin/forty-two-watts-linux-arm64
shasum -a 256 bin/forty-two-watts-linux-arm64
```

Expected: `bin/forty-two-watts-linux-arm64` is an ELF 64-bit ARM aarch64 executable and a local SHA-256 is recorded.

- [ ] **Step 6: Commit**

```bash
git add .changeset/unify-terminal-native-theme.md
git commit -m "chore: add theme unification changeset"
```

---

### Task 8: Deploy to `192.168.1.139` and Verify

**Files:**
- No repository files modified.
- Remote files may be backed up/updated only after the layout probe proves their identity.

**Interfaces:**
- Consumes: the user-provided full SSH target, `bin/forty-two-watts-linux-arm64`, detected install/service/binary aliases, saved development override, and existing `/app/data` bind.
- Produces: a running Pi development deployment with rollback artifacts and independently verified binary/dashboard revision.

- [ ] **Step 1: Obtain and validate the SSH target**

Require the user to provide the exact target in `user@192.168.1.139` form. The environment variable must be populated from that response before continuing. Validate it with:

```bash
: "${FTW_SSH_TARGET:?Set FTW_SSH_TARGET from the user's explicit response}"
case "$FTW_SSH_TARGET" in
  ?*@192.168.1.139) ;;
  *) echo "target must be user@192.168.1.139" >&2; exit 1 ;;
esac
```

Expected: the value contains one username, `@`, and the exact requested host. Do not infer the username from SSH config, prior hosts, or unrelated memory.

- [ ] **Step 2: Run the read-only layout and architecture probe**

Run:

```bash
ssh "$FTW_SSH_TARGET" 'set -eu
  uname -m
  if [ -d "$HOME/ftw" ]; then dir="$HOME/ftw"
  elif [ -d "$HOME/forty-two-watts" ]; then dir="$HOME/forty-two-watts"
  else echo "no FTW Compose directory found" >&2; exit 1
  fi
  cd "$dir"
  services="$(docker compose config --services)"
  count="$(printf "%s\n" "$services" | grep -Ec "^(ftw|forty-two-watts)$" || true)"
  [ "$count" -eq 1 ] || { echo "expected exactly one FTW main service" >&2; exit 1; }
  service="$(printf "%s\n" "$services" | grep -E "^(ftw|forty-two-watts)$")"
  docker compose config "$service" | grep -q "/app/data" ||
    { echo "main service does not map persistent /app/data" >&2; exit 1; }
  cid="$(docker compose ps -q "$service")"
  printf "dir=%s service=%s container=%s\n" "$dir" "$service" "$cid"
  docker inspect "$cid" --format "image={{.Config.Image}} mounts={{json .Mounts}}"
  for bin in "$HOME/ftw-dev/bin/ftw" "$HOME/ftw-dev/bin/forty-two-watts"; do
    if [ -e "$bin" ]; then printf "dev_binary=%s\n" "$bin"; file "$bin"; fi
  done
  ls -1 docker-compose.override.yml* 2>/dev/null || true'
```

Expected: `aarch64`/`arm64`, exactly one supported main service, a proven `/app/data` bind, one supported dev binary path or a saved override that proves it, and an unambiguous Compose directory. Stop on any ambiguity.

- [ ] **Step 3: Copy the binary atomically and preserve rollback**

Choose only the binary path proven by Step 2. For canonical `~/ftw-dev/bin/ftw`, run:

```bash
scp bin/forty-two-watts-linux-arm64 "$FTW_SSH_TARGET:~/ftw-dev/bin/ftw.new"
ssh "$FTW_SSH_TARGET" 'set -eu
  stamp="$(date +%Y%m%d-%H%M%S)"
  if [ -e "$HOME/ftw-dev/bin/ftw" ]; then
    cp "$HOME/ftw-dev/bin/ftw" "$HOME/ftw-dev/bin/ftw.$stamp.bak"
  fi
  mv -f "$HOME/ftw-dev/bin/ftw.new" "$HOME/ftw-dev/bin/ftw"
  chmod 755 "$HOME/ftw-dev/bin/ftw"
  file "$HOME/ftw-dev/bin/ftw"
  sha256sum "$HOME/ftw-dev/bin/ftw"'
```

For a proven legacy `~/ftw-dev/bin/forty-two-watts`, run:

```bash
scp bin/forty-two-watts-linux-arm64 "$FTW_SSH_TARGET:~/ftw-dev/bin/forty-two-watts.new"
ssh "$FTW_SSH_TARGET" 'set -eu
  stamp="$(date +%Y%m%d-%H%M%S)"
  if [ -e "$HOME/ftw-dev/bin/forty-two-watts" ]; then
    cp "$HOME/ftw-dev/bin/forty-two-watts" "$HOME/ftw-dev/bin/forty-two-watts.$stamp.bak"
  fi
  mv -f "$HOME/ftw-dev/bin/forty-two-watts.new" "$HOME/ftw-dev/bin/forty-two-watts"
  chmod 755 "$HOME/ftw-dev/bin/forty-two-watts"
  file "$HOME/ftw-dev/bin/forty-two-watts"
  sha256sum "$HOME/ftw-dev/bin/forty-two-watts"'
```

Expected: remote hash matches the local SHA-256 and a timestamped previous binary remains.

- [ ] **Step 4: Restore/preserve the proven development override and recreate services**

If `docker-compose.override.yml` already proves the detected binary mount, back it up unchanged. If only a saved `docker-compose.override.yml.dev-*.bak` proves it, restore the newest saved development override. Then run:

```bash
ssh "$FTW_SSH_TARGET" 'set -eu
  if [ -d "$HOME/ftw" ]; then cd "$HOME/ftw"; else cd "$HOME/forty-two-watts"; fi
  service="$(docker compose config --services | grep -E "^(ftw|forty-two-watts)$")"
  stamp="$(date +%Y%m%d-%H%M%S)"
  if [ -f docker-compose.override.yml ]; then
    cp docker-compose.override.yml "docker-compose.override.yml.$stamp.bak"
  else
    saved="$(ls -1t docker-compose.override.yml.dev-*.bak 2>/dev/null | head -n1 || true)"
    [ -n "$saved" ] || { echo "no proven development override" >&2; exit 1; }
    cp "$saved" docker-compose.override.yml
  fi
  docker compose up -d "$service"
  if docker compose config --services | grep -qx ftw-updater; then
    docker compose up -d --force-recreate ftw-updater
  fi'
```

Expected: only the detected main service and, when present, the updater sidecar are recreated. `/app/data` is not moved or replaced.

- [ ] **Step 5: Verify container, mount, binary, and served dashboard independently**

Run:

```bash
ssh "$FTW_SSH_TARGET" 'set -eu
  if [ -d "$HOME/ftw" ]; then cd "$HOME/ftw"; else cd "$HOME/forty-two-watts"; fi
  service="$(docker compose config --services | grep -E "^(ftw|forty-two-watts)$")"
  cid="$(docker compose ps -q "$service")"
  docker compose ps "$service"
  docker inspect "$cid" --format "image={{.Config.Image}} status={{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} mounts={{json .Mounts}}"
  docker compose logs --tail=80 "$service"'
curl -fsS "http://192.168.1.139:8080/api/version"
curl -fsS "http://192.168.1.139:8080/" | grep -F 'theme-runtime.js?v=theme1'
```

Expected: one running/healthy main container, the existing `/app/data` mount, the proven dev binary bind, startup/version output matching the development revision, and HTML containing the new theme runtime asset. A successful Compose command alone is not sufficient.

- [ ] **Step 6: Report the local branch and remote rollback artifacts**

Report:

```text
branch: feat/dashboard-simplification
Pi: 192.168.1.139
detected Compose directory/service/binary path
local and remote SHA-256
container status and dashboard revision evidence
timestamped binary and override backup paths
PR/push: not performed
```
