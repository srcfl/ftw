# FTW roadmap

The [product vision](../VISION.md) sets the direction. This roadmap turns it
into user outcomes and acceptance evidence. It is not a list of shipped
features or permission to start every track at once.

Fredrik chooses the next bounded change. Check current code, tests, open PRs
and site evidence before claiming a gap or completion. Preserve established
behaviour while simplifying the product. Bug fixes, security, recovery and
necessary maintenance continue alongside product work.

## Implementation review, 12 September 2026

This baseline checks Core `f1a3b765`, webapp `ff7af033`, native app
`79fdd0e8`, drivers `7e594655`, and website `d26e14f7`, after the shared
vision changes merged. It combines source review, repository tests and a local
webapp simulator rendered in a browser. It is not an audit of every installed
box, hardware combination or measured saving.

Much of the required foundation exists. The next work should close gaps in
that foundation and complete daily flows, rather than replace the planner or
add another control service.

### Close confirmed control gaps first

Core already has a [site freshness gate](../go/cmd/ftw/site_dispatch_safety.go),
[device-fault exclusion and retries](../go/cmd/ftw/driver_failure_default.go),
and [planner filtering of available batteries](../go/internal/mpc/service.go).
These need to work through every path. Current legacy paths still differ:

- [LuaDriver.Command](../go/internal/drivers/lua.go) returns success when
  `driver_command` is absent.
- [siteLoadW](../go/internal/control/dispatch.go) sums cached battery and PV
  watts without excluding offline drivers. In the existing regression cases,
  a 500 W house becomes 4,500 W or 5,500 W.
- The same dispatch file substitutes 10% for missing battery SoC. One
  regression case produces a -1,200 W battery target without a SoC reading.
- `liveCurtailLimitW` accepts an old or offline meter in isolated tests.
  The outer site gate already blocks stale-site dispatch; the inner helper
  still needs its own correct freshness contract.

