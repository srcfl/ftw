# V2X Integration Completion Plan

Created: 2026-06-11
Branch baseline: `feat/v2x-manual-command`

## Current State

The branch already has the core manual V2X plumbing:

- `v2x_charger` telemetry exists with signed site convention:
  `+W = vehicle charging`, `-W = vehicle discharging`.
- Experimental MQTT drivers exist for Ferroamp DC2 V2X and Ambibox V2X.
- `POST /api/v2x/command` can send manual signed setpoints to one configured
  V2X driver.
- The dashboard driver cards expose V2X status and manual Charge, Discharge,
  and Stop controls when a `v2x_charger` driver is live.
- A default-off `v2x:` policy model exists in config with reserve,
  departure, max power, export, grid-charging, capacity, and cycle-cost
  fields.
- `GET /api/v2x/policy` and `/api/status.v2x_policy` expose the current safe
  charge/discharge envelope per V2X driver.
- `docs/v2x.md`, `docs/api.md`, and a V2X `minor` changeset document the
  manual pilot surface.
- `/api/status`, Home Assistant, loadmodel, MPC divergence detection, history
  research export, and Nova export are V2X-aware.
- Dispatch is V2X-aware enough to avoid charging the stationary battery from a
  V2X discharge by default.
- Full Go test suite passes locally.

The missing pieces are planner/dispatch consumption of the policy envelope,
hardware sign verification, and a staged rollout path beyond the manual pilot.

## Goal

Ship V2X in stages:

1. Manual pilot: safe enough to test against real hardware.
2. Operator controls: visible, understandable, and reversible.
3. Policy engine: reserve/departure constraints before automation.
4. Planner/dispatch integration: V2X can become a dispatchable asset behind an
   explicit feature flag.
5. Production rollout: hardware-verified drivers and release notes.

## Non-Goals For The First PR

- No automatic V2G arbitrage.
- No planner-controlled V2X setpoints.
- No implicit use of a car battery as home storage.
- No default-on behavior for newly configured V2X drivers.

## Milestone 0: Branch And Release Hygiene

Objective: make the branch reviewable and release-safe.

Tasks:

- Decide whether the dashboard status aggregation fix should stay in the V2X
  branch or be split into its own PR. Splitting it would make V2X review
  cleaner, but keeping it is acceptable if we want one operational PR.
- Add a V2X changeset. This should be at least `minor`, because the branch adds
  new drivers and a new API endpoint.
- Keep or rewrite the existing patch changeset for the status aggregation fix.
- Add a short operator note under `docs/` explaining manual V2X test mode,
  sign convention, and the fact that automation is intentionally disabled.
- Open a draft PR once the branch has a matching changeset and docs.

Acceptance:

- `git diff --check` passes.
- `go test ./...` passes.
- Changeset text mentions V2X explicitly.
- PR description says "manual/test only; no automatic V2X dispatch".

## Milestone 1: Hardware Verification

Objective: prove the two experimental drivers match real hardware behavior.

Tasks:

- Build a repeatable test runbook:
  - configure one V2X driver;
  - verify it appears in `/api/status`;
  - send `0 W`, small charge, small discharge, and stop commands;
  - compare charger telemetry, site meter response, and MQTT payloads.
- For Ferroamp DC2, verify the sign of `pe/measured_current`,
  `pe/measured_voltage`, and computed `dc_w` in both charge and discharge.
- For Ambibox, verify `powerAc`, `powerDc`, energy counters, limits, and the
  `device/ess/0/targetPower` command semantics.
- Verify stale telemetry behavior: no fresh power pair means no live setpoint
  acceptance for non-zero commands.
- Record tested firmware, MQTT topics, sign observations, and limits in a doc.

Acceptance:

- A live charge command moves site meter in the expected import direction.
- A live discharge command moves site meter in the expected export/import
  reduction direction.
- `v2x_stop` returns the charger to zero target.
- Driver `DefaultMode` sends a zero target.
- Any unverified driver remains marked `experimental`.

## Milestone 2: Operator UI For Manual V2X

Objective: make manual testing usable without curl.

Tasks:

- Add a V2X section/card to the existing dashboard driver/status surface:
  - driver name and online/offline state;
  - connected/plug state;
  - vehicle SoC;
  - signed AC power;
  - DC voltage/current/power;
  - session charge/discharge Wh;
  - charger limits;
  - control mode/status.
- Add a manual control modal:
  - driver selector if multiple V2X drivers exist;
  - segmented charge/discharge/stop control;
  - bounded W input or slider using reported limits where available;
  - confirmation before discharge;
  - clear copy that commands are manual test commands.
- Disable non-zero controls when the selected driver is not live.
- Prefer `/api/status` data first; add `GET /api/v2x/status` only if the UI
  becomes awkward or status payload shape gets too large.

Acceptance:

- A user can inspect V2X state and send stop/charge/discharge commands from the
  UI.
- Offline/stale V2X drivers cannot receive non-zero manual setpoints.
- Mobile layout does not overlap and controls remain reachable.
- No marketing or explanatory landing page; it is an operational control.

## Milestone 3: V2X Policy Model

