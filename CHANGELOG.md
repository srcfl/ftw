# Changelog

## 0.108.2

### Patch Changes

- 0779ff2: **Hardening: cover-load and passive-arbitrage-idle carve-outs now reset stale
  energy-path bookkeeping on every tick they fire**, mirroring what
  `preparePlannerSelf` already does for `planner_self`.

  Without this, `slotDelivered` / `lastTickTs` / `currentDirective` could
  carry leftover state from a prior energy-path tick into the carve-out
  window. A subsequent transition back to the energy path within the same
  15-min slot (e.g., a mid-slot replan flipping the slot's intent, or an
  operator mode-hop) would then read those stale values and miscompute
  `remainingWh`. Same forward-transition risk that `planner_self` has
  guarded against since PR #131.

  No new behaviour, no signal change in the steady-state cover-load reactive
  path — purely defence-in-depth for plan-refinement / mode-transition
  scenarios. Two regression tests pin the bookkeeping reset for both the
  `planner_arbitrage` cover-load and the `planner_passive_arbitrage` idle
  carve-outs.

## 0.108.1

### Patch Changes

- 1160393: fix(pvmodel): MPC now consumes the unanchored structural PV predictor so the rolling residual correction (PR #381) is not applied twice. Previously `mpcSvc.PV` was wired to `pvSvc.Predict`, which already folds in the live-vs-model now-anchor; combined with `PVResidualCorrect` the planner saw the structural-vs-live bias subtracted twice and could plan as if PV was ~0 W on a sunny day with a heavy downward residual. A new `pvmodel.Service.PredictStructural` returns the RLS-only prediction; the anchored `Predict` is kept for UI overlays and dispatch's live-reading path.

## 0.108.0

### Minor Changes

- b887541: **PV twin now applies a short-horizon residual correction on top of the
  structural RLS prediction.** The RLS model's forgetting factor (~3h
  half-life @ 60s cadence) is tuned to learn site orientation, shading
  and slow soiling drift; it does not respond fast enough to "today's
  persistent NWP bias" — e.g. when measured cloud cover is heavier than
  the forecast assumed for the last 90 minutes, structural predictions
  stay biased high while RLS chews through the samples needed to adapt.

  The new layer keeps a 2-hour rolling buffer of (predicted_at_t,
  actual_at_t) pairs, computes the mean residual, and applies it as an
  additive bias to MPC slot predictions, fading linearly over a 2 h
  horizon (full correction ≤ 30 min, zero by 120 min). Beyond 2 h the
  structural model is again the best estimate — weather fronts roll in,
  time-of-day shifts, and the residual is no longer relevant.

  Gates (`go/internal/pvmodel/residual.go`):

  - ≥ 20 samples in the 2 h window before any correction applies.
  - `|mean residual|` ≥ 25 W → otherwise treated as "no bias detected".
  - `std / |mean|` ≤ 1.0 → variance-dominated streams are skipped.
  - `dt ≤ 0` (past slot) → factor = 0.

  Wiring: `pvmodel.Service.ResidualCorrect` is plumbed into
  `mpc.Service.PVResidualCorrect` (new optional hook). The planner calls
  the corrector on the slot midpoint inside `buildSlots`, after the twin
  prediction and before `selectPlannerPVW` blends with the NWP forecast.
  A nil hook is a hard no-op, so existing wiring without the corrector
  is unchanged.

  **PV only**: load is multimodal (appliances cycling) and a rolling-mean
  correction can chase the noise. Variance gate would catch it most of
  the time, but risk/reward is poor without dedicated diagnostics.
  Revisit when load observability lands.

  Diagnostics exposed via `GET /api/pvmodel`:
  `pv_residual_correction_w` (the value the planner would apply 15 min
  out), `pv_residual_sample_count`, `pv_residual_mean_w`,
  `pv_residual_std_w`, `pv_residual_window_minutes`.

- 2ff3d09: feat(mpc): tighter replan triggers + twin-driven replan signal

  Tightens the reactive replan thresholds (PV 500→250 Wh, load 400→200 Wh,
  half-life 15→8 min, cooldown 60→30 s) and adds a third trigger that fires
  when the PV or load twin's CURRENT prediction has shifted materially (RMSE

  > 250 W PV / 200 W load over the next 16 slots) from the prediction the
  > active plan was built on.

  The twin already self-corrects every cycle through RLS; the planner only
  consumed its output every 15 min. The new signal closes that gap without
  waiting for the integral-of-error to accumulate. Replanning is ~100 ms on
  a Pi 4 (51 × 21 × 193 DP cells, sub-1 % CPU) — being stingy was the wrong
  default.

### Patch Changes

- 55fb0c3: **Codex review follow-ups for v0.107.0** — fixes 2 P1 and 2 P2 review
  findings on the dispatch / loadmodel changes shipped in v0.107.0.

  **P1: Heating coefficient survives restarts.** `main.go` had been calling
  `loadSvc.SetHeatingCoef(cfg.Weather.HeatingWPerDegC)` at startup, which
  unconditionally overwrote any value persisted from previous training.
  After every binary update the adaptive fit was thrown away. New
  `SeedHeatingCoef(w)` only writes the value when the model has no samples
  yet — operator config is the cold-start prior, observation drives the
  value once learning has begun. `SetHeatingCoef` remains for explicit
  operator overrides.

  **P1: Cover-load carve-out actually chases grid=0.** The PR #378
  carve-out only set `useEnergyPath = false`; in production `main.go` wires
  both `SlotDirective` and `PlanTarget`, so the code fell into the legacy
  `!useEnergyPath` branch and called `SetGridTarget(plannedImportW)` —
  chasing the planned positive import instead of grid=0. Result: cover-
  load slot with a 1.7 kW planned import would back the battery off all
  the way to idle instead of covering live load. Fixed by forcing
  `SetGridTarget(0)` for carve-out slots and skipping the legacy
  PlanTarget block when a carve-out predicate fires.

  **P2: Live-export gate predicate tightened.** `passiveArbitrageIdleSlot`
  used `dir.BatteryEnergyWh <= idleWhGate`, which is true for _any_
  negative-energy slot (planned discharge). Tightened to
  `|BatteryEnergyWh| ≤ idleWhGate` so the predicate names what it does
  (true idle only). The planned-discharge case is now folded into
  `coverLoadDischargeSlot`, which was also extended to cover
  `planner_passive_arbitrage` (not just `planner_arbitrage`), and the
  live-export gate now fires on either predicate.

  **P2: SlotDeliveryStats catches sign mismatches.** Planned `-425 Wh`
  discharge vs actual `+425 Wh` charge would have scored `|actual| /
|planned| = 1.0` = "on target" — the largest possible miss, invisible
  on `/api/status`. New `SignMismatchCount` field fires when planned and
  actual have opposite signs (and both exceed the idle cutoff). The
  magnitude over/under counters then only fire on same-sign cases,
  keeping their semantics clean.

