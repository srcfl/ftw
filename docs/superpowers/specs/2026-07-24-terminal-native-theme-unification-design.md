# Terminal Native theme unification design

Date: 2026-07-24
Status: approved direction; written specification awaiting review

## Summary

Unify the complete FTW web UI around one neutral Terminal Native surface
system. The current dashboard uses the newer `--ink` / `--line` palette, while
Energy history, diagnostics, parts of Settings, several component fallbacks,
and the setup wizard still use a fixed blue-purple `--surface` palette. The
result is most visible in the bluish top header and the purple Detailed records
panels.

This pass makes the newer palette canonical, maps the legacy tokens onto it,
and removes remaining hard-coded blue or purple colors from interface chrome.
Semantic data colors remain distinct. The visual identity stays recognizably
FTW: compact operational layouts, sans-serif headings, monospaced data, amber
interaction states, and semantic green, red, cyan, and violet.

The same pass also:

- removes the redundant `FTW` text beside the FTW logo;
- makes Flow the first and default Power now presentation;
- keeps Values as the secondary presentation and preserves explicit user
  preferences;
- scrolls a newly selected mobile destination to the top; and
- prepares a verified ARM64 development build for deployment to the user's
  Raspberry Pi before any pull request is opened.

## Goals

- Give every FTW surface a consistent neutral canvas, panel, recessed, border,
  and text hierarchy in both dark and light themes.
- Include Overview, Energy, Plan, History, More, Settings, Update Center,
  diagnostics, notification surfaces, all registered `ftw-*` components, and
  the standalone `/setup` wizard.
- Remove legacy blue-purple interface backgrounds without flattening semantic
  chart series into one color.
- Keep the current information architecture, component capabilities, and
  safety behavior unchanged.
- Make a fresh browser open Power now in Flow while respecting an explicit
  stored Values choice.
- Make mobile destination switching start the selected section at the top.
- Verify locally, then deploy to `192.168.1.139` using the detected FTW layout
  and existing persistent data mount.

## Non-goals

- No backend, planner, driver, power-sign, database, or API changes.
- No broad typography, spacing, radius, or information-architecture redesign.
- No replacement of existing chart visualizations or semantic series colors.
- No conversion of all legacy light-DOM markup into Web Components.
- No changes to setup workflow behavior or configuration shape.
- No new theme choices beyond the existing dark and light modes.
- No push or pull request before the Raspberry Pi review.

## Chosen approach: canonical tokens plus targeted cleanup

Use the existing `web/components/theme.css` as the sole palette authority.
Retain the current `--ink`, `--ink-raised`, `--ink-sunken`, `--line`, `--fg`,
and semantic token names. Redefine the older base tokens as compatibility
aliases to those canonical roles:

```css
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

This bridge updates legacy light-DOM CSS and the setup wizard without a risky
markup rewrite. Targeted edits then remove hard-coded chrome colors that bypass
both token families.

### Rejected alternatives

1. An `app.css` override layer would be faster but could not reliably retheme
   shadow DOM, Update Center internals, or `/setup`.
2. Rebuilding every surface on the foundation Web Components would be cleaner
   in isolation but would expand this visual pass into a high-risk UI rewrite.

## Surface palette

### Dark

| Role | Value | Use |
|---|---:|---|
| Canvas | `#0d0d0d` | page background |
| Recessed | `#101010` | inputs, table bodies, chart wells |
| Panel | `#161616` | cards and primary sections |
| Raised | `#1e1e1e` | drawers, menus, modal headers |
| Border | `#2a2a2a` | all surface boundaries |
| Primary text | `#e8e8e8` | headings and values |
| Secondary text | `#a0a0a0` | descriptions and labels |
| Muted text | `#858585` | metadata and placeholders |

### Light

| Role | Value | Use |
|---|---:|---|
| Canvas | `#f4f4f2` | page background |
| Recessed | `#ecece8` | inputs, table bodies, chart wells |
| Panel | `#fafaf8` | cards and primary sections |
| Raised | `#ffffff` | drawers, menus, modal headers |
| Border | `#cecec7` | all surface boundaries |
| Primary text | `#191919` | headings and values |
| Secondary text | `#4f4f4b` | descriptions and labels |
| Muted text | `#686862` | metadata and placeholders |

