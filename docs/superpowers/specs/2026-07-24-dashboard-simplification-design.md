# Dashboard simplification design

Date: 2026-07-24
Status: approved visual direction and information architecture

## Summary

Keep FTW's five top-level destinations: Overview, Energy, Plan, History and
More. Give each destination a clearer job, then make Overview answer the three
questions an operator asks most often:

1. What is happening now?
2. What does electricity cost now and when is the next low-price window?
3. What will FTW do next?

The visual direction remains Terminal Native and preserves FTW's current DNA:
dark operational surfaces, monospaced data, compact spacing, amber interaction
states and semantic green/red/cyan. This is an information-hierarchy change,
not a brand replacement.

## Goals

- Make Overview useful without scrolling through expert charts.
- Add a compact price view and a plain-language plan summary to Overview.
- Preserve the existing circle-based energy-flow visualization as an explicit
  alternative to a compact power-values view.
- Keep the existing five destinations and historical hash routes.
- Reduce duplicated information and equal-weight card grids.
- Make the first mobile viewport answer "now, price, next" without requiring
  chart interpretation.
- Preserve core safety authority and existing API contracts.

## Non-goals

- No backend, planner, dispatch or power-sign changes.
- No replacement of the detailed price, live-power or plan charts.
- No new configuration schema or persisted server state.
- No changes to Settings information architecture in this pass.
- No driver, optimizer or hardware-control changes.
- No push or pull request as part of the local-preview phase.

## Chosen information architecture: Clean five

### Overview: current situation and immediate outlook

Overview contains, in order:

1. A compact page heading with connection/health state.
2. **Power now**, with a `Values / Flow` segmented control.
3. **Electricity price**, showing the current price, next low-price window and
   a compact time profile.
4. **What FTW does next**, showing one actionable plan statement and its
   governing reason or safety state.
5. A compact today strip for import, export, solar generation and savings.
6. Fuse state, retained as a safety-relevant item below the primary answers.

Overview does not contain the full price chart, live telemetry chart, full plan
chart, historical report or diagnostics.

### Energy: detailed current energy

Energy retains the existing full electricity-price chart, today's directional
energy totals, live power chart, 24-hour battery/SoC chart and conditional
heating section. The page heading explains that this is the detailed monitoring
surface.

The first screen prioritizes the live chart. Today's totals and full price chart
remain available without duplicating the new compact Overview cards.

### Plan: detailed future actions

Plan keeps the strategy control, horizon control, replan action and detailed
schedule chart. Its five equal-weight briefing cards are visually condensed
into:

- one primary state and next-action statement;
- one secondary line for reason and active safety adjustment;
- a compact metadata row for forecast confidence, expected SoC and planner
  source.

The plan chart remains the expert explanation surface. Overview only shows the
normalized plain-language summary.

### History: measured outcomes and earlier decisions

History keeps both outcomes and plan-decision inspection because the selected
information architecture retains five destinations. The default reading order
is:

1. history in numbers;
2. savings;
3. detailed energy export/reporting;
4. plan decisions.

Detailed reporting and plan-decision diagnostics receive lower visual emphasis
than the outcome summary. Existing `#history/<timestamp>` and
`#diagnose/<timestamp>` compatibility remains intact.

### More: configuration and advanced operations

More retains Settings, Setup, Version and updates, plus the existing Advanced
entry point. No new operational detail is moved into Overview.

## Power now: Values and Flow

### Control

The current unlabeled header icon becomes a labeled segmented control inside
the Power now card:

- **Values**: compact table/grid of current readings.
- **Flow**: the existing circle-based energy-flow visualization.

The control uses `role="tablist"`, two tab buttons, `aria-selected`, keyboard
arrow navigation and visible focus states. Mobile touch targets are at least
44 px.

### Preference and defaults

- Reuse the existing `ftw-hero-mode` local-storage key for compatibility.
- Existing `hero` means Flow.
- Existing `numbers` means Values.
- A new browser with no preference starts on Values, matching the simplified
  dashboard.
- Changing the selection updates immediately and persists locally.
- If local storage is unavailable, the control continues to work for the
  current page without persistence.

### Values presentation

Values is one bounded Power now surface rather than five unrelated cards.
Desktop uses columns; mobile uses compact rows. It includes:

- Solar generation;
- Grid import/export;
- Home load;
- Battery power and SoC when available;
- EV power/state when available.

The existing element IDs used by status polling remain stable so the control
tick and telemetry rendering do not need a second data path.

### Flow presentation

Flow reuses the existing `<ftw-energy-flow>` component and current click
targets. When embedded inside Power now, its redundant internal heading is
suppressed and the outer card supplies the accessible name. The visualization
may remain taller than Values because it is an explicitly selected exploratory
view rather than the default dashboard density.

## Compact price summary