## 0.107.0

### Minor Changes

- adf3f86: **Fix: `planner_arbitrage` cover-load discharge slots now chase the live
  zero-grid line instead of rigidly running the planned discharge power.**
  When the DP picks a discharge slot to _offset expensive import_ (rather
  than to _export at peak price_), the energy-allocation path used to lock
  the battery at `remainingWh × 3600 / remainingS` regardless of live
  conditions — exporting at spot price any forecast-load undershoot and
  under-covering any forecast-load overshoot. The EMS now routes these
  slots through reactive PI on grid=0, the same path
  `planner_passive_arbitrage` non-charge slots and `planner_self`
  participant slots already use.

  Detection: `PlannedGridW > -100 W` (no significant planned export) AND
  `BatteryEnergyWh < -50 Wh` (discharge planned). Peak-export slots
  (`PlannedGridW < -100 W`) stay on the energy path — extra export there is
  bonus revenue at the price the DP picked the slot for. Charge slots
  stay on the energy path so deliberate grid-charge intent is honoured.

  Found 2026-05-28: plan estimated baseload 1.7 kW for a slot that scheduled
  the battery to be empty by 23:30. Real load was 0.9 kW; battery sat at
  -1.7 kW exporting 800 W at spot. Then load surged to 3.2 kW and the
  battery stayed at -1.7 kW, forcing 0.5 kW import. Both directions are
  now reactive — the slot's Wh budget guides where the battery is
  generally headed, the meter decides the instantaneous power.

- fdbf53c: **Load model now adapts the heating coefficient online from measurements.**
  Previously `HeatingW_per_degC` was operator-set and never moved — if the
  value drifted from reality (or the house turned out not to track outdoor
  temperature at all), forecasts silently inflated cold-day load and the
  MPC made decisions on phantom heating draw.

  The fit runs as one-parameter SGD on the prediction residual:
  `coef ← coef + α · err / deltaT`. Gated on `bucket.Samples ≥
MinTrustSamples` (residual derives the slope from the bucket baseline)
  and on `deltaT ≥ 3 °C` (warm samples and near-reference samples have no
  heating signal to extract). Clamped to `[0, 1500] W/°C`.

  The fit runs **before** the outlier filter so a wildly stale coefficient
  can recover — without that ordering, every cold sample under a wrong
  coef looks like an outlier vs the warm-day MAE and nothing could ever
  pull the value down.

  Operator config (`Planner.HeatingWPerDegC` / `SetHeatingCoef`) still
  seeds the initial estimate and is re-applied on
  `POST /api/loadmodel/reset`. From there observation drives the value.
  Households whose load is temperature-independent (district heating,
  solar-gain-dominated shoulder seasons, well-insulated homes) converge
  toward 0 W/°C.

  Found 2026-05-28 on site .40: planner predicted 2782 W load for a sunny
  May afternoon (actual 504 W). Root cause was the un-adapted heating
  term — `300 W/°C × (18 − 11.4 °C) = 1980 W` of phantom load applied
  without seasonal / solar-gain awareness. The dispatcher fix in #375
  prevents the _symptom_; this change addresses the _cause_.