Small text must meet WCAG AA 4.5:1 contrast against the surface on which it is
rendered. Existing `--fg-label` remains the dedicated compact-label token.

## Functional color discipline

- Amber is reserved for selected controls, primary local actions, current-time
  markers, and attention that is not an error.
- Green communicates healthy, exported, generated, saved, or successful state.
- Red communicates imported load, failure, unsafe, or destructive state.
- Cyan communicates battery, EV, or informational state.
- Violet is allowed for distinct data series such as a second battery, but not
  as a panel or menu background.
- Near-black `--on-accent` text is used on amber filled controls.
- Color never acts as the sole state indicator; labels and values remain.

Hard-coded colors remain acceptable only when they encode a real data series
or draw an image/canvas scene. Interface chrome, empty states, tooltips, table
surfaces, headers, and controls must use theme tokens.

## Global canvas and header

- Replace the decorative blue radial page gradients with the solid Canvas
  token. Data visualizations may retain gradients when the gradient itself
  communicates energy flow or a threshold.
- Replace the translucent, blurred, hard-coded header pseudo-element with a
  flat Panel or Raised surface and one Border token.
- Keep the existing header dimensions and rounded geometry to preserve current
  FTW DNA.
- Remove `<h1>FTW</h1>` from the dashboard header. The existing logo remains
  left-aligned with `alt="FTW"` and is the sole brand mark.
- Desktop navigation remains next to the logo. The mobile logo and menu button
  keep their current left/right anchors.
- The mobile drawer uses Raised for its outer surface, Recessed for grouped
  controls, Border for separation, and amber only for the active destination.

## Component coverage

### Foundation components

Audit `ftw-card`, `ftw-modal`, `ftw-tabs`, `ftw-badge`,
`ftw-progress-bar`, and `ftw-legend`. These already mostly use canonical
tokens; normalize any fallback colors and add an `--on-accent` token where
near-black selected text is currently repeated.

### Data and control components

Audit the energy flow, history card, savings card, energy cake, bar chart,
price chart, battery control, PV control, update check, notification status,
notification history, notification test, and PV array 3D components.

- Keep their semantic data colors.
- Move tooltip, empty, control, and card surfaces onto the canonical roles.
- Replace blue/slate fallback panel colors such as `#1e293b`, `#334155`,
  `#0f172a`, and fixed purple fills when they represent chrome.
- Make all shadow components inherit light/dark text and border roles.
- Keep the 3D scene's physical object colors, while moving its labels,
  tooltips, and control chrome onto the canonical surface roles.

### Legacy dashboard and setup surfaces

The compatibility aliases retheme the legacy selectors in `web/style.css`,
including Energy history, diagnostics, Settings, forms, and `/setup`.
Targeted selectors handle places that use fixed RGBA or hex backgrounds.
Setup keeps its current layout, steps, and behavior.

### Canvas charts

Canvas chrome such as grid lines, labels, tooltip backgrounds, and empty-state
text resolves computed theme tokens at draw time. Series colors remain
semantic and distinct. Theme switching must redraw any canvas whose chrome
depends on the active palette.

## Energy history and earlier decisions

The History destination keeps its current hierarchy and controls.

- Detailed records header, chart well, summary cards, table, and table header
  use Panel/Recessed/Border instead of the legacy purple surface.
- Earlier decisions uses the same neutral roles.
- Active decision rows use a low-opacity amber tint and amber edge rather than
  blue.
- Reason pills retain semantic differentiation but use token-based tints that
  work in both themes.

## Power now default and preference compatibility

The segmented control reads **Flow / Values**, in that order.

- No stored preference: Flow.
- Stored `hero` or `flow`: Flow.
- Stored `numbers` or `values`: Values.
- Unknown or corrupt value: Flow.
- Selecting Flow continues to persist the legacy-compatible `hero`.
- Selecting Values continues to persist the legacy-compatible `numbers`.