Objective: define the safety contract that automation must obey.

Tasks:

- Done: Add a V2X policy model in a small `go/internal/v2x` package or a
  focused config/control helper:
  - enabled flag, default false;
  - minimum vehicle reserve SoC;
  - departure target SoC or required energy;
  - departure time window;
  - max charge/discharge W;
  - export allowed;
  - grid charging allowed;
  - optional cycle cost or degradation penalty;
  - stale or missing required input collapses the envelope to `0 W`.
- Done: Start with YAML plus API readback, then add editable UI only after the
  policy shape survives one hardware test.
- Done: Make policy evaluation produce explicit allowed charge/discharge
  envelopes for the current tick.
- Done: Add tests for reserve, departure, disconnected vehicle, stale SoC, and
  charger limit interactions.
- Remaining decision: whether production policy editing should stay in static
  YAML, move into the Settings UI, or become a loadpoint-style persisted
  schedule for recurring departure rules.

Acceptance:

- Policy can answer: "what V2X power range is safe right now?"
- Missing SoC or stale telemetry collapses the safe range to `0 W`.
- Reserve/departure constraints are test-covered before planner integration.

## Milestone 4: Planner And Dispatch Integration

Objective: let the EMS use V2X automatically, but only behind policy and a
feature flag.

Tasks:

- Extend planner inputs with V2X assets as separate dispatchable storage:
  - signed current W;
  - vehicle SoC;
  - capacity or available energy estimate;
  - safe charge/discharge envelope from policy;
  - deadline/required energy;
  - cycle cost.
- Keep stationary batteries and V2X separate in the plan. Do not hide V2X
  inside the existing battery pool, because reserve/departure constraints are
  materially different.
- Extend slot directives with V2X target energy/power.
- Extend dispatch output with V2X targets and send them through
  `v2x_set_power`.
- Apply fuse guard and stale-meter guard to combined stationary battery + V2X
  behavior.
- Add a feature flag such as `site.v2x_auto_enabled` or a dedicated V2X policy
  `auto_dispatch: true`, default false.

Acceptance:

- With auto disabled, behavior is identical to manual-pilot mode.
- With auto enabled and policy allowing discharge, planner can reduce peak/grid
  import using V2X without violating reserve/departure requirements.
- With policy denying discharge, planner cannot use V2X even if prices are
  favorable.
- Stale V2X telemetry sends zero target and removes V2X from the planner asset
  set.

## Milestone 5: Observability And Safety

Objective: make V2X behavior debuggable and reversible.

Tasks:

- Add history fields for planned V2X target and actual V2X W where useful.
- Add a diagnostics view or existing diagnose integration for V2X plan vs
  actual.
- Add events/notifications for:
  - V2X driver offline while auto enabled;
  - reserve constraint blocking discharge;
  - departure target at risk;
  - charger refuses or clamps a command.
- Add a manual emergency stop endpoint or reuse `POST /api/v2x/command`
  `v2x_stop` from UI with prominent placement.
- Ensure watchdog/default-mode behavior always sends zero target.

Acceptance:

- Operator can answer "why is V2X idle?" from UI/API state.
- Operator can stop V2X without changing EMS mode.
- Auto mode failure path is zero target, not stale last target.

## Milestone 6: Production Readiness

Objective: move from pilot to supported feature.

Tasks:

- Promote verified drivers from `experimental` only after hardware evidence.
- Add docs:
  - `docs/v2x.md`;
  - config example;
  - safety model;
  - test procedure;
  - known hardware limitations.
- Add e2e/simulator coverage for a synthetic V2X driver if practical.
- Add release note/changelog entries for manual mode and later auto mode.
- Decide whether Nova unified schema needs to ship before production V2X or
  whether legacy V2X mapping is sufficient.

Acceptance:

- Feature is documented for operators.
- Supported hardware list is honest.
- Automatic behavior is opt-in and has a tested fallback to zero.

## Suggested PR Sequence

PR 1: Manual V2X pilot

- Current telemetry/API/driver work.
- V2X changeset and docs.
- Manual dashboard controls so hardware users can test without curl.
- Default-off V2X policy readback and envelope tests.
- No automatic dispatch.

PR 2: Auto-dispatch behind flag

- Planner asset model.
- V2X slot directives.
- Dispatch target sending.
- Safety/observability tests.

PR 3: Hardware promotion

- Verified driver docs.
- Remove or refine experimental warnings per device.
- Production release notes.

## Open Decisions

- Should PR 1 include a basic UI, or should it stay API-only so we can hardware
  verify faster?
- Which hardware should be the first verification target: Ferroamp DC2 or
  Ambibox?
- Should the existing dashboard status aggregation fix be split from the V2X
  branch?
- Should production policy editing stay YAML-only, move into Settings UI, or
  become a loadpoint-style persisted departure schedule?
- Is Nova legacy schema sufficient for first production use, or should V2X wait
  for unified schema adoption?

## Immediate Next Step

Prepare PR 1:

1. Decide whether to split the dashboard aggregation fix.
2. Run the full local test suite after the policy changes.
3. Open a draft PR and run CI.
4. Pick one hardware target and run the verification matrix.