Nine existing regression cases from
[#1170](https://github.com/srcfl/ftw/pull/1170) and
[#1199](https://github.com/srcfl/ftw/pull/1199) were run against this baseline
through a temporary Go test overlay: seven failed and two passed. No runtime
source was changed. These are gaps in the existing test coverage and code,
not regressions from the documentation merge.

Continue those PRs before adding wider actuation. Recheck their current diffs
and reviews: missing-SoC protection must survive slew and final clamps;
read-only metadata must agree with actual command enforcement; PV curtailment
needs the default-mode gate too. Verify the bundled driver pin through startup
with any stricter host rule. An earlier approval or a merged driver-source
change does not prove the currently pinned recovery bundle passes.

### Existing behaviour and remaining product work

| Area | What the baseline contains | What remains |
|---|---|---|
| Mixed equipment | Separate SolarEdge legacy and Pixii drivers exist. Their [published evidence](https://github.com/srcfl/device-drivers/blob/7e5946555245dee2150d46d1b7278aaec5ebe242/drivers/lua/solaredge_legacy.lua) and [Pixii metadata](https://github.com/srcfl/device-drivers/blob/7e5946555245dee2150d46d1b7278aaec5ebe242/drivers/lua/pixii.lua) still say experimental; the legacy header and declared curtail capability also need to agree. | Reconcile known field runs with the catalog, then verify the named inverter + battery + charger combination. Distinguish measured telemetry, verified commands and unverified control. Metadata is not evidence that no user has ever run the hardware. |
| Setup and usable power | Discovery, fingerprinting and per-device settings exist. [Planner battery limits](../go/cmd/ftw/main.go) use configured limits or a 0.5C estimate, then an aggregate fuse cap. | A guided commissioning result and measured usable-power learning are still missing from the reviewed setup path. Read verified limits first; learn response within them. A capacity-derived estimate is not a learned power limit. |
| Forecasts and defaults | [PV learning](../optimizer/native/README.md) needs no panel geometry or rating. [Cold-load selection](../go/cmd/ftw/forecast_tracking.go) keeps the site prior until learning, and [load/net-risk tests](../go/cmd/ftw/forecast_load_risk_test.go) cover uncertainty. Charge and discharge efficiency are included. The [minimum arbitrage spread](../go/internal/config/config.go) defaults to zero. | Verify first-day and learned performance on held-out site periods. Keep the separate opt-in wear-cost requirement open; a minimum arbitrage spread is not a general wear model. Align all persisted defaults and worker support before exposing such a setting. Do not rebuild cold-start support already delivered in #1204. |
| Daily charging | [The webapp panel](https://github.com/srcfl/ftw-webapp/blob/ff7af033fa3fcdeb38882e3ff365e8d6d7aba75a/src/views/EvPanel.svelte) already saves SoC on slider release, changes schedules without a Save button and supports Charge now. | [Now](https://github.com/srcfl/ftw-webapp/blob/ff7af033fa3fcdeb38882e3ff365e8d6d7aba75a/src/views/Now.svelte) normally opens that panel after a charger tap or notification link. Bring the relevant SoC action directly into the post-plug-in entry experience. Complete the goal → plan → delivered-energy flow on real chargers, including offline cars and restarts. |
| Notifications | Core and the webapp implement subscription, charging connection/completion/interruption events and device alerts. See the [shared push catalogue](../contract/push-catalogue.yaml) and [rule defaults](../go/internal/notifications/service.go). | There is no dedicated predicted-missed-departure event in that catalogue. Add an actionable goal-risk notification and make activation clear during charging setup, with user consent. Verify delivery while the app is closed; an interrupted-session alert alone does not cover a future shortfall. |
| Live trust and expert access | Flow exists. [LivePanel](https://github.com/srcfl/ftw-webapp/blob/ff7af033fa3fcdeb38882e3ff365e8d6d7aba75a/src/views/LivePanel.svelte) already puts a recent one-second trace behind each energy bubble and freezes it on silence. Core stores [structured v2 command results](../go/internal/state/driver_command_results.go), plan diagnostics and issued forecasts. | Join request, accepted intent, command response and measured effect in the normal experience and a structured analysis API, including legacy drivers and different sample cadences. Current result records and live watts are useful parts, not a complete proof of causality. Measure response time on a target box. |
| External control and agents | [Protocol command IDs and authorization leases](../go/internal/appproto/command.go), scoped operations, [bounded battery holds](../go/internal/api/api_battery_manual.go), schedule APIs and encrypted sessions exist. [HASS callbacks](../go/cmd/ftw/main.go) persist modes and grid targets. The built-in [Ask why tools](../go/internal/api/api_assistant_tools.go) are read-only. | Define renewable external control separately from durable goals. Losing HASS does not currently expire its saved mode. Existing authorization leases do not supply that policy. Build structured agent reads first, then permitted schedule/plan writes and a cloud MCP endpoint using the same Core checks. |
| Savings | [The API](../go/internal/api/api_savings.go) explicitly reports `site_total` against `no_pv_no_battery_vehicle_energy_at_daily_average`. Actual import cost and export revenue are available. | Make the scope clear on each surface that says “saved”. Then add and validate the same-hardware self-consumption counterfactual, including EV behaviour and stored-energy accounting. Do not relabel the current figure as FTW's incremental benefit. |
| Heat and settings | Thermal contracts and an [explicitly opted-in solar feed](../go/cmd/ftw/solar_feed_send.go) already exist. The on-box [planner settings](../web/settings/tabs/planner.js) and webapp use different levels of technical language; the on-box minimum SoC still says “House reserve”. | Keep existing opt-ins explicit while phase one uses heat data for planning. Align basic controls around user goals and distinguish operating limits from forecast caution. Audit stored settings before removing or hiding them. Active tank/hot-water optimization remains a later bounded outcome. |

### Recommended delivery order

These are proposed priorities, not permission to start every row in parallel
or dates promised to users. Each delivery should have a focused PR and a clear
result that the owner can review.

| Order | Delivery | Done when |
|---|---|---|
| 1 | Close the confirmed legacy control and host gaps in #1170 and #1199. | The failing baseline cases pass through the final command path; remaining review findings are resolved; the pinned driver set starts and reaches its safe defaults. |
| 2 | Complete everyday charging. | Plug in → open app → correct SoC → see accepted plan takes no extra navigation or Save. Recurring weekday goals, Charge now, restart recovery and goal-risk notifications work together on a named charger and offline-car setup. |
| 3 | Complete first-day commissioning and simple defaults. | A new mixed site reaches safe automatic operation with confirmed fuse/meter, minimal required input and a receipt for observed control. Wrong starting ratings and failed integrations have clear handling; learned power does not replace hard equipment limits. |
| 4 | Share live evidence with people and agents. | One structured path explains intent, command result, freshness and measured outcome. Both normal Flow and an authorized analysis agent can use it. Target-box latency and differing sampling rates are measured; forecast evaluation covers cold start and learned periods. |
| 5 | Complete external authority and fair value as separate focused changes. | Temporary control expires to a defined local default; durable goals persist; schedule/plan access can be revoked; cloud MCP does not expose data to the relay. Separately, the validated self-consumption comparison reports FTW's incremental value and missing evidence honestly. |
| Later | Bounded thermal control and further expert extensions. | A named tank/hot-water use case meets comfort, hardware and failure requirements without making ordinary household setup harder. Native expansion still follows its existing Pair + Now verification gates. |

Necessary safety, security, recovery and support fixes continue throughout.
Correct misleading value labels when their scope is known; that need not wait
for the new counterfactual model. Reading and analysis access for agents can
also support the evidence work before agents receive control authority.

## Current focus: a complete and trustworthy default experience

Make discovery, planning, control and feedback fit together for mixed hardware
and for both novice and expert users. Minimize setup and routine decisions.
Complete a user flow across Core and clients when needed, using paired PRs
for shared contracts.

Every row below is a target. Existing code contains parts of these flows;
the row is complete only when its evidence exists for the version under review.
This replaces older dated status snapshots. It does not reset completed work
or reopen closed issues.

| Outcome | Product requirement | Acceptance evidence |
|---|---|---|
| Simple setup and first-day value | Discover mixed equipment. Confirm the main fuse and site meter. Read battery capacity where possible and ask for kWh when needed. Power settings and solar kWp are optional where safe device information and learning allow. Provide useful initial load and PV forecasts. | A fresh install reaches useful automatic operation without panel drawings or expert settings. Missing data and wrong start estimates have tested behaviour. Record device identity, known limits and uncertainty. Validate on named hardware combinations as well as simulators. |
| A clear commissioning result | Check commands and measured response within known limits. Distinguish working telemetry from working control. Exclude failed control from both the plan and dispatch. | Show request, device response, measured effect and timing. Cover delayed response, refusal, disconnect, stale site data and recovery. A short commissioning test does not claim full hardware qualification. |
| Live control that earns trust | Keep the fast local feel. Make request, acceptance, command, response, physical effect and freshness visible in the normal Flow experience. | Browser review on desktop and mobile, timing measurements on a target box, and traces with different sampling rates. A pending command or old reading never appears as completed or fresh. Examine existing Live and Flow views before deciding their final layout. |
| Good automatic planning | Use site physics and charge/discharge efficiency. Default wear cost is zero; users may opt in. Keep hard SoC limits separate from forecast-based caution and explicit backup needs. | Cold-start and learned forecasts, stale inputs, multiple assets and unavailable optimizer paths have tests. Backtests use held-out periods and report uncertainty. Defaults and persisted settings agree across UI, Core and worker. Site runs establish practical benefit. |
| Reliable daily charging | Persistent weekday target and deadline. Offline-car estimates. Direct SoC slider after connection, no extra save, prompt replanning, one-action Charge now and notifications when action is needed. | Test from app intent to charger/car response and delivered energy, including missed-goal risk, absent vehicle cloud, unknown SoC, reconnect and restart. Test notifications with the app closed. Confirm physical charging separately from simulation. |
| Useful analysis and fair savings | Keep enough provenance to explain plans and outcomes. Main savings target compares with ordinary self-consumption on the same installation. | Actual cost reconciles with measured import/export and prices. Specify EV behaviour, initial and final stored-energy accounting, efficiency and coverage. Show missing and negative results. Label the current no-PV/no-battery comparison as total site value until replacement is verified. |
| External automation and agent access | Give authorized clients structured analysis data, schedule/goal changes and proposed-plan submission. Temporary external control expires; durable goals persist. Support local access and secure cloud MCP access. | Paired Core/client contract tests cover permissions, expiry, replay, rejection, revocation and reconnect. An agent can trace a request through to measured outcome. Prove local fallback when the caller disappears. Reuse the session/relay where suitable and verify that relay and escrow remain blind. Cloud MCP is a target, not a claim of a shipped endpoint. |
| Less configuration, reliable operation | Every normal setting serves a user need. Keep expert controls discoverable. Installation, updates, backup and recovery remain part of the finished experience. | Audit settings and feature use before removal. Test migration of stored choices so hidden settings cannot keep directing behaviour. Verify restart, upgrade and restore on a target box and review affected UI flows. |

Safety is part of each row. Core remains the only dispatch authority, every
plan is untrusted input, stale required site-meter data stops dispatch, and
failed devices receive their safe default where reachable.

## Heat: data first, bounded control later

First read heat-pump and heating data to improve load forecasts and planning.
Leave comfort control with the heat pump. The battery and other flexible
assets serve the household's needs.

A later control case is a buffer tank or hot-water store charged during cheap
periods. It needs known temperature and storage bounds, safe defaults, verified
hardware control and a user goal. Do not imply active heat support from a
telemetry-only driver or require every house to supply a thermal model.

## Client and repository responsibilities

| Repository | Responsibility in this direction |
|---|---|
| `srcfl/ftw` | Core safety, state, commissioning, dispatch, on-box UI, history, API and shared product direction. |
| `srcfl/energyplan` | Private solver and forecast implementation; defaults and model quality must match Core's contract. Only compiled artifacts and public integration metadata go to Core. |
| `srcfl/device-drivers` | Mixed-device support, stable identity, trustworthy readings, declared limits, structured command results and hardware evidence. |
| `srcfl/ftw-webapp` | Fast everyday UI, charging interaction, intent/result feedback, notifications and encrypted client/session contracts. |
| `srcfl/ftw-app` | The same product principles, with current Pair + Now verification gates preserved before expanding native scope. |
| `srcfl/ftw-web` | Explain the product and contribution route accurately. Separate available behaviour from product goals. |

## How work enters the roadmap

External users submit issues, not PRs. Sourceful implements selected work.
Acceptance of an issue does not invite an external implementation PR.

For each selected change, state the household need, the behaviour to change,
the existing work it touches and the evidence that will establish completion.
Keep details in the issue and PR. Do not create another feature inventory or
long-lived agent work plan beside this roadmap.

Decide exclusions as concrete needs arise. Existing proposals, including the
[tariff and demand discussion](https://github.com/srcfl/ftw/issues/866), remain
evidence to assess; they are neither blanket implementation bans nor delivery
promises. An older PR's title or approval is not proof it still fits current
code or direction. Coordinate changes with its author and preserve unique
work before any owner-authorized closure.