- c1cbda7: feat(diagnostics): per-slot Wh delivery tracking for reactive dispatch paths

  Adds an independent per-slot Wh accumulator that runs on every dispatch
  tick regardless of which execution path was taken (planner_self, planner
  passive_arbitrage idle slots, the planner_arbitrage cover-load carve-out
  from PR #378, manual modes, plain self_consumption). At slot rollover
  the actual fleet delivery is compared against the plan's
  `BatteryEnergyWh`; ratios outside [0.5, 1.5] are logged and bump
  `SlotDeliveryStats.OverDeliveryCount` / `UnderDeliveryCount`, surfaced
  on `/api/status`. Idle slots (|planned| ≤ 50 Wh) are skipped — ratio
  against ~0 is meaningless.

  Pure observability — no dispatch decision reads the counters and no
  hard Wh cap is applied to reactive paths. The point is to measure
  first, decide on enforcement later.

### Patch Changes

- bdf2352: **Fix: `planner_passive_arbitrage` no longer absorbs live PV surplus into the
  battery on a plan-idle slot.** When the DP picked idle for a slot
  (`battery_w = 0`) and live conditions turn out to have more PV (or less
  load) than the forecast assumed, the dispatcher now holds the battery at
  0 and lets the surplus export — rather than collapsing to
  self-consumption and chasing `grid = 0` by ramping the charge up.

  The DP picks idle slots deliberately, often to preserve export revenue
  at the current spot when future PV is plentiful and future prices are
  lower. The old behaviour reactively swallowed that surplus because the
  fallback path was symmetric with self-consumption ("balance to zero"),
  which discards the DP's intent. The gate is the mirror of
  `plannerSelfExportSurplusGate`, but triggered on the **live** baseline
  grid (`grid_meter − Σ battery_w`) rather than the plan's forecasted
  grid — for the slot we're already in, live measurements override the
  (possibly-stale) forecast.

  Reactive discharge on live import is unchanged: a passive-arbitrage
  idle slot with the meter importing still allows the battery to cover
  the load. The change is one-sided — block reactive charging when the
  meter shows export potential the forecast missed.

  Found 2026-05-28 on a sunny May afternoon with a wildly over-estimated
  load forecast: planner expected ~2.8 kW load vs. actual ~0.5 kW, picked
  idle on net-≈0 forecast, and the dispatcher charged 2.6 kW into the
  battery despite high current spot (160 öre), low future spot (95 öre),
  and abundant forecast PV in upcoming slots.

## 0.106.0

### Minor Changes

- 9638c78: **Ferroamp self-healing watchdog for the sticky-pplim trap.** When the
  SSO reports the post-incident signature — DC bus voltage > 200 V, zero
  PV current, no fault, relay closed — continuously for ten minutes, the
  driver now auto-publishes `pplim arg=<pplim_release_w>` to release
  the lock. Operator opts in by setting `config.pplim_release_w > 0`;
  without it, the watchdog logs a per-incident warning but does not
  publish (we have no safe release value to send).

  A five-minute cooldown between successive recoveries prevents command-
  spam if the release doesn't take. A new `stuck_pv_recovery_count`
  metric tracks lifetime recovery count so operators can alert on a
  chronic condition.

  Reuses the existing `pplim_release_w` field — same value, dual
  purpose (dispatcher `curtail_disable` release AND watchdog
  self-recovery).

  Layered with [#367](https://github.com/frahlg/forty-two-watts/pull/367)
  (driver hard-fail on `pplim arg=0`) and the dispatcher fix in the
  parallel PR (`fix(curtail): no spurious release ...`) this is the
  third and final layer of defense against the 2026-05-27 brick.

### Patch Changes

- 312e9ba: **Defense-in-depth against the 2026-05-27 Ferroamp brick.** Two
  independent changes that, combined with PR #367's driver-side hard
  fail on `pplim arg=0`, eliminate every known trigger path:

  - **Dispatcher**: `ComputePVCurtail` no longer emits a `curtail_disable`
    release simply because a previously-curtailed driver dropped out of
    the proportional allocation due to its own `|PV|` crashing to ~0
    (often a direct consequence of OUR curtail throttling that driver
    down). The release is now only sent when the curtail directive
    truly clears, or the driver is removed from `SupportsPVCurtail`, or
    the driver goes offline. Also: per-driver allocations rounding to
    `≤ 1 W` are suppressed entirely — never publish a near-zero
    `pplim` that some inverters treat as a hard "limit to 0 W" lock.

  - **Ferroamp driver**: subscribes to `extapi/control/response`
    (was: `extapi/result` — wrong topic, never received anything),
    parses `{"status":"ack|nak", ...}` responses, and exposes
    cumulative `extapi_nak_count` + `extapi_ack_count` metrics. NAK
    responses are also logged as warnings with `transId` + `msg`
    fields. The 2026-05-27 brick was preceded by minutes of
    `nak: no available ESOs detected in system` that we couldn't see
    through ftw telemetry — now the operator can alert on any non-zero
    NAK rate.

  Tests added:

  - Four new dispatcher regressions in `control/pv_curtail_test.go`
    guarding the suppression / release semantics.
  - One driver test in `drivers/lua_ferroamp_curtail_test.go`
    asserting NAK + ACK counter advancement.

- 322ffe2: **Ferroamp safety fix:** the Lua driver now refuses to publish
  `pplim arg=0` from any `curtail` / `curtail_disable` path.

  Ferroamp's extapi treats `{"cmd":{"name":"pplim","arg":0}}` as
  "limit PV output to 0 W" — same wire bytes as a naive release would
  have, opposite semantics. The inverter sticks at 0 W PV until the
  operator clears pplim from the Ferroamp portal or power-cycles the
  EnergyHub. On 2026-05-27 this fired against a live SE4 site after the
  dispatcher's proportional curtail allocation gave a 0-share to
  Ferroamp; recovery required a 30+ minute outage and a portal-side
  reset.

  Changes:

  - `curtail` with `power_w <= 0` is now a logged no-op (was: published
    `pplim arg=0`).
  - `curtail_disable` is a logged no-op by default (was: published
    `pplim arg=0`). To restore automatic release, set
    `config.pplim_release_w` on the driver to the inverter's nominal
    max (e.g. `15000` for a 15 kW SSO). The driver then publishes
    `pplim arg=<release_w>` which Ferroamp accepts as "raise the limit".
  - New unit tests guard the wire payload against any regression that
    reintroduces `pplim arg=0`.
  - Docs in `docs/configuration.md` describe the trap and the new
    config field.

  Operators with `supports_pv_curtail: true` on Ferroamp **should** add
  `config.pplim_release_w: <SSO-rated-watts>` to keep curtailment
  auto-releasing. Without it, curtail still engages correctly, but
  release becomes a portal action.

## 0.105.0

### Minor Changes

- c206f4c: **Friend-types-code redesign of the pair-approval flow.** v0.104.0 shipped the code on both sides (dashboard + friend's landing page) and required the operator to type it back in — confusing UX, and the cross-origin POST from the LAN dashboard to the public relay was blocked by CORS so the Allow button silently did nothing.

  New flow:

  - Dashboard displays the 4-digit code along with the URL for the operator to share. "Copy code" and "Copy URL + code" buttons make the bundle easy to send in one Signal/SMS message.
  - The relay's landing page **no longer shows the code**. It shows an input field. The friend types the code they received separately from the host.
  - POST happens same-origin (browser → relay), no CORS surprises.
  - On success, the page reveals the dashboard URL + the `claude mcp add` one-liner.

  Security model is unchanged in substance — possession of (URL + code) activates the session — but the flow now matches the operator's mental model (share both, friend types code). The host no longer has to be live at connect-time to approve.

  Tests adjusted: relay landing-page test now asserts the code is **NOT** present in the served HTML; component source-hygiene tests assert the operator-side input field is gone. 31 node-tests + Go test suite all green.

## 0.104.0

### Minor Changes

- 8e2c08f: **Pair-card v2 with real relay presence + voice-channel approval.** When the friend opens the relay URL, the dashboard now surfaces the full URL with a Copy button, the 4-digit voice-channel approval code in big numbers, and an inline Allow form that POSTs the typed code straight to the relay's `/h/<token>/approve` once the operator hears the matching digits from their friend on voice. The misleading "0 clients connected" counter is replaced with a live presence indicator (live / active / idle / pending / dead) driven by a new `GET /tunnel/sessions/<token>/info` endpoint on the relay that tracks landing-page hits + last-tunneled-request timestamps; ftw-pair polls it every heartbeat and forwards the snapshot to `/api/pair/status`.

  The friend-message template is rewritten for the URL flow — no more `curl install-ftw-connect.sh` references, no more old binary install path. Operator-facing security: if the friend reads back a code that doesn't match the one shown on the dashboard, the validator refuses to approve and warns "leaked URL".

  Pure render helpers split into `web/components/ftw-pair-card-render.js` and covered by 42 `node --test` cases (state-machine snapshots, golden-string assertions on the friend message, source-hygiene checks that catch regressions where someone re-introduces `ftw-connect` references). Run with `npm test` from the repo root.

### Patch Changes

- cf93ada: Internal groundwork for owner remote access via passkey: adds the `trusted_devices` table in state.db with full CRUD (`SaveTrustedDevice`, `LoadTrustedDevices`, `LookupTrustedDevice`, `UpdateTrustedDeviceSignCount`, `DeleteTrustedDevice`) and pulls in `github.com/go-webauthn/webauthn` as a direct dependency. No user-visible surface yet — the host endpoints, relay `/me/<site-id>` routing, and enrollment/login UIs land in follow-up commits on this branch.

## 0.103.0

### Minor Changes

- 8841201: Disable the passive-arbitrage PV-charge bonus by default (was 30 öre/kWh).

  The bonus credited each kWh of battery charge fed from live PV surplus,
  intended to break ties when the DP saw "store PV now" and "export PV
  now, reimport later" as economically equivalent. In practice the import
  tariff + VAT asymmetry already makes storage strictly preferred under
  typical retail pricing, so the bonus was redundant.

  The redundancy is harmless on flat-price days, but on days with future
  negative-price hours the bonus pulled morning battery charging forward
  to the point where no SoC headroom remained when the negative-price
  window arrived — forcing PV export against negative prices instead of
  absorbing the (paid-to-consume) energy into the battery.

  Behavior change: operators who relied on the bonus can re-enable it
  explicitly via `planner.pv_charge_bonus_ore_kwh` in `config.yaml`.
  The previous fallback that silently reinstated 30 öre/kWh when the
  value was set to 0 has also been removed — 0 now means 0.

- 476a13c: Expose `CHARGE_CEIL_SOC` and `DISCHARGE_FLOOR_SOC` in the Ferroamp Lua
  driver as operator-tunable YAML config fields.

  ```yaml
  - name: ferroamp
    config:
      charge_ceil_soc: 1.0 # default 0.95 — charge all the way to 100%
      discharge_floor_soc: 0.05 # default 0.15 — discharge down to 5%
  ```

  Both fields are optional and default to the existing constants, so
  existing configurations behave identically. Out-of-range or
  non-numeric values are logged as warnings and the default is kept. To
  actually reach 100 % SoC the operator must also raise
  `planner.soc_max_pct` — the planner cap and the driver cap are two
  independent layers.

- bfc1504: **Replace `ftw-connect` with a URL on `relay.fortytwowatts.com`.** Friend opens a browser to `/h/<6-word-token>`, sees a 4-digit code, reads it to the host on voice, host clicks Allow on the dashboard. Then both Claude Code (`--transport http https://relay.../h/<token>/mcp`) and the web dashboard (`/h/<token>/web/`) work for the rest of the TTL.

  Under the hood: new `ftw-relay` HTTPS request-response relay (linux/amd64 + linux/arm64 release assets), new `internal/tunnel` long-poll protocol, rewired `ftw-pair` host loop. Deletes `ftw-connect`, `ftw-subetha`, `internal/subetha`, the curl installer script, and the old `docs/subetha-deploy.md` runbook. Operator deploys the new relay per `docs/relay-deploy.md` (Cloudflare Origin Cert + systemd, ~15 min).

  Known temporary regression: the dashboard's "friend connected" counter always shows 0 until a follow-up PR wires it through a new relay-side sessions endpoint.

### Patch Changes

- 7b95ce9: Switch the release pipeline from semantic-release to Changesets.

  - `.changeset/*.md` files drive the next version bump + CHANGELOG entry.
  - A "Version Packages" PR opens automatically when changesets accumulate
    on master; merging it cuts the tag and runs the binaries / docker /
    rpi-image / Discord jobs unchanged.
  - PRs to master are now gated on the `changeset-check` workflow — add a
    changeset with `npx changeset`, or apply the `no-changeset` label for
    pure docs / CI / chore PRs.
  - Hitchhiker codename header preserved via `scripts/apply-codename.cjs`.

## [0.8.0](https://github.com/frahlg/forty-two-watts/compare/v0.7.0...v0.8.0) (2026-04-16)

### Features

- **drivers/zap:** aggregate PV from attached inverters ([fb8ca88](https://github.com/frahlg/forty-two-watts/commit/fb8ca8869bea4cac079f68fd9d66a96e7428aac3))
- **drivers:** add Sourceful Zap meter driver ([f1877cc](https://github.com/frahlg/forty-two-watts/commit/f1877cc5b6abdfc7634fbfb07ccdedc927342144))

### Bug Fixes

- key local-vs-cloud HTTP on connection_defaults.host ([5b30477](https://github.com/frahlg/forty-two-watts/commit/5b3047711d7410ef68dff75280a4f1f262a4a55b)), closes [#76](https://github.com/frahlg/forty-two-watts/issues/76)

## [0.7.0](https://github.com/frahlg/forty-two-watts/compare/v0.6.1...v0.7.0) (2026-04-16)

### Features

- **drivers:** align Solis + Deye control with Zap reference ([#74](https://github.com/frahlg/forty-two-watts/issues/74)) ([281f4df](https://github.com/frahlg/forty-two-watts/commit/281f4dfc8027acfedb9ac8ea7ad6fba290ee30c0))

## [0.6.1](https://github.com/frahlg/forty-two-watts/compare/v0.6.0...v0.6.1) (2026-04-16)

### Bug Fixes

- add HTTP capability support for catalog drivers + clarify grid tariff label ([#75](https://github.com/frahlg/forty-two-watts/issues/75)) ([d4cc95e](https://github.com/frahlg/forty-two-watts/commit/d4cc95e21df5853af82f0f11fd69d762a96f353e))

## [0.6.0](https://github.com/frahlg/forty-two-watts/compare/v0.5.2...v0.6.0) (2026-04-16)

### Features

- EV driver UI + lifecycle controls + creds visibility ([#73](https://github.com/frahlg/forty-two-watts/issues/73)) ([52a482a](https://github.com/frahlg/forty-two-watts/commit/52a482a81701ec0e9da2bdfa94e06ca03f5fa21b))

### Bug Fixes

- 3 P1 + 1 P2 from Codex + UI cleanup ([48e0d28](https://github.com/frahlg/forty-two-watts/commit/48e0d2865beac703805765ab238058565f1e91e7))

### UI

- move EV credentials to Devices tab, remove EV Charger tab ([7cd2d9f](https://github.com/frahlg/forty-two-watts/commit/7cd2d9f3af4a547cf9370c29614607b764d9e59f))

## [0.5.3](https://github.com/frahlg/forty-two-watts/compare/v0.5.2...v0.5.3) (2026-04-16)

### Bug Fixes

- 3 P1 + 1 P2 from Codex + UI cleanup ([48e0d28](https://github.com/frahlg/forty-two-watts/commit/48e0d2865beac703805765ab238058565f1e91e7))

### UI

- move EV credentials to Devices tab, remove EV Charger tab ([7cd2d9f](https://github.com/frahlg/forty-two-watts/commit/7cd2d9f3af4a547cf9370c29614607b764d9e59f))

## [0.5.2](https://github.com/frahlg/forty-two-watts/compare/v0.5.1...v0.5.2) (2026-04-16)

### Bug Fixes

- 4 wizard review bugs — path traversal, /setup route, scan API, skip validation ([#70](https://github.com/frahlg/forty-two-watts/issues/70)) ([f691015](https://github.com/frahlg/forty-two-watts/commit/f691015fe154f59e4ce24914674ea924184f556a))

## [0.5.1](https://github.com/frahlg/forty-two-watts/compare/v0.5.0...v0.5.1) (2026-04-16)

### Bug Fixes

- prevent driver paths from accumulating "../" on each config save ([790429f](https://github.com/frahlg/forty-two-watts/commit/790429f79b56281e5fe5875cc6c51e2d3e05572e))

## [0.5.0](https://github.com/frahlg/forty-two-watts/compare/v0.4.0...v0.5.0) (2026-04-16)

### Features

- add setup wizard frontend (web/setup.html + web/setup.js) ([#66](https://github.com/frahlg/forty-two-watts/issues/66)) ([bc1a285](https://github.com/frahlg/forty-two-watts/commit/bc1a2850e8f15c2d1d6d483be6ed627df7b76f5b))
- bootstrap mode + network scanner for onboarding wizard ([#67](https://github.com/frahlg/forty-two-watts/issues/67)) ([267cef4](https://github.com/frahlg/forty-two-watts/commit/267cef42481ee8515abe0ef26ebb5721650d414e))
- wizard dashboard trigger + driver catalog enrichment ([#68](https://github.com/frahlg/forty-two-watts/issues/68)) ([78c83cf](https://github.com/frahlg/forty-two-watts/commit/78c83cf207bf0664e17dabca6c988fdb6f0e5e81))

## [0.4.0](https://github.com/frahlg/forty-two-watts/compare/v0.3.0...v0.4.0) (2026-04-16)

### Features

- config/UI improvements — kWh display, secure EV password, planner tab ([#65](https://github.com/frahlg/forty-two-watts/issues/65)) ([35ab03d](https://github.com/frahlg/forty-two-watts/commit/35ab03d7b5f63ffcc471bf28e1409d761bf0f7d2))
- Easee Cloud driver + host.http_get/post for Lua drivers ([#56](https://github.com/frahlg/forty-two-watts/issues/56)) ([4cdc942](https://github.com/frahlg/forty-two-watts/commit/4cdc9421590385e8f00301925d590f6fb093ebaf))
- EV charger config + credential masking in API responses ([#58](https://github.com/frahlg/forty-two-watts/issues/58)) ([c22cb80](https://github.com/frahlg/forty-two-watts/commit/c22cb805af960bcafc353846f62e2406fc791e17))

### Bug Fixes

- 5 Go-side P1 bugs from Codex review ([#46](https://github.com/frahlg/forty-two-watts/issues/46)) ([0cd2885](https://github.com/frahlg/forty-two-watts/commit/0cd2885bdb79d6a4c3116bb4930ec785cea8f944))
- 5 Go-side P1 bugs from Codex review ([#47](https://github.com/frahlg/forty-two-watts/issues/47)) ([4f2eaf6](https://github.com/frahlg/forty-two-watts/commit/4f2eaf69f626caddf2bae456ac047301f9a36840))
- address P2 review comments across control, MPC, drivers, and UI ([#64](https://github.com/frahlg/forty-two-watts/issues/64)) ([fcafa88](https://github.com/frahlg/forty-two-watts/commit/fcafa88f12c714a1930342dd9f28ea07d18440c2))
- **ci:** disable @semantic-release/github PR annotation features ([4020d46](https://github.com/frahlg/forty-two-watts/commit/4020d4606e0f81924cca5d0e06f4ab743bf8f1d5)), closes [#32](https://github.com/frahlg/forty-two-watts/issues/32) [#33](https://github.com/frahlg/forty-two-watts/issues/33) [#34](https://github.com/frahlg/forty-two-watts/issues/34) [#35](https://github.com/frahlg/forty-two-watts/issues/35) [#36](https://github.com/frahlg/forty-two-watts/issues/36) [#39](https://github.com/frahlg/forty-two-watts/issues/39)
- **ci:** switch semantic-release to conventionalcommits preset ([7e0bb89](https://github.com/frahlg/forty-two-watts/commit/7e0bb895f7a8f8271033336899bed8639e772dc4))
- **ci:** upgrade GitHub Actions to Node.js 24 (drop deprecated Node 20) ([4005bd8](https://github.com/frahlg/forty-two-watts/commit/4005bd8b982c091bff4dcd428cebbe1a08447242))
- Lua driver Command() reading wrong field — Sungrow ignored targets ([9237156](https://github.com/frahlg/forty-two-watts/commit/923715691d55c9dc5c3058b72271d00a72d9c93a))
- populate EV Charger tab from driver config when ev_charger is empty ([5e6b116](https://github.com/frahlg/forty-two-watts/commit/5e6b11676bc972a2c983d39a345a3b5f8dbc77dc))
- remove dead evSlider event listeners that crash app.js ([8ae76c7](https://github.com/frahlg/forty-two-watts/commit/8ae76c710b4ca2d15eb71399211849c4ce03a4bb))
- replace wonky Catmull-Rom spline with simple linear forecast ([abea431](https://github.com/frahlg/forty-two-watts/commit/abea431d7895504116600384c6a92e9577675607))
- show '...' instead of stale v0.1.0 while JS loads version ([dc65065](https://github.com/frahlg/forty-two-watts/commit/dc65065784cad8c018f64338284b5f4b6441ac22))
- **solaredge_pv:** read SunSpec scale factors every poll, not cached ([#38](https://github.com/frahlg/forty-two-watts/issues/38)) ([26f8793](https://github.com/frahlg/forty-two-watts/commit/26f8793f22888dc11d29fd157b10b4340da34c8d))

### Drivers

- add Eastron SDM630 Lua driver ([#18](https://github.com/frahlg/forty-two-watts/issues/18)) ([d5ad806](https://github.com/frahlg/forty-two-watts/commit/d5ad8066377371eb63f320969d153ece50d1266a))
- add Ferroamp Modbus driver (alt transport to ferroamp.lua) ([#31](https://github.com/frahlg/forty-two-watts/issues/31)) ([03b802c](https://github.com/frahlg/forty-two-watts/commit/03b802cefcd1f4d2e07ad05f493ca5643585ed0c))
- fix 9 P1 bugs flagged by Codex review ([#44](https://github.com/frahlg/forty-two-watts/issues/44)) ([b20e485](https://github.com/frahlg/forty-two-watts/commit/b20e485f5fa0a5a20d3a4e83d49410528f81ea1e))
- port Deye SUN-SG hybrid inverter to 42W v2.1 Lua host ([#29](https://github.com/frahlg/forty-two-watts/issues/29)) ([df8fbc0](https://github.com/frahlg/forty-two-watts/commit/df8fbc006375dfc2a3abeb2bc8ec0f01f3e1d0e1))
- port Fronius GEN24 (SunSpec) to Lua ([#19](https://github.com/frahlg/forty-two-watts/issues/19)) ([c1fc875](https://github.com/frahlg/forty-two-watts/commit/c1fc87559b404aa0429ed8ca0a71539e634cb59d))
- port Fronius Smart Meter (SunSpec Modbus, read-only) ([#24](https://github.com/frahlg/forty-two-watts/issues/24)) ([575895c](https://github.com/frahlg/forty-two-watts/commit/575895c7469283bd139deb481e601068045f7519))
- port GoodWe hybrid inverter (ET-Plus / EH) to Lua v2.1 ([#28](https://github.com/frahlg/forty-two-watts/issues/28)) ([e43d2d9](https://github.com/frahlg/forty-two-watts/commit/e43d2d92ef1a7fd26c65b839944bc8d98fa4915a))
- port Growatt hybrid inverter driver (read-only) ([#20](https://github.com/frahlg/forty-two-watts/issues/20)) ([92524ac](https://github.com/frahlg/forty-two-watts/commit/92524acdd890507873a6d5f54b3b6d4335b8e610))
- port Huawei SUN2000 hybrid inverter ([#15](https://github.com/frahlg/forty-two-watts/issues/15)) ([09a8855](https://github.com/frahlg/forty-two-watts/commit/09a88558d0ae17c7e6bdd26387c663badb55e37b))
- port Kostal Plenticore / Piko IQ (Lua, read-only) ([#21](https://github.com/frahlg/forty-two-watts/issues/21)) ([bdeca96](https://github.com/frahlg/forty-two-watts/commit/bdeca96e6c3e05cfe968e20ceb298221f2be5c84))
- port Pixii PowerShaper battery driver to v2.1 Lua host ([#22](https://github.com/frahlg/forty-two-watts/issues/22)) ([70a96d1](https://github.com/frahlg/forty-two-watts/commit/70a96d1120b2aab2cb12ef49688fe3cb204789e3))
- port SMA hybrid inverter Lua driver ([#23](https://github.com/frahlg/forty-two-watts/issues/23)) ([dd34555](https://github.com/frahlg/forty-two-watts/commit/dd3455577c7a3adebad252f81d40b81d3b982350))
- port Sofar HYD-ES/HYD-EP from hugin to Lua v2.1 ([#26](https://github.com/frahlg/forty-two-watts/issues/26)) ([14f6131](https://github.com/frahlg/forty-two-watts/commit/14f6131952b033381a5501f76265714a2b985f1c))
- port SolarEdge SunSpec inverter + meter to Lua (read-only) ([#30](https://github.com/frahlg/forty-two-watts/issues/30)) ([1007e63](https://github.com/frahlg/forty-two-watts/commit/1007e63f9d1908f3210d9b80037e4a6e05e3fa78))
- port Solis hybrid inverter ([#27](https://github.com/frahlg/forty-two-watts/issues/27)) ([98b2a50](https://github.com/frahlg/forty-two-watts/commit/98b2a50ccf59c45130de951dd22db4fc17a67a1a))
- port Victron Energy GX Modbus driver ([#25](https://github.com/frahlg/forty-two-watts/issues/25)) ([ad71db2](https://github.com/frahlg/forty-two-watts/commit/ad71db269438e7aa6e11c632ba1db10897db81be))

### UI

- add status bar with driver health indicators ([b048d60](https://github.com/frahlg/forty-two-watts/commit/b048d60a57049385c498cc4e592ee049a3a05809))
- EV status card + Easee control commands ([#59](https://github.com/frahlg/forty-two-watts/issues/59)) ([b03749a](https://github.com/frahlg/forty-two-watts/commit/b03749ac9ae670447a201e65ed4a57e0db4e99d8))
- fix summary cards grid for 7 cards + raise side-by-side breakpoint ([6e19973](https://github.com/frahlg/forty-two-watts/commit/6e1997312df8ca5b889000d286d0b0782059b701))
- inline target on hover + driver card + collapsible model cards ([de88f43](https://github.com/frahlg/forty-two-watts/commit/de88f4326e5aa5587b623cde76371c0f410eff27))
- legend wrap + nice-tick y-axis + cleaner chart labels ([#33](https://github.com/frahlg/forty-two-watts/issues/33)) ([aeb1d1c](https://github.com/frahlg/forty-two-watts/commit/aeb1d1cb2ab6d69984cdcd424cb6c3da7d775407))
- remove manual EV charging slider ([063174c](https://github.com/frahlg/forty-two-watts/commit/063174cc259d46185da34bad827c16994a3c6e33))
- show mode band in plan chart + grid target on status card ([877e0bd](https://github.com/frahlg/forty-two-watts/commit/877e0bde83964ddb26ce4894ab0adc446fd7801b))
- smooth Catmull-Rom spline for forecast + 15min forecast zone ([dba51a5](https://github.com/frahlg/forty-two-watts/commit/dba51a54c26e6329a4eca850b81b4a22974efcfd))

### Control loop

- fold live DerEV readings into the EV clamp ([#36](https://github.com/frahlg/forty-two-watts/issues/36)) ([5d57d68](https://github.com/frahlg/forty-two-watts/commit/5d57d68c50e6a417b45695bd3ccf551e8566277a))
- slew-rate anchors on actual battery power, not stale command ([#41](https://github.com/frahlg/forty-two-watts/issues/41)) ([4f73f19](https://github.com/frahlg/forty-two-watts/commit/4f73f19abfb6e322a4934d9e9bb46b645afd1352))

### MPC planner

- fall back to forecast when learned PV twin collapses ([#39](https://github.com/frahlg/forty-two-watts/issues/39)) ([f3062ac](https://github.com/frahlg/forty-two-watts/commit/f3062acdd54206de8287b0a9af3862a13cb13105))
- log optimize params + ems_mode per action for plan chart ([9e8c14b](https://github.com/frahlg/forty-two-watts/commit/9e8c14bd388b869091c2315bd4a42def648bf987))
- value SoC at import−export spread in self-consumption modes ([#40](https://github.com/frahlg/forty-two-watts/issues/40)) ([a90d525](https://github.com/frahlg/forty-two-watts/commit/a90d5259209ca9fd8094927b060f62633dd3b5d0))

### Telemetry

- add DerEV type for EV charger readings ([#34](https://github.com/frahlg/forty-two-watts/issues/34)) ([65c9e2c](https://github.com/frahlg/forty-two-watts/commit/65c9e2c23b5f3eb7cb55fd952be7e724b2270e17))

### TSDB

- long-format SQLite (14d) + Parquet rolloff for older ([c53c964](https://github.com/frahlg/forty-two-watts/commit/c53c964e825c896fc0cf760a21ee7b0e29421d2f))

### Safety

- watchdog marks stale drivers offline + reverts to autonomous ([519196c](https://github.com/frahlg/forty-two-watts/commit/519196c01255db3947774bb8a267961b755d261e))

## v0.4.0-alpha (2026-04-16)

First public alpha. Running in production on real hardware but API and config format may still change. See the full changelog below or the [README](README.md) for what the system can do.

### Highlights

- **19 Lua drivers** — Sungrow, Solis, Huawei, Deye, SMA, Fronius, SolarEdge, Kostal, GoodWe, Growatt, Sofar, Victron, Ferroamp (MQTT + Modbus), Pixii, Eastron SDM630, Fronius Smart Meter, Easee Cloud
- **MPC planner** — 48h dynamic programming with three strategies (self-consumption, cheap charging, arbitrage)
- **EV charging** — Easee Cloud integration + OCPP 1.6J Central System
- **Digital twins** — self-learning PV, load, and price models
- **Pure Go + Lua** — single static binary, no Rust, no WASM, no CGo
- **Web dashboard** with real-time power flow, planner visualization, and full config UI
- **Home Assistant** MQTT autodiscovery

---

## Auto-generated changelog (internal)

## [2.3.0](https://github.com/frahlg/forty-two-watts/compare/v2.2.6...v2.3.0) (2026-04-16)

### Features

- config/UI improvements — kWh display, secure EV password, planner tab ([#65](https://github.com/frahlg/forty-two-watts/issues/65)) ([35ab03d](https://github.com/frahlg/forty-two-watts/commit/35ab03d7b5f63ffcc471bf28e1409d761bf0f7d2))

## [2.2.6](https://github.com/frahlg/forty-two-watts/compare/v2.2.5...v2.2.6) (2026-04-16)

### Bug Fixes

- populate EV Charger tab from driver config when ev_charger is empty ([5e6b116](https://github.com/frahlg/forty-two-watts/commit/5e6b11676bc972a2c983d39a345a3b5f8dbc77dc))

## [2.2.5](https://github.com/frahlg/forty-two-watts/compare/v2.2.4...v2.2.5) (2026-04-16)

### Bug Fixes

- address P2 review comments across control, MPC, drivers, and UI ([#64](https://github.com/frahlg/forty-two-watts/issues/64)) ([fcafa88](https://github.com/frahlg/forty-two-watts/commit/fcafa88f12c714a1930342dd9f28ea07d18440c2))

## [2.2.4](https://github.com/frahlg/forty-two-watts/compare/v2.2.3...v2.2.4) (2026-04-16)

### Bug Fixes

- replace wonky Catmull-Rom spline with simple linear forecast ([abea431](https://github.com/frahlg/forty-two-watts/commit/abea431d7895504116600384c6a92e9577675607))

### UI

- add status bar with driver health indicators ([b048d60](https://github.com/frahlg/forty-two-watts/commit/b048d60a57049385c498cc4e592ee049a3a05809))
- smooth Catmull-Rom spline for forecast + 15min forecast zone ([dba51a5](https://github.com/frahlg/forty-two-watts/commit/dba51a54c26e6329a4eca850b81b4a22974efcfd))

## [2.2.3](https://github.com/frahlg/forty-two-watts/compare/v2.2.2...v2.2.3) (2026-04-16)

### Bug Fixes

- remove dead evSlider event listeners that crash app.js ([8ae76c7](https://github.com/frahlg/forty-two-watts/commit/8ae76c710b4ca2d15eb71399211849c4ce03a4bb))

### UI

- fix summary cards grid for 7 cards + raise side-by-side breakpoint ([6e19973](https://github.com/frahlg/forty-two-watts/commit/6e1997312df8ca5b889000d286d0b0782059b701))

## [2.2.2](https://github.com/frahlg/forty-two-watts/compare/v2.2.1...v2.2.2) (2026-04-16)

### Bug Fixes

- show '...' instead of stale v0.1.0 while JS loads version ([dc65065](https://github.com/frahlg/forty-two-watts/commit/dc65065784cad8c018f64338284b5f4b6441ac22))

## [2.2.1](https://github.com/frahlg/forty-two-watts/compare/v2.2.0...v2.2.1) (2026-04-16)

### Bug Fixes

- **ci:** disable @semantic-release/github PR annotation features ([4020d46](https://github.com/frahlg/forty-two-watts/commit/4020d4606e0f81924cca5d0e06f4ab743bf8f1d5)), closes [#32](https://github.com/frahlg/forty-two-watts/issues/32) [#33](https://github.com/frahlg/forty-two-watts/issues/33) [#34](https://github.com/frahlg/forty-two-watts/issues/34) [#35](https://github.com/frahlg/forty-two-watts/issues/35) [#36](https://github.com/frahlg/forty-two-watts/issues/36) [#39](https://github.com/frahlg/forty-two-watts/issues/39)
- **ci:** switch semantic-release to conventionalcommits preset ([7e0bb89](https://github.com/frahlg/forty-two-watts/commit/7e0bb895f7a8f8271033336899bed8639e772dc4))
- **ci:** upgrade GitHub Actions to Node.js 24 (drop deprecated Node 20) ([4005bd8](https://github.com/frahlg/forty-two-watts/commit/4005bd8b982c091bff4dcd428cebbe1a08447242))

### UI

- remove manual EV charging slider ([063174c](https://github.com/frahlg/forty-two-watts/commit/063174cc259d46185da34bad827c16994a3c6e33))

# [2.2.0](https://github.com/frahlg/forty-two-watts/compare/v2.1.0...v2.2.0) (2026-04-16)

### Features

- EV charger config + credential masking in API responses ([#58](https://github.com/frahlg/forty-two-watts/issues/58)) ([c22cb80](https://github.com/frahlg/forty-two-watts/commit/c22cb805af960bcafc353846f62e2406fc791e17))

# [2.1.0](https://github.com/frahlg/forty-two-watts/compare/v2.0.1...v2.1.0) (2026-04-16)

### Features

- Easee Cloud driver + host.http_get/post for Lua drivers ([#56](https://github.com/frahlg/forty-two-watts/issues/56)) ([4cdc942](https://github.com/frahlg/forty-two-watts/commit/4cdc9421590385e8f00301925d590f6fb093ebaf))

## [2.0.1](https://github.com/frahlg/forty-two-watts/compare/v2.0.0...v2.0.1) (2026-04-16)

### Bug Fixes

- 5 Go-side P1 bugs from Codex review ([#46](https://github.com/frahlg/forty-two-watts/issues/46)) ([0cd2885](https://github.com/frahlg/forty-two-watts/commit/0cd2885bdb79d6a4c3116bb4930ec785cea8f944))
- 5 Go-side P1 bugs from Codex review ([#47](https://github.com/frahlg/forty-two-watts/issues/47)) ([4f2eaf6](https://github.com/frahlg/forty-two-watts/commit/4f2eaf69f626caddf2bae456ac047301f9a36840))
- **solaredge_pv:** read SunSpec scale factors every poll, not cached ([#38](https://github.com/frahlg/forty-two-watts/issues/38)) ([26f8793](https://github.com/frahlg/forty-two-watts/commit/26f8793f22888dc11d29fd157b10b4340da34c8d))