Add a compact rendering mode to `<ftw-price-chart>` and instantiate it on
Overview. The detailed instance remains on Energy.

The compact mode shows:

- current consumer-resolved price and unit;
- the next lowest future slot inside the published horizon;
- a small today profile with the current position and low window;
- a clear link to the full Energy price chart.

Compact mode omits the VAT and horizon controls, hover-dependent explanation
and detailed high/average statistics. It respects the same stored VAT
preference as the full chart so two surfaces never disagree about the displayed
price.

The two component instances may each fetch `/api/prices`; at a five-minute
interval this is a deliberately simpler and safer local request pattern than
introducing a new global client-side store.

## Plain-language plan summary

`plan.js` remains the owner of plan fetching and interpretation. Extract the
current plan-briefing decision logic into a pure normalization function that
returns one view model. Both the detailed Plan briefing and Overview summary
render from that view model.

The Overview summary contains:

- state badge: active, ready, manual, preparing, fallback or stale;
- next action and time;
- short reason;
- active safety adjustment or "no active safety adjustment";
- expected SoC when relevant;
- link to the full Plan destination.

This avoids a second `/api/mpc/plan` poller and prevents wording or fallback
logic from diverging between Overview and Plan.

## Data flow

```text
/api/status -----------------> app.js -----------------> Power now values/flow
       |                                                connection + fuse state
       |
       +----------------------> plan.js

/api/mpc/plan ---------------> plan.js ----------------> normalized plan brief
                                                        |               |
                                                        v               v
                                                  Overview summary   Plan detail

/api/prices -----------------> ftw-price-chart --------> compact Overview mode
                         \----> ftw-price-chart --------> detailed Energy mode
```

No planner output is sent to hardware by these surfaces. The UI only reads
validated core state and existing plan responses.

## Loading, unavailable and safety states

### Price

- Loading: retain card dimensions and show `Loading prices...`.
- Price not configured: show `Price unavailable` with a path to Price settings.
- Fetch failure with prior data: keep the last rendered data and mark it stale.
- Fetch failure without data: show an unavailable state rather than an empty
  chart.

### Plan

- Planner off: `Manual control is active`.
- No plan yet: `Preparing the first plan`.
- Stale plan: `Fallback active` with warning treatment.
- Solver fallback: name the built-in plan source.
- Active clamp: state that safety adjusted the requested device target.
- No battery: omit battery/SoC-specific wording.

### Live telemetry

- Missing values render as an em dash, not zero.
- Stale or disconnected site state remains visually distinct from a valid zero.
- Existing stale-meter safety behavior is unchanged.

## Responsive behavior

### Desktop

- Overview uses a 12-column grid.
- Power now spans the full width.
- Price and plan summary sit side by side.
- Today totals form one compact strip.

### Mobile

- Sections collapse to one column in the same reading order.
- Values is the default Power now view.
- Flow remains available through the in-card segmented control.
- Price and plan summaries avoid horizontal scrolling.
- The sticky bottom navigation receives sufficient page-bottom clearance and
  does not cover card controls or content.

## Accessibility

- Maintain at least WCAG AA 4.5:1 text contrast in light and dark themes.
- Do not communicate price, plan or safety state by color alone.
- Use tabular numerals for live values.
- Give charts text summaries and meaningful accessible names.
- Support keyboard navigation for primary navigation and the Power now switch.
- Respect reduced-motion preferences.
- Keep headings in a logical page hierarchy.

## Testing and verification

### Automated web tests

- Five desktop and mobile destinations remain present.
- Historical hash aliases remain compatible.
- Overview owns Power now, compact price and plan-summary surfaces.
- Power now exposes both Values and Flow with accessible state.
- `ftw-hero-mode` compatibility and the Values default are covered.
- Plan normalization covers planner-off, preparing, active, stale and fallback
  states.
- Compact price mode covers populated, unconfigured and unavailable states.

### Browser verification

At desktop and 390 px mobile widths:

- compare Values and Flow;
- reload and confirm the selected Power now view persists;
- verify current price and next low window;
- verify manual, active and stale plan summaries;
- inspect all five destinations;
- confirm bottom navigation does not obscure content;
- check light and dark themes;
- inspect console errors.

### Repository verification

Run the narrow web tests while iterating, then:

```bash
npm test
make verify
```

The change is user-visible and therefore requires a `.changeset/*.md` entry.

## Local-preview acceptance criteria

- The live local dashboard opens on the simplified Values view for a fresh
  browser.
- Flow restores the current circle visualization without losing telemetry or
  click behavior.
- Overview shows current price and the next low-price window.
- Overview describes FTW's next action without requiring the plan chart.
- The five destinations and historical deep links still work.
- Desktop and mobile screenshots match the Terminal Native direction.
- No PR or push is created during the preview phase.