Flow remains the same live circle visualization and polling path. Values is a
secondary compact presentation, not a separate data source.

## Mobile destination scrolling

Only a user-initiated click in `#mobile-destinations` triggers the reset.
When the clicked destination differs from the current destination:

1. update the hash through the existing navigation path;
2. apply the new view;
3. scroll the document to `top: 0` with non-animated behavior.

Browser Back/Forward, legacy deep links such as `#diagnose/<timestamp>`, and
in-section anchors do not force a scroll reset. Tapping the already active
destination does not change the current position.

## Loading, error, and unavailable states

- Existing fallback behavior remains functional when optional APIs return 404
  or are unavailable.
- Loading and empty states use Recessed/Muted roles and retain their guidance.
- Errors use semantic red and include explanatory text.
- No component may fall back to a blue or purple panel when a token is missing;
  neutral fallback values are used instead.
- Theme changes must not erase stale-state or safety-state distinctions.

## Accessibility

- Text contrast is at least WCAG AA in both themes.
- Focus rings use amber with sufficient offset from the control border.
- Selected segmented controls use near-black on amber.
- Status remains understandable without color.
- The logo retains an accessible name after the duplicate text is removed.
- Mobile destination buttons remain at least 44 px high.
- Reduced-motion preferences continue to suppress decorative transitions.

## Testing

### Automated web tests

- Canonical legacy-token aliases exist in `theme.css`.
- Disallowed legacy blue-purple chrome values do not appear in the audited
  surface rules, with an explicit allowlist for data-series colors.
- The dashboard header contains one FTW brand representation: the logo.
- Power now defaults to Flow, orders Flow before Values, and preserves both
  legacy stored values.
- Mobile bottom navigation scrolls only user-initiated destination switches.
- The five destinations and historical hash aliases remain compatible.
- All first-party JavaScript remains syntactically valid.

### Browser verification

At desktop and 390 px mobile widths, inspect dark and light variants of:

- Overview in Flow and Values;
- Energy, including both charts and price;
- Plan;
- History, including Detailed records and Earlier decisions;
- More;
- Settings and its tabs;
- Update Center and notification history;
- mobile header drawer; and
- `/setup`.

Confirm that no blue-purple panel chrome remains, no content overflows, the
logo-only header stays aligned, Flow is the fresh default, stored Values is
respected, and mobile section changes land at the top.

### Repository verification

Run:

```bash
npm test
make verify
```

This is a user-visible change and receives a patch changeset.

## Raspberry Pi deployment

Deployment happens only after local tests and browser verification pass.

1. Obtain the complete SSH target in `user@192.168.1.139` form.
2. Run the switching-ftw-deploy-mode read-only probe to detect the canonical or
   legacy install directory, main service, image, data mount, and saved
   overrides.
3. Verify the Pi architecture before producing or copying an ARM64 binary.
4. Preserve the current development binary and override with timestamped
   backups.
5. Recreate only the detected main service with `docker compose up -d
   <service>`; never run `down`, rename the project, or move `/app/data`.
6. Verify the running container, image and bind mount, then verify the served
   dashboard revision independently.
7. Leave rollback artifacts on the Pi and do not open a pull request.

If the detected service layout is ambiguous, the `/app/data` bind is absent,
or the binary mount target cannot be proven, deployment stops for review.

## Acceptance criteria

- All FTW web surfaces use one neutral Terminal Native palette in dark and
  light modes.
- Energy history and the top header no longer appear blue or purple.
- No duplicate FTW wordmark appears beside the logo.
- Fresh browsers open Power now in Flow; explicit Values preferences survive.
- Mobile destination switches display the selected page from the top.
- Setup inherits the same tokens without behavioral or layout regression.
- Automated and browser verification pass.
- The verified build runs on the user's Pi with its existing data and a
  recoverable previous deployment.
- The branch remains unpushed and no pull request exists.
